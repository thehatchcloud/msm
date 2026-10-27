package worlds

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func setTimes(dirfd int, name string, st *unix.Stat_t) error {
	ts := []unix.Timespec{st.Atim, st.Mtim}
	return unix.UtimesNanoAt(dirfd, name, ts, unix.AT_SYMLINK_NOFOLLOW)
}

// freeBytes is the space an unprivileged writer can still use on the
// filesystem holding path.
func freeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

const (
	tmpfsMagic = 0x01021994
	ramfsMagic = 0x858458f6
)

// checkRAMBacked proves path is on tmpfs. ramfs is refused: it has no size
// limit, so a full one exhausts the machine's memory instead of failing
// with ENOSPC.
func checkRAMBacked(path string, _ runProgram) error {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	switch int64(st.Type) {
	case tmpfsMagic:
		return nil
	case ramfsMagic:
		return fmt.Errorf("%s is on ramfs, which has no size limit; mount a size-limited tmpfs instead", path)
	}
	return fmt.Errorf("%s is not on a RAM-backed filesystem (tmpfs): it is a disk directory", path)
}

// exchange swaps two directories atomically. ok is false when the
// filesystem cannot.
func exchange(a, b string) (ok bool, err error) {
	err = unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
	switch err {
	case unix.EINVAL, unix.ENOSYS, unix.ENOTSUP:
		return false, nil
	}
	return true, err
}
