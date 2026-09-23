package screen

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const ioctlGetTermios = unix.TIOCGETA

// sZomb is XNU's SZOMB process state (sys/proc.h).
const sZomb = 5

func (SystemProcesses) Get(pid int) (ProcInfo, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ProcInfo{}, fmt.Errorf("screen: read process %d: %w", pid, err)
	}
	if int(kp.Proc.P_pid) != pid || kp.Proc.P_stat == sZomb {
		return ProcInfo{}, fmt.Errorf("%w: %d", ErrNoProcess, pid)
	}
	argv, err := procArgs(pid)
	if err != nil {
		return ProcInfo{}, err
	}
	return ProcInfo{PID: pid, PPID: int(kp.Eproc.Ppid), UID: int(kp.Eproc.Pcred.P_ruid), Argv: argv}, nil
}

func (p SystemProcesses) Children(ppid int) ([]ProcInfo, error) {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("screen: list processes: %w", err)
	}
	var children []ProcInfo
	for _, kp := range all {
		if int(kp.Eproc.Ppid) != ppid || kp.Proc.P_stat == sZomb {
			continue
		}
		info, err := p.Get(int(kp.Proc.P_pid))
		if errors.Is(err, ErrNoProcess) {
			continue // exited while the table was being read
		}
		if err != nil {
			return nil, err
		}
		children = append(children, info)
	}
	return children, nil
}

func procArgs(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ESRCH) {
			return nil, fmt.Errorf("%w: %d exited", ErrNoProcess, pid)
		}
		return nil, fmt.Errorf("screen: read process %d arguments: %w", pid, err)
	}
	return parseProcArgs(raw)
}
