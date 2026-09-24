// Package cli implements the public command-line boundary.
// Command parsing, help, flags and completion are owned by Cobra.
package cli

import (
	"fmt"
	"io"

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
		err = root.Execute()
	}
	if err != nil {
		fmt.Fprintf(stderr, "msm: %v\n", err)
		return ExitError
	}
	return ExitOK
}
