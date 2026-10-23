package servers

import (
	"errors"
	"testing"

	"github.com/thehatchcloud/msm/internal/identity"
)

func TestActAsOwner(t *testing.T) {
	other := identity.Identity{Username: "alice", UID: 4001, GID: 4001}
	third := identity.Identity{Username: "bob", UID: 4002, GID: 4002}

	t.Run("unprivileged owner", func(t *testing.T) {
		e := newEnv(t, "")
		m := e.manager(t, nil, e.owner.UID)
		if err := m.ActAsOwner(e.owner); err != nil {
			t.Error(err)
		}
		if len(e.dropped) != 0 {
			t.Errorf("dropped = %v", e.dropped)
		}
	})
	t.Run("unprivileged other user", func(t *testing.T) {
		e := newEnv(t, "")
		m := e.manager(t, nil, 1234)
		if err := m.ActAsOwner(other); !errors.Is(err, identity.ErrPrivilegeRequired) {
			t.Errorf("ActAsOwner = %v, want ErrPrivilegeRequired", err)
		}
	})
	t.Run("root drops once, to one user", func(t *testing.T) {
		e := newEnv(t, "")
		m := e.manager(t, nil, 0)
		if err := m.ActAsOwner(other); err != nil {
			t.Fatal(err)
		}
		if err := m.ActAsOwner(other); err != nil {
			t.Errorf("same owner again: %v", err)
		}
		if len(e.dropped) != 1 || e.dropped[0] != other {
			t.Errorf("dropped = %v, want one drop to alice", e.dropped)
		}
		if err := m.ActAsOwner(third); !errors.Is(err, identity.ErrPrivilegeRequired) {
			t.Errorf("another owner after the drop = %v, want ErrPrivilegeRequired", err)
		}
		// The storage-root owner's own drop is refused the same way.
		if err := m.drop(); e.owner.UID != 0 && !errors.Is(err, identity.ErrPrivilegeRequired) {
			t.Errorf("drop after dropping to another user = %v", err)
		}
	})
	t.Run("root owner needs no drop", func(t *testing.T) {
		e := newEnv(t, "")
		m := e.manager(t, nil, 0)
		if err := m.ActAsOwner(identity.Identity{Username: "root"}); err != nil {
			t.Error(err)
		}
		if len(e.dropped) != 0 {
			t.Errorf("dropped = %v", e.dropped)
		}
	})
	t.Run("drop failure", func(t *testing.T) {
		e := newEnv(t, "")
		m := e.manager(t, nil, 0)
		m.dropTo = func(identity.Identity) error { return errors.New("denied") }
		if err := m.ActAsOwner(other); err == nil {
			t.Error("a failed drop must fail")
		}
	})
}
