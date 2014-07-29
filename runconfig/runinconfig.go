package runconfig

import "github.com/docker/docker/engine"

type RunInConfig struct {
	User         string
	Privileged   bool
	Tty          bool
	Container    string
	AttachStdin  bool
	AttachStderr bool
	AttachStdout bool
	Cmd          []string
}

func RunInConfigFromJob(job *engine.Job) *RunInConfig {
	runInConfig := &RunInConfig{
		User:         job.Getenv("User"),
		Privileged:   job.GetenvBool("Privileged"),
		Tty:          job.GetenvBool("Tty"),
		Container:    job.Getenv("Container"),
		AttachStdin:  job.GetenvBool("AttachStdin"),
		AttachStderr: job.GetenvBool("AttachStderr"),
		AttachStdout: job.GetenvBool("AttachStdout"),
	}
	if Cmd := job.GetenvList("Cmd"); Cmd != nil {
		runInConfig.Cmd = Cmd
	}

	return runInConfig
}
