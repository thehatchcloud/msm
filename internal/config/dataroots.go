package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/thehatchcloud/msm/internal/legacyconf"
)

// DataRoots is where servers and shared JARs live on disk.
type DataRoots struct {
	ServerStoragePath string
	JarStoragePath    string
}

// ResolveDataRoots decides ServerStoragePath and JarStoragePath without
// ever forcing an existing installation to move.
//
// An imported legacy configuration always wins outright, explicit setting
// or not: legacy already carries msm.conf's own documented defaults for
// whatever it does not set (see legacyconf.Global), so an existing
// root-owned installation keeps exactly the layout it already has.
//
// With no legacy configuration to import, running as root keeps the
// classic system-wide layout under /opt/msm, matching what msm.conf and
// every existing root-owned installation already expect. Running
// unprivileged instead defaults to a rootless, per-user data directory, so
// a non-root user can start using msm without first being granted write
// access to /opt/msm.
func ResolveDataRoots(legacy *legacyconf.GlobalSettings, euid int) (DataRoots, error) {
	if legacy != nil {
		return DataRoots{
			ServerStoragePath: legacy.ServerStoragePath,
			JarStoragePath:    legacy.JarStoragePath,
		}, nil
	}
	if euid == 0 {
		return DataRoots{
			ServerStoragePath: legacyconf.DefaultServerStoragePath,
			JarStoragePath:    legacyconf.DefaultJarStoragePath,
		}, nil
	}

	root, err := rootlessDataDir()
	if err != nil {
		return DataRoots{}, err
	}
	return DataRoots{
		ServerStoragePath: filepath.Join(root, "servers"),
		JarStoragePath:    filepath.Join(root, "jars"),
	}, nil
}

// rootlessDataDir resolves an unprivileged per-user data directory,
// following the same platform convention os.UserConfigDir uses for
// configuration: $XDG_DATA_HOME/msm on Linux (falling back to
// $HOME/.local/share/msm), and $HOME/Library/Application Support/msm on
// macOS. The standard library does not expose the equivalent "data
// directory" call directly, so this mirrors it by hand.
func rootlessDataDir() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "msm"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve a rootless data directory: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "msm"), nil
	}
	return filepath.Join(home, ".local", "share", "msm"), nil
}
