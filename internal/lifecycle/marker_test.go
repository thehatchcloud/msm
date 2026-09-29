package lifecycle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/identity"
)

func TestSetActive(t *testing.T) {
	me := identity.Identity{Username: "me", UID: os.Geteuid(), GID: os.Getegid()}
	for _, euid := range []int{me.UID, 0} {
		root := t.TempDir()
		dir := filepath.Join(root, "s")
		os.Mkdir(dir, 0o755)
		marker := filepath.Join(dir, "active")
		if err := setActive(root, "s", marker, me, euid, true); err != nil || !isActive(marker) {
			t.Fatalf("euid %d: activate: %v", euid, err)
		}
		if err := setActive(root, "s", marker, me, euid, true); err != nil {
			t.Fatalf("euid %d: activate twice: %v", euid, err)
		}
		if err := setActive(root, "s", marker, me, euid, false); err != nil || isActive(marker) {
			t.Fatalf("euid %d: deactivate: %v", euid, err)
		}
		if err := setActive(root, "s", marker, me, euid, false); err != nil {
			t.Fatalf("euid %d: deactivate twice: %v", euid, err)
		}

		// A marker that is a symbolic link is removed, never followed.
		outside := filepath.Join(t.TempDir(), "precious")
		os.WriteFile(outside, []byte("x"), 0o644)
		os.Symlink(outside, marker)
		if err := setActive(root, "s", marker, me, euid, true); err != nil {
			t.Fatal(err)
		}
		if err := setActive(root, "s", marker, me, euid, false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(outside); err != nil || isActive(marker) {
			t.Fatalf("euid %d: link target lost or marker kept: %v", euid, err)
		}

		// A server directory swapped for a link is refused.
		real := filepath.Join(root, "real")
		os.Rename(dir, real)
		os.Symlink(real, dir)
		if err := setActive(root, "s", marker, me, euid, true); err == nil {
			t.Fatalf("euid %d: followed a linked server directory", euid)
		}
	}

	// As root, a marker elsewhere is refused; otherwise it is honoured.
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "s"), 0o755)
	elsewhere := filepath.Join(t.TempDir(), "flags", "s-active")
	os.MkdirAll(filepath.Dir(elsewhere), 0o755)
	if err := setActive(root, "s", elsewhere, me, 0, true); err == nil || !strings.Contains(err.Error(), "not directly inside") {
		t.Fatalf("root with an outside marker: %v", err)
	}
	if err := setActive(root, "s", elsewhere, me, me.UID+1, true); err != nil || !isActive(elsewhere) {
		t.Fatalf("outside marker: %v", err)
	}
	if err := setActive(root, "s", elsewhere, me, me.UID+1, false); err != nil || isActive(elsewhere) {
		t.Fatalf("outside marker: %v", err)
	}
}
