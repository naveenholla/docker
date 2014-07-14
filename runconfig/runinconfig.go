package runconfig

import "github.com/dotcloud/docker/engine"

type RunInConfig struct {
	User       string
	Privileged bool
	Tty        bool
	Env        []string
	Cmd        []string
	Entrypoint []string
	Stdin      bool
	Stdout     bool
	Stderr     bool
	Stream     bool
	OpenStdin  bool // Open stdin
}

func RunInConfigFromJob(job *engine.Job) *RunInConfig {
	runInConfig := &RunInConfig{
		User:       job.Getenv("User"),
		Privileged: job.GetenvBool("Privileged"),
		Tty:        job.GetenvBool("Tty"),
		Stdin:      job.GetenvBool("AttachStdin"),
		Stdout:     job.GetenvBool("AttachStdout"),
		Stderr:     job.GetenvBool("AttachStderr"),
		Stream:     job.GetenvBool("stream"),
		OpenStdin:  job.GetenvBool("OpenStdin"),
	}
	if Env := job.GetenvList("Env"); Env != nil {
		runInConfig.Env = Env
	}
	if Cmd := job.GetenvList("Cmd"); Cmd != nil {
		runInConfig.Cmd = Cmd
	}
	if Entrypoint := job.GetenvList("Entrypoint"); Entrypoint != nil {
		runInConfig.Entrypoint = Entrypoint
	}

	return runInConfig
}
