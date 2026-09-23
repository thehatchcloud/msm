// Package process is a narrow, shell-free boundary for future runtime adapters.
package process

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"syscall"
)

type Command struct {
	Path string
	Args []string
	Dir  string
	Env  []string // nil inherits the parent environment, like os/exec.
	// Credential, when set, runs the child as another user. Only a root
	// caller can use it; supplementary groups are cleared, matching
	// identity.DropTo. nil runs the child as the caller.
	Credential *Credential
}

// Credential is the user and primary group a child process runs as.
type Credential struct {
	UID uint32
	GID uint32
}

type Result struct {
	Stdout string
	Stderr string
}

type Runner interface {
	Run(context.Context, Command) (Result, error)
}

// Stdio is the terminal an interactive child inherits.
type Stdio struct {
	In, Out, Err *os.File
}

// Terminal runs a child connected directly to the caller's terminal, such
// as a console attach, instead of capturing its output.
type Terminal interface {
	RunAttached(context.Context, Command, Stdio) error
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command Command) (Result, error) {
	cmd := newCmd(ctx, command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return Result{Stdout: stdout.String(), Stderr: stderr.String()}, err
}

func (ExecRunner) RunAttached(ctx context.Context, command Command, stdio Stdio) error {
	cmd := newCmd(ctx, command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio.In, stdio.Out, stdio.Err
	return cmd.Run()
}

func newCmd(ctx context.Context, command Command) *exec.Cmd {
	cmd := exec.CommandContext(ctx, command.Path, command.Args...)
	cmd.Dir, cmd.Env = command.Dir, command.Env
	if c := command.Credential; c != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: c.UID, Gid: c.GID, Groups: []uint32{},
		}}
	}
	return cmd
}
