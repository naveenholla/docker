// +build linux,cgo

package native

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/pkg/term"
	"github.com/docker/docker/utils"
	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/apparmor"
	"github.com/docker/libcontainer/cgroups/fs"
	"github.com/docker/libcontainer/cgroups/systemd"
	"github.com/docker/libcontainer/namespaces"
	"github.com/docker/libcontainer/syncpipe"
)

const (
	DriverName = "native"
	Version    = "0.2"
)

func init() {
	execdriver.RegisterInitFunc(DriverName, func(args *execdriver.InitArgs) error {
		var container *libcontainer.Config
		if args.Args[0] == "nsenter" {			
			if err := json.Unmarshal([]byte(args.ContainerJson), &container); err != nil {
				return fmt.Errorf("failed to unmarshall -containerjson: %s - %s", args.ContainerJson, err)
			}

			return namespaces.NsEnter(container, args.Args[2:])
		}

		f, err := os.Open(filepath.Join(args.Root, "container.json"))
		if err != nil {
			return err
		}

		if err := json.NewDecoder(f).Decode(&container); err != nil {
			f.Close()
			return err
		}
		f.Close()

		rootfs, err := os.Getwd()
		if err != nil {
			return err
		}

		syncPipe, err := syncpipe.NewSyncPipeFromFd(0, uintptr(args.Pipe))
		if err != nil {
			return err
		}

		if err := namespaces.Init(container, rootfs, args.Console, syncPipe, args.Args); err != nil {
			return err
		}

		return nil
	})
}

type activeContainer struct {
	container *libcontainer.Config
	cmd       *exec.Cmd
}

type driver struct {
	root             string
	initPath         string
	activeContainers map[string]*activeContainer
	sync.Mutex
}

func NewDriver(root, initPath string) (*driver, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}

	// native driver root is at docker_root/execdriver/native. Put apparmor at docker_root
	if err := apparmor.InstallDefaultProfile(); err != nil {
		return nil, err
	}
	return &driver{
		root:             root,
		initPath:         initPath,
		activeContainers: make(map[string]*activeContainer),
	}, nil
}

func (d *driver) Run(c *execdriver.Command, pipes *execdriver.Pipes, startCallback execdriver.StartCallback) (int, error) {
	// take the Command and populate the libcontainer.Config from it
	container, err := d.createContainer(c)
	if err != nil {
		return -1, err
	}

	var term execdriver.Terminal

	if c.ProcessConfig.Tty {
		term, err = NewTtyConsole(&c.ProcessConfig, pipes)
	} else {
		term, err = execdriver.NewStdConsole(&c.ProcessConfig, pipes)
	}
	c.ProcessConfig.Terminal = term

	d.Lock()
	d.activeContainers[c.ID] = &activeContainer{
		container: container,
		cmd:       &c.ProcessConfig.Cmd,
	}
	d.Unlock()

	var (
		dataPath = filepath.Join(d.root, c.ID)
		args     = append([]string{c.ProcessConfig.Entrypoint}, c.ProcessConfig.Arguments...)
	)

	if err := d.createContainerRoot(c.ID); err != nil {
		return -1, err
	}
	defer d.removeContainerRoot(c.ID)

	if err := d.writeContainerFile(container, c.ID); err != nil {
		return -1, err
	}

	return namespaces.Exec(container, c.ProcessConfig.Stdin, c.ProcessConfig.Stdout, c.ProcessConfig.Stderr, c.ProcessConfig.Console, c.Rootfs, dataPath, args, func(container *libcontainer.Config, console, rootfs, dataPath, init string, child *os.File, args []string) *exec.Cmd {
		// we need to join the rootfs because namespaces will setup the rootfs and chroot
		initPath := filepath.Join(c.Rootfs, c.InitPath)

		c.ProcessConfig.Path = d.initPath
		c.ProcessConfig.Args = append([]string{
			initPath,
			"-driver", DriverName,
			"-console", console,
			"-pipe", "3",
			"-root", filepath.Join(d.root, c.ID),
			"--",
		}, args...)

		// set this to nil so that when we set the clone flags anything else is reset
		c.ProcessConfig.SysProcAttr = nil
		system.SetCloneFlags(&c.ProcessConfig.Cmd, uintptr(namespaces.GetNamespaceFlags(container.Namespaces)))
		c.ProcessConfig.ExtraFiles = []*os.File{child}

		c.ProcessConfig.Env = container.Env
		c.ProcessConfig.Dir = c.Rootfs

		return &c.ProcessConfig.Cmd
	}, func() {
		if startCallback != nil {
			c.ProcessConfig.ContainerPid = c.ProcessConfig.Process.Pid
			startCallback(&c.ProcessConfig)
		}
	})
}

func (d *driver) Kill(p *execdriver.Command, sig int) error {
	return syscall.Kill(p.ProcessConfig.Process.Pid, syscall.Signal(sig))
}

func (d *driver) Pause(c *execdriver.Command) error {
	active := d.activeContainers[c.ID]
	if active == nil {
		return fmt.Errorf("active container for %s does not exist", c.ID)
	}
	active.container.Cgroups.Freezer = "FROZEN"
	if systemd.UseSystemd() {
		return systemd.Freeze(active.container.Cgroups, active.container.Cgroups.Freezer)
	}
	return fs.Freeze(active.container.Cgroups, active.container.Cgroups.Freezer)
}

func (d *driver) Unpause(c *execdriver.Command) error {
	active := d.activeContainers[c.ID]
	if active == nil {
		return fmt.Errorf("active container for %s does not exist", c.ID)
	}
	active.container.Cgroups.Freezer = "THAWED"
	if systemd.UseSystemd() {
		return systemd.Freeze(active.container.Cgroups, active.container.Cgroups.Freezer)
	}
	return fs.Freeze(active.container.Cgroups, active.container.Cgroups.Freezer)
}

func (d *driver) Terminate(p *execdriver.Command) error {
	// lets check the start time for the process
	state, err := libcontainer.GetState(filepath.Join(d.root, p.ID))
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// TODO: Remove this part for version 1.2.0
		// This is added only to ensure smooth upgrades from pre 1.1.0 to 1.1.0
		data, err := ioutil.ReadFile(filepath.Join(d.root, p.ID, "start"))
		if err != nil {
			// if we don't have the data on disk then we can assume the process is gone
			// because this is only removed after we know the process has stopped
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		state = &libcontainer.State{InitStartTime: string(data)}
	}

	currentStartTime, err := system.GetProcessStartTime(p.ProcessConfig.Process.Pid)
	if err != nil {
		return err
	}

	if state.InitStartTime == currentStartTime {
		err = syscall.Kill(p.ProcessConfig.Process.Pid, 9)
		syscall.Wait4(p.ProcessConfig.Process.Pid, nil, 0, nil)
	}
	d.removeContainerRoot(p.ID)

	return err

}

func (d *driver) Info(id string) execdriver.Info {
	return &info{
		ID:     id,
		driver: d,
	}
}

func (d *driver) Name() string {
	return fmt.Sprintf("%s-%s", DriverName, Version)
}

func (d *driver) GetPidsForContainer(id string) ([]int, error) {
	d.Lock()
	active := d.activeContainers[id]
	d.Unlock()

	if active == nil {
		return nil, fmt.Errorf("active container for %s does not exist", id)
	}
	c := active.container.Cgroups

	if systemd.UseSystemd() {
		return systemd.GetPids(c)
	}
	return fs.GetPids(c)
}

func (d *driver) writeContainerFile(container *libcontainer.Config, id string) error {
	data, err := json.Marshal(container)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filepath.Join(d.root, id, "container.json"), data, 0655)
}

func (d *driver) createContainerRoot(id string) error {
	return os.MkdirAll(filepath.Join(d.root, id), 0655)
}

func (d *driver) removeContainerRoot(id string) error {
	d.Lock()
	delete(d.activeContainers, id)
	d.Unlock()

	return os.RemoveAll(filepath.Join(d.root, id))
}

func getEnv(key string, env []string) string {
	for _, pair := range env {
		parts := strings.Split(pair, "=")
		if parts[0] == key {
			return parts[1]
		}
	}
	return ""
}

type TtyConsole struct {
	MasterPty *os.File
}

func NewTtyConsole(processConfig *execdriver.ProcessConfig, pipes *execdriver.Pipes) (*TtyConsole, error) {
	ptyMaster, console, err := system.CreateMasterAndConsole()
	if err != nil {
		return nil, err
	}

	tty := &TtyConsole{
		MasterPty: ptyMaster,
	}

	if err := tty.AttachPipes(&processConfig.Cmd, pipes); err != nil {
		tty.Close()
		return nil, err
	}

	processConfig.Console = console

	return tty, nil
}

func (t *TtyConsole) Master() *os.File {
	return t.MasterPty
}

func (t *TtyConsole) Resize(h, w int) error {
	return term.SetWinsize(t.MasterPty.Fd(), &term.Winsize{Height: uint16(h), Width: uint16(w)})
}

func (t *TtyConsole) AttachPipes(command *exec.Cmd, pipes *execdriver.Pipes) error {
	go func() {
		if wb, ok := pipes.Stdout.(interface {
			CloseWriters() error
		}); ok {
			defer wb.CloseWriters()
		}

		io.Copy(pipes.Stdout, t.MasterPty)
	}()

	if pipes.Stdin != nil {
		go func() {
			io.Copy(t.MasterPty, pipes.Stdin)

			pipes.Stdin.Close()
		}()
	}

	return nil
}

func (t *TtyConsole) Close() error {
	return t.MasterPty.Close()
}

func (d *driver) RunIn(c *execdriver.Command, processConfig *execdriver.ProcessConfig, pipes *execdriver.Pipes, startCallback execdriver.StartCallback) (int, error) {
	active := d.activeContainers[c.ID]
	if active == nil {
		return -1, fmt.Errorf("No active container exists with ID %s", c.ID)
	}
	state, err := libcontainer.GetState(filepath.Join(d.root, c.ID))
	if err != nil {
		return -1, fmt.Errorf("State unavailable for container with ID %s. The container may have been cleaned up already. Error: %s", c.ID, err)
	}

	var term execdriver.Terminal

	if processConfig.Tty {
		term, err = NewTtyConsole(processConfig, pipes)
	} else {
		term, err = execdriver.NewStdConsole(processConfig, pipes)
	}

	processConfig.Terminal = term

	args := append([]string{processConfig.Entrypoint}, processConfig.Arguments...)

	processConfig.Path = d.initPath

	nsenterCmd, err := namespaces.GetNsEnterCommand(strconv.Itoa(state.InitPid), active.container, processConfig.Console, args)
	if err != nil {
		return -1, fmt.Errorf("failed to get nsenter command - %s", err)
	}
	
	processConfig.Args = append([]string{
		c.InitPath,
		"--driver", DriverName }, nsenterCmd...) 

	processConfig.Dir = "/"

/*
	processConfig.Stdout = os.Stdout
	processConfig.Stderr = os.Stderr
	processConfig.Stdin = os.Stdin
*/
	utils.Debugf("About to run process %+v", processConfig)
	if err := processConfig.Start(); err != nil {
		return -1, err
	}
	if startCallback != nil {
		startCallback(processConfig)
	}

	if err := processConfig.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return -1, err
		}
	}

	return processConfig.ProcessState.Sys().(syscall.WaitStatus).ExitStatus(), nil
}
