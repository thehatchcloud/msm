// Package process is a narrow, shell-free boundary for future runtime adapters.
package process

import (
	"bytes"
	"context"
	"os/exec"
)

type Command struct {
	Path string
	Args []string
	Dir  string
	Env  []string // nil inherits the parent environment, like os/exec.
}

type Result struct {
	Stdout string
	Stderr string
}

type Runner interface {
	Run(context.Context, Command) (Result, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command Command) (Result, error) {
	cmd := exec.CommandContext(ctx, command.Path, command.Args...)
	cmd.Dir, cmd.Env = command.Dir, command.Env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return Result{Stdout: stdout.String(), Stderr: stderr.String()}, err
}
