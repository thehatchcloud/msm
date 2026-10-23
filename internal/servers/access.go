package servers

import (
	"context"
	"fmt"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
)

// Global is the imported msm.conf, or nil when there is none. Other
// managers read the settings they own from it.
func (m *Manager) Global() *legacyconf.File { return m.cfg.Global }

// EUID is the effective user ID the manager acts with.
func (m *Manager) EUID() int { return m.euid }

// Probe observes a server's process: Stopped only when the manager has
// proven no live session holds the server's screen name.
func (m *Manager) Probe(ctx context.Context, s *legacyconf.ServerSettings) State {
	return m.state(ctx, s)
}

// RequireStopped refuses a server whose state is not Stopped: running,
// occupied and unknown all refuse, since each could be a live server.
func RequireStopped(name string, st State) error { return requireStopped(name, st) }

// ActAsOwner makes this process able to change files owned by owner, the
// OS user of the server being changed. Root drops to owner permanently,
// before its first filesystem change, so a privileged process never
// mutates a tree the owner can write. An unprivileged caller must already
// be owner. Because the drop is irrevocable, a root process that has
// dropped to one user refuses a server owned by another.
func (m *Manager) ActAsOwner(owner identity.Identity) error {
	if m.euid != 0 {
		if m.euid == owner.UID {
			return nil
		}
		return fmt.Errorf("%w: this server belongs to %s (uid %d); run as that user or as root",
			identity.ErrPrivilegeRequired, owner.Username, owner.UID)
	}
	if owner.UID == 0 {
		return nil
	}
	if m.dropped {
		if m.droppedTo == owner.UID {
			return nil
		}
		return fmt.Errorf("%w: this process already dropped root privileges to another user; run again for the server owned by %s",
			identity.ErrPrivilegeRequired, owner.Username)
	}
	if err := m.dropTo(owner); err != nil {
		return fmt.Errorf("servers: drop privileges to %s: %w", owner.Username, err)
	}
	m.dropped, m.droppedTo = true, owner.UID
	return nil
}
