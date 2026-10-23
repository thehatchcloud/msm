package backup

import "golang.org/x/sys/unix"

// renameNoReplace renames oldpath to newpath and fails with EEXIST rather
// than replacing something already there. ok is false when the filesystem
// cannot do that.
func renameNoReplace(oldpath, newpath string) (ok bool, err error) {
	err = unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	switch err {
	case unix.EINVAL, unix.ENOSYS, unix.ENOTSUP:
		return false, nil
	}
	return true, err
}
