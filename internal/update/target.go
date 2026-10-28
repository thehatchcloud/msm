package update

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	// ErrPackageManaged: the executable belongs to a package manager, which
	// must update it.
	ErrPackageManaged = errors.New("executable is managed by a package manager")
	// ErrNotReplaceable: the executable is not a regular file we can replace.
	ErrNotReplaceable = errors.New("executable cannot be replaced")
	// ErrPermission: the invoking user may not replace the executable.
	ErrPermission = errors.New("not permitted to replace the executable")
	// ErrBusy: another update holds the lock.
	ErrBusy = errors.New("another update is running")
	// ErrNoRollback: there is no previous version to return to.
	ErrNoRollback = errors.New("there is no previous version to roll back to")
)

// packageDirs are directories whose files a package manager owns. Anything
// resolving into them is left alone: replacing it would make the package
// database lie about what is installed. A manually installed msm belongs in
// /usr/local/bin, /opt or the user's own bin directory.
var packageDirs = []string{
	"/usr/bin/", "/bin/", "/usr/sbin/", "/sbin/", "/usr/lib/", "/usr/libexec/",
	"/snap/", "/var/lib/snapd/", "/var/lib/flatpak/", "/nix/store/",
	"/opt/homebrew/Cellar/", "/usr/local/Cellar/", "/home/linuxbrew/.linuxbrew/Cellar/",
	"/opt/local/bin/", "/opt/pkg/",
}

// Target is the executable an update replaces.
type Target struct {
	// Path is the real file, with every symlink resolved; Dir is its
	// directory, where staging happens so the final rename is atomic.
	Path string
	Dir  string
	Mode fs.FileMode
	UID  int
	GID  int
}

// ResolveTarget follows symlinks from exe to the real file and checks that it
// may be replaced. A symlink such as /usr/local/bin/msm -> /opt/msm/bin/msm
// stays a symlink; the file it names is what is replaced.
func ResolveTarget(exe string) (*Target, error) {
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve %s: %v", ErrNotReplaceable, exe, err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return nil, err
	}
	for _, p := range packageDirs {
		if strings.HasPrefix(real, p) {
			return nil, fmt.Errorf("%w: %s is under %s; update it with that package manager, or install msm somewhere like /usr/local/bin first", ErrPackageManaged, real, strings.TrimSuffix(p, "/"))
		}
	}
	info, err := os.Lstat(real)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotReplaceable, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrNotReplaceable, real)
	}
	t := &Target{Path: real, Dir: filepath.Dir(real), Mode: info.Mode().Perm()}
	t.UID, t.GID = owner(info.Sys())
	if err := unix.Access(t.Dir, unix.W_OK|unix.X_OK); err != nil {
		return nil, fmt.Errorf("%w: cannot write to %s (%v); run the update as the user that owns it, for example with sudo", ErrPermission, t.Dir, err)
	}
	return t, nil
}

// PreviousPath is where the version before the last update is kept.
func (t *Target) PreviousPath() string { return t.Path + ".previous" }

func permissionOrErr(err error, dir string) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %v; run the update as the user that owns %s", ErrPermission, err, dir)
	}
	return err
}
