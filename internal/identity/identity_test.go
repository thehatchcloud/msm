package identity

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
	"testing"
)

func TestCurrentMatchesOSUser(t *testing.T) {
	want, err := user.Current()
	if err != nil {
		t.Skipf("os/user.Current unavailable in this environment: %v", err)
	}
	got, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != want.Username {
		t.Fatalf("Username = %q, want %q", got.Username, want.Username)
	}
	wantUID, _ := strconv.Atoi(want.Uid)
	if got.UID != wantUID {
		t.Fatalf("UID = %d, want %d", got.UID, wantUID)
	}
}

func TestLookupUnknownUserFails(t *testing.T) {
	if _, err := Lookup("msm-does-not-exist-user"); err == nil {
		t.Fatal("expected an error looking up a nonexistent user")
	}
}

func TestDropToNoOpWhenAlreadyTheTargetUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this process is root; see TestDropToSubprocess for the root behavior")
	}
	current, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := DropTo(current); err != nil {
		t.Fatalf("DropTo(current user) = %v, want nil", err)
	}
}

func TestDropToFailsWithoutRootForADifferentUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this process is root; see TestDropToSubprocess for the root behavior")
	}
	current, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	other := Identity{Username: "someone-else", UID: current.UID + 1, GID: current.GID}
	err = DropTo(other)
	if !errors.Is(err, ErrPrivilegeRequired) {
		t.Fatalf("DropTo(other user) = %v, want ErrPrivilegeRequired", err)
	}
}

// dropSubprocessEnv marks the re-exec'd child that actually performs (and
// therefore, since it is irrevocable, must perform in a disposable process
// rather than this test binary's own) a root-to-unprivileged privilege
// drop, and then exercises DropTo's now-unprivileged behavior in the same
// process: a no-op for its own new identity, and a rejection for anything
// else.
const dropSubprocessEnv = "MSM_IDENTITY_TEST_DROP_SUBPROCESS"

// unprivilegedTestUsername is looked up by both the parent (to skip if the
// environment lacks it) and the child (to drop to it). "nobody" exists on
// every mainstream Linux and macOS installation.
const unprivilegedTestUsername = "nobody"

func TestDropToSubprocess(t *testing.T) {
	if os.Getenv(dropSubprocessEnv) == "1" {
		runDropSubprocessChild(t)
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires running as root")
	}
	if _, err := Lookup(unprivilegedTestUsername); err != nil {
		t.Skipf("no %q user available to drop to: %v", unprivilegedTestUsername, err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestDropToSubprocess$", "-test.v")
	cmd.Env = append(os.Environ(), dropSubprocessEnv+"=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subprocess failed: %v\n%s", err, output)
	}
	t.Logf("subprocess output:\n%s", output)
}

// runDropSubprocessChild runs inside the re-exec'd child process. It never
// returns control with root regained: DropTo is irrevocable, by design.
func runDropSubprocessChild(t *testing.T) {
	target, err := Lookup(unprivilegedTestUsername)
	if err != nil {
		t.Fatalf("look up %q: %v", unprivilegedTestUsername, err)
	}

	if err := DropTo(target); err != nil {
		t.Fatalf("DropTo(%q) as root = %v, want nil", unprivilegedTestUsername, err)
	}
	if os.Geteuid() != target.UID || os.Getegid() != target.GID {
		t.Fatalf("after DropTo, euid/egid = %d/%d, want %d/%d", os.Geteuid(), os.Getegid(), target.UID, target.GID)
	}
	fmt.Printf("dropped to uid=%d gid=%d\n", os.Geteuid(), os.Getegid())

	// The drop must be irrevocable: root is gone, so reasserting it must
	// fail rather than silently succeeding.
	if err := syscall.Setuid(0); err == nil {
		t.Fatal("re-acquiring uid 0 after DropTo succeeded; the privilege drop was not irrevocable")
	}

	// Now unprivileged: DropTo to the identity already running is a no-op.
	// (Deliberately not using Current() here: see its doc comment on why
	// it cannot be trusted to reflect a same-process raw-syscall drop.)
	if err := DropTo(target); err != nil {
		t.Fatalf("DropTo(target) while already running as target = %v, want nil", err)
	}

	// DropTo to a different user without root must be rejected, not
	// silently ignored.
	other := Identity{Username: "root", UID: 0, GID: 0}
	if err := DropTo(other); !errors.Is(err, ErrPrivilegeRequired) {
		t.Fatalf("DropTo(root) while unprivileged = %v, want ErrPrivilegeRequired", err)
	}
}
