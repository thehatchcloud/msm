// Package buildinfo holds reproducible build metadata, set by the linker.
package buildinfo

import (
	"fmt"
	"strings"
)

var (
	version = "go-port-dev"
	commit  = "unknown"
	// updateKeys are the comma-separated base64 ed25519 public keys whose
	// signatures the self-updater accepts, and updateURL is the https
	// directory its signed manifest is fetched from. Both are empty in a
	// development build, which therefore cannot update itself.
	updateKeys = ""
	updateURL  = ""
)

type Info struct {
	Version string
	Commit  string
	// UpdateKeys and UpdateURL are the build's trusted release signing keys
	// and release location (see internal/update).
	UpdateKeys []string
	UpdateURL  string
}

func Current() Info {
	info := Info{Version: version, Commit: commit, UpdateURL: updateURL}
	for _, k := range strings.Split(updateKeys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			info.UpdateKeys = append(info.UpdateKeys, k)
		}
	}
	return info
}

func (i Info) String() string {
	return fmt.Sprintf("Minecraft Server Manager (Go port) %s (commit %s)", i.Version, i.Commit)
}
