// Package buildinfo holds reproducible build metadata, set by the linker.
package buildinfo

import "fmt"

var (
	version = "go-port-dev"
	commit  = "unknown"
)

type Info struct {
	Version string
	Commit  string
}

func Current() Info { return Info{Version: version, Commit: commit} }

func (i Info) String() string {
	return fmt.Sprintf("Minecraft Server Manager (Go port) %s (commit %s)", i.Version, i.Commit)
}
