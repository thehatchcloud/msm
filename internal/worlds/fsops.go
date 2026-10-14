package worlds

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// fsOps is every filesystem mutation this package makes. Production code
// uses osOps; tests substitute a wrapper that fails a chosen call, to prove
// that an interruption at any step leaves the world where it was, or in a
// place the error names, never half-moved and reported as done.
//
// The *at calls act on a directory descriptor opened without following a
// symbolic link, so a privileged caller changing links in a server
// directory cannot be redirected by the directory's owner.
type fsOps interface {
	MkdirAll(path string, perm fs.FileMode) error
	Rename(from, to string) error
	RemoveAll(path string) error
	// CopyTree copies the tree at src to the new path dst, keeping symbolic
	// links as links and refusing anything but files, directories and links.
	CopyTree(src, dst string) error
	Mkdirat(dirfd int, name string, mode uint32) error
	Symlinkat(target string, dirfd int, name string) error
	Renameat(dirfd int, from, to string) error
	Unlinkat(dirfd int, name string) error
}

type osOps struct{}

func (osOps) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }
func (osOps) Rename(from, to string) error                 { return os.Rename(from, to) }

// RemoveAll never follows a symbolic link: a link inside the tree is
// unlinked, and whatever it points at is left alone.
func (osOps) RemoveAll(path string) error { return os.RemoveAll(path) }

func (osOps) Mkdirat(dirfd int, name string, mode uint32) error {
	return unix.Mkdirat(dirfd, name, mode)
}

func (osOps) Symlinkat(target string, dirfd int, name string) error {
	return unix.Symlinkat(target, dirfd, name)
}

func (osOps) Renameat(dirfd int, from, to string) error {
	return unix.Renameat(dirfd, from, dirfd, to)
}

func (osOps) Unlinkat(dirfd int, name string) error { return unix.Unlinkat(dirfd, name, 0) }

func (osOps) CopyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	return copyDir(src, dst, info)
}

func copyDir(src, dst string, info fs.FileInfo) error {
	if err := os.Mkdir(dst, info.Mode().Perm()|0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		from, to := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		fi, err := os.Lstat(from)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			if err := copyDir(from, to, fi); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			if err := copyFile(from, to, fi); err != nil {
				return err
			}
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(from)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, to); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is a %s, which cannot be copied", from, fileKind(fi.Mode()))
		}
	}
	// Directory permissions last, so a read-only directory can be filled.
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	return nil
}

func copyFile(src, dst string, info fs.FileInfo) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm()|0o200)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	return nil
}

func fileKind(m fs.FileMode) string {
	switch {
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "device"
	}
	return "special file"
}

func isExdev(err error) bool { return errors.Is(err, unix.EXDEV) }
