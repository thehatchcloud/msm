package screen

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const ioctlGetTermios = unix.TCGETS

func (SystemProcesses) Get(pid int) (ProcInfo, error) {
	dir := "/proc/" + strconv.Itoa(pid)
	ppid, err := parentPID(pid)
	if err != nil {
		return ProcInfo{}, err
	}
	uid, err := realUID(dir + "/status")
	if err != nil {
		return ProcInfo{}, fmt.Errorf("screen: read process %d owner: %w", pid, err)
	}
	cmdline, err := os.ReadFile(dir + "/cmdline")
	if err != nil {
		return ProcInfo{}, fmt.Errorf("screen: read process %d arguments: %w", pid, err)
	}
	var argv []string
	if len(cmdline) > 0 {
		argv = strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	}
	return ProcInfo{PID: pid, PPID: ppid, UID: uid, Argv: argv}, nil
}

// parentPID reads only /proc/<pid>/stat, which every user can read, so a
// scan for children does not depend on access to unrelated processes.
func parentPID(pid int) (int, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("%w: %d", ErrNoProcess, pid)
	}
	if err != nil {
		return 0, fmt.Errorf("screen: read process %d: %w", pid, err)
	}
	// The command name is parenthesized and may itself contain ") ", so
	// the state and parent PID follow the last closing parenthesis.
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	if len(fields) < 2 {
		return 0, fmt.Errorf("screen: malformed stat for process %d", pid)
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return 0, fmt.Errorf("%w: %d exited", ErrNoProcess, pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("screen: malformed stat for process %d", pid)
	}
	return ppid, nil
}

func realUID(path string) (int, error) {
	status, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(status), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if fields := strings.Fields(rest); len(fields) > 0 {
				return strconv.Atoi(fields[0])
			}
		}
	}
	return 0, errors.New("no Uid line")
}

func (p SystemProcesses) Children(ppid int) ([]ProcInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("screen: list processes: %w", err)
	}
	var children []ProcInfo
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		parent, err := parentPID(pid)
		if err != nil || parent != ppid {
			continue // exited while the table was being read, or unrelated
		}
		info, err := p.Get(pid)
		if errors.Is(err, ErrNoProcess) {
			continue
		}
		if err != nil {
			return nil, err
		}
		children = append(children, info)
	}
	return children, nil
}
