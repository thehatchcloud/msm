// Package identity resolves the operating-system user that owns the
// manager and each server, and drops root privileges before a caller
// touches a server's files or, in a later task, executes its process. It
// never shells out to sudo and never relies on a setuid helper binary: the
// only way this package's caller gains privilege is by already running as
// root, and the only thing DropTo does with that privilege is give it up,
// irrevocably, before returning.
package identity

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// Identity is a resolved operating-system user.
type Identity struct {
	Username string
	UID      int
	GID      int
}

// Lookup resolves username to its identity.
func Lookup(username string) (Identity, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return Identity{}, fmt.Errorf("identity: look up user %q: %w", username, err)
	}
	return fromOSUser(u)
}

// Current resolves the identity the calling process is currently running
// as. Do not call Current to check the effect of a DropTo in the same
// process: os/user.Current caches its result for the life of the process
// and, when linked with cgo, can keep reporting a libc-cached identity
// after a raw setuid(2) syscall like DropTo's has already changed the
// kernel's. Compare os.Geteuid/os.Getegid directly instead.
func Current() (Identity, error) {
	u, err := user.Current()
	if err != nil {
		return Identity{}, fmt.Errorf("identity: look up current user: %w", err)
	}
	return fromOSUser(u)
}

func fromOSUser(u *user.User) (Identity, error) {
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return Identity{}, fmt.Errorf("identity: parse uid for %q: %w", u.Username, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return Identity{}, fmt.Errorf("identity: parse gid for %q: %w", u.Username, err)
	}
	return Identity{Username: u.Username, UID: uid, GID: gid}, nil
}

// ErrPrivilegeRequired is returned by DropTo when the calling process is
// not running as root and target is a different user than the one already
// running.
var ErrPrivilegeRequired = errors.New("identity: dropping to a different user requires starting as root")

// DropTo makes the calling process irrevocably become target: it clears
// supplementary groups to target's primary group, then sets the group ID,
// then the user ID, in that order, so the ability to change the group
// cannot be regained once the user ID is no longer 0. It never invokes
// sudo, su, or a setuid helper; the process must already be root.
//
// If the process is not running as root (effective UID 0), DropTo is a
// no-op when target is already the current user — the ordinary,
// unprivileged case this manager runs in most of the time — and otherwise
// fails with ErrPrivilegeRequired rather than silently continuing as the
// wrong user.
func DropTo(target Identity) error {
	if os.Geteuid() != 0 {
		// Compare against the kernel's own idea of the effective UID
		// (os.Geteuid, a raw syscall) rather than os/user.Current: after a
		// privilege drop performed with a raw syscall, as this function
		// does below, a cgo-linked os/user's libc-cached credentials can
		// still report the pre-drop identity even though the kernel has
		// already changed it.
		if os.Geteuid() == target.UID {
			return nil
		}
		return fmt.Errorf("%w: currently uid %d, wanted %q (uid %d)",
			ErrPrivilegeRequired, os.Geteuid(), target.Username, target.UID)
	}

	// The standard library's Setgroups/Setgid/Setuid apply to every OS
	// thread atomically on platforms (Linux since Go 1.16) where the
	// kernel would otherwise expose per-thread credentials; a raw syscall
	// issued by only one thread would leave the process in a mixed,
	// unsafe privilege state.
	if err := syscall.Setgroups([]int{target.GID}); err != nil {
		return fmt.Errorf("identity: drop supplementary groups for %q: %w", target.Username, err)
	}
	if err := syscall.Setgid(target.GID); err != nil {
		return fmt.Errorf("identity: set gid %d for %q: %w", target.GID, target.Username, err)
	}
	if err := syscall.Setuid(target.UID); err != nil {
		return fmt.Errorf("identity: set uid %d for %q: %w", target.UID, target.Username, err)
	}

	if os.Geteuid() != target.UID || os.Getegid() != target.GID {
		return fmt.Errorf("identity: privilege drop to %q did not take effect", target.Username)
	}
	return nil
}
