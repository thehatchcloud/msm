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
func Run(args []string, stdout, stderr io.Writer, info buildinfo.Info) int {
	root, err := NewRootCommand(info)
	if err == nil {
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
