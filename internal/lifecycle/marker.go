package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/thehatchcloud/msm/internal/identity"
)

// setActive records a server's intent in its FLAG_ACTIVE_PATH marker, as
// server_set_active does: the marker exists when the server should run.
//
// A marker directly inside the server directory (the default "active") is
// changed through a descriptor for that directory, opened without following
// a symbolic link, and is never itself followed, so a root caller cannot be
// redirected elsewhere by the server's owner. A new marker is handed to the
// owner. A marker configured anywhere else is changed only by a caller that
// is not root.
func setActive(root, name, marker string, owner identity.Identity, euid int, active bool) error {
	dir := filepath.Join(root, name)
	if filepath.Dir(marker) != dir {
		if euid == 0 {
			return fmt.Errorf("FLAG_ACTIVE_PATH %s is not directly inside %s; as root the manager changes only a marker there, so run as %s",
				marker, dir, owner.Username)
		}
		if active {
			f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("mark server %q active: %w", name, err)
			}
			return f.Close()
		}
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("mark server %q inactive: %w", name, err)
		}
		return nil
	}

	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", root, err)
	}
	defer unix.Close(rootFD)
	dirFD, err := unix.Openat(rootFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open server directory %s: %w", dir, err)
	}
	defer unix.Close(dirFD)
	base := filepath.Base(marker)
	if !active {
		err := unix.Unlinkat(dirFD, base, 0)
		if err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("mark server %q inactive: remove %s: %w", name, marker, err)
		}
		return nil
	}
	fd, err := unix.Openat(dirFD, base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if errors.Is(err, unix.EEXIST) {
		return nil // already active, whatever the marker is
	}
	if err != nil {
		return fmt.Errorf("mark server %q active: create %s: %w", name, marker, err)
	}
	defer unix.Close(fd)
	if euid == 0 {
		if err := unix.Fchown(fd, owner.UID, owner.GID); err != nil {
			return fmt.Errorf("mark server %q active: hand %s to %s: %w", name, marker, owner.Username, err)
		}
	}
	return nil
}

// isActive reports whether the server's marker exists, the same test
// servers.Manager.List uses.
func isActive(marker string) bool {
	_, err := os.Lstat(marker)
	return err == nil
}
