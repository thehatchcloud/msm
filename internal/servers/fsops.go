package servers

import (
	"io/fs"
	"os"

	"github.com/thehatchcloud/msm/internal/atomicfile"
)

// fsOps is every filesystem mutation this package makes. Production code
// uses osOps; tests substitute a wrapper that fails a chosen call, to prove
// that an interruption at any step leaves either the original instance or a
// recoverable staged directory, never a half-finished operation reported as
// complete.
type fsOps interface {
	Mkdir(path string, perm fs.FileMode) error
	WriteFile(path string, data []byte, perm fs.FileMode) error
	Rename(from, to string) error
	RemoveAll(path string) error
	Symlink(target, link string) error
}

type osOps struct{}

func (osOps) Mkdir(path string, perm fs.FileMode) error { return os.Mkdir(path, perm) }

func (osOps) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return atomicfile.Write(path, data, perm)
}

func (osOps) Rename(from, to string) error { return os.Rename(from, to) }

// RemoveAll never follows a symbolic link: a link inside the tree is
// unlinked, and whatever it points at is left alone.
func (osOps) RemoveAll(path string) error { return os.RemoveAll(path) }

func (osOps) Symlink(target, link string) error { return os.Symlink(target, link) }
