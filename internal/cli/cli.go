// Package cli implements the public command-line boundary.
// Command parsing, help, flags and completion are owned by Cobra.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/thehatchcloud/msm/internal/buildinfo"
)

const (
	ExitOK    = 0
	ExitError = 1
)

// Run is the process boundary: Cobra returns errors; only main exits.
// A fresh command tree keeps flags and Viper state isolated per invocation.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, info buildinfo.Info) int {
	return run(args, stdin, stdout, stderr, info, defaultDeps())
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, info buildinfo.Info, d deps) int {
	root, err := newRootCommand(info, d)
	if err == nil {
		root.SetIn(stdin)
		root.SetOut(stdout)
		root.SetErr(stderr)
		// A non-nil empty slice prevents Cobra from falling back to os.Args.
		root.SetArgs(append([]string{}, args...))
		// Ctrl+C or a termination request cancels the command's context: a
		// countdown aborts and tells the players; a wait stops waiting.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = root.ExecuteContext(ctx)
	}
	switch {
	case errors.Is(err, errNoSuchCommand):
		// The legacy line exactly, without the "msm: " error prefix.
		fmt.Fprintln(stderr, err)
		return ExitError
	case err != nil:
		fmt.Fprintf(stderr, "msm: %v\n", err)
		return ExitError
	}
	return ExitOK
}
