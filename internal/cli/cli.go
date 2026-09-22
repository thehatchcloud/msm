// Package cli implements the public command-line boundary.
// Only help and version are implemented in the foundation milestone.
package cli

import (
	"fmt"
	"io"

	"github.com/thehatchcloud/msm/internal/buildinfo"
)

const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 65
)

const help = `Usage: msm <command>

Minecraft Server Manager: Go port foundation

Commands:
  help       Show this help
  version    Show the Go-port version and source commit

Server management is not implemented yet. This binary never invokes the
legacy Bash manager and is not a replacement for a production installation.
`

// Run consumes argv without shell evaluation. It does not read configuration,
// touch server files, or start any external processes.
func Run(args []string, stdout, stderr io.Writer, info buildinfo.Info) int {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")) {
		if _, err := io.WriteString(stdout, help); err != nil {
			fmt.Fprintf(stderr, "msm: writing help: %v\n", err)
			return ExitError
		}
		return ExitOK
	}
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		if _, err := fmt.Fprintln(stdout, info.String()); err != nil {
			fmt.Fprintf(stderr, "msm: writing version: %v\n", err)
			return ExitError
		}
		return ExitOK
	}
	fmt.Fprintln(stderr, "msm: unsupported command or arguments; this Go foundation implements only help and version")
	return ExitUsage
}
