package screen

import (
	"bytes"
	"encoding/binary"
	"errors"
	"path/filepath"
	"slices"
)

// ErrNoProcess reports that a PID does not currently name a live process.
var ErrNoProcess = errors.New("screen: no such process")

// ProcInfo is the identity of one live process. UID is the real user ID,
// so a setuid-root screen installation is still attributed to its user.
type ProcInfo struct {
	PID  int
	PPID int
	UID  int
	Argv []string
}

// ProcessTable reads the operating system's process table. Zombies are
// treated as exited and never returned.
type ProcessTable interface {
	Get(pid int) (ProcInfo, error)
	Children(ppid int) ([]ProcInfo, error)
}

// SystemProcesses reads the live process table: /proc on Linux, sysctl on
// macOS. Neither runs ps or any other external tool.
type SystemProcesses struct{}

// sameInvocation compares a running process's argv with the configured
// invocation. Arguments must match exactly; argv[0] only by base name,
// because a launcher (such as macOS's /usr/bin/java stub) may re-exec the
// same arguments from a different path.
func sameInvocation(running, want []string) bool {
	if len(running) != len(want) || len(want) == 0 {
		return false
	}
	return filepath.Base(running[0]) == filepath.Base(want[0]) &&
		slices.Equal(running[1:], want[1:])
}

// parseProcArgs decodes macOS kern.procargs2: a native-endian int32 argc,
// the executable path, NUL padding, then argc NUL-terminated arguments
// followed by the environment, which is ignored. It lives here so its tests
// run on every platform.
func parseProcArgs(raw []byte) ([]string, error) {
	if len(raw) < 4 {
		return nil, errors.New("screen: truncated process arguments")
	}
	argc := int(binary.NativeEndian.Uint32(raw))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return nil, errors.New("screen: truncated process arguments")
	}
	rest = bytes.TrimLeft(rest[end:], "\x00")
	argv := make([]string, 0, min(argc, len(rest)))
	for range argc {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, errors.New("screen: truncated process arguments")
		}
		argv = append(argv, string(rest[:end]))
		rest = rest[end+1:]
	}
	return argv, nil
}
