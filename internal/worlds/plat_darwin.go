package worlds

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"
)

func setTimes(dirfd int, name string, st *unix.Stat_t) error {
	ts := []unix.Timespec{st.Atim, st.Mtim}
	return unix.UtimesNanoAt(dirfd, name, ts, unix.AT_SYMLINK_NOFOLLOW)
}

func freeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// checkRAMBacked proves path is on a volume macOS created from memory
// (hdiutil attach -nomount ram://...). macOS has no tmpfs, so the volume's
// device is looked up in "hdiutil info", which lists RAM disks with an
// image path of ram://.
func checkRAMBacked(path string, run runProgram) error {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	dev := unix.ByteSliceToString(st.Mntfromname[:])
	out, err := run(context.Background(), "hdiutil", "info")
	if err != nil {
		return fmt.Errorf("cannot tell whether %s is RAM-backed: hdiutil info failed: %w", path, err)
	}
	if !ramDeviceListed(out, dev) {
		return fmt.Errorf("%s is on %s, which is not a RAM disk made with hdiutil (ram://): it is a disk directory", path, dev)
	}
	return nil
}

func exchange(a, b string) (ok bool, err error) {
	err = unix.RenamexNp(a, b, unix.RENAME_SWAP)
	switch err {
	case unix.EINVAL, unix.ENOSYS, unix.ENOTSUP:
		return false, nil
	}
	return true, err
}
