package filelock

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireBlocksUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan *Lock, 1)
	go func() {
		l, err := Acquire(path)
		if err != nil {
			t.Error(err)
			return
		}
		acquired <- l
	}()

	select {
	case <-acquired:
		t.Fatal("second Acquire returned before the first lock was released")
	case <-time.After(100 * time.Millisecond):
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	select {
	case second := <-acquired:
		if err := second.Release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire never completed after release")
	}
}

func TestReleaseTwiceReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err == nil {
		t.Fatal("expected an error releasing an already-released lock")
	}
}

func TestAcquireManyDedupesOverlappingPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.lock")
	locks, err := AcquireMany(path, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 {
		t.Fatalf("got %d locks, want 1 for a duplicated path", len(locks))
	}
	if err := ReleaseAll(locks); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireManyFixedOrderAvoidsDeadlock(t *testing.T) {
	dir := t.TempDir()
	serverLock := filepath.Join(dir, "server.lock")
	registryLock := filepath.Join(dir, "registry.lock")

	done := make(chan struct{}, 2)
	run := func(first, second string) {
		locks, err := AcquireMany(first, second)
		if err != nil {
			t.Error(err)
			done <- struct{}{}
			return
		}
		time.Sleep(20 * time.Millisecond)
		if err := ReleaseAll(locks); err != nil {
			t.Error(err)
		}
		done <- struct{}{}
	}

	// Both goroutines request the same two resources in opposite order.
	// AcquireMany must still take them in one fixed order internally, or
	// this test would deadlock and time out.
	go run(serverLock, registryLock)
	go run(registryLock, serverLock)

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("AcquireMany deadlocked on overlapping resources")
		}
	}
}

func TestAcquireManyReleasesAlreadyAcquiredLocksOnFailure(t *testing.T) {
	dir := t.TempDir()
	// "a-ok" sorts before the failing path so it is acquired first.
	ok := filepath.Join(dir, "a-ok.lock")
	// A path inside a missing directory always fails to open.
	failing := filepath.Join(dir, "missing-dir", "z-fails.lock")

	if _, err := AcquireMany(ok, failing); err == nil {
		t.Fatal("expected AcquireMany to fail")
	}

	// If the first lock was not released on failure, re-acquiring it would
	// block forever; bound the wait so the test fails instead of hanging.
	reacquired := make(chan *Lock, 1)
	go func() {
		l, err := Acquire(ok)
		if err != nil {
			t.Error(err)
			return
		}
		reacquired <- l
	}()
	select {
	case l := <-reacquired:
		if err := l.Release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lock acquired before a later failure was not released")
	}
}

func TestServerAndRegistryLockPaths(t *testing.T) {
	if got, want := ServerLockPath("/opt/msm/servers/example"), "/opt/msm/servers/example/.msm.lock"; got != want {
		t.Fatalf("ServerLockPath = %q, want %q", got, want)
	}
	if got, want := RegistryLockPath("/opt/msm/jars"), "/opt/msm/jars/.msm-registry.lock"; got != want {
		t.Fatalf("RegistryLockPath = %q, want %q", got, want)
	}
}
