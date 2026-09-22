// Package filelock provides cross-process advisory locks that serialize
// mutation of a server directory or the shared JAR/registry store. Locking
// several resources together always takes them in one fixed, path-sorted
// order, so two callers locking overlapping resource sets can never
// deadlock against each other.
package filelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

// Lock is a held advisory lock backing a single path. The zero value is not
// a held lock; obtain one from Acquire.
type Lock struct {
	path string
	file *os.File
}

// Acquire blocks until it holds an exclusive advisory lock on path. The lock
// file is created if missing, with no promise about its contents; callers
// must not read or write it as data storage. path's parent directory must
// already exist.
func Acquire(path string) (*Lock, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("filelock: resolve %s: %w", path, err)
	}
	file, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("filelock: open %s: %w", abs, err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		file.Close()
		return nil, fmt.Errorf("filelock: lock %s: %w", abs, err)
	}
	return &Lock{path: abs, file: file}, nil
}

// Path reports the absolute path backing the lock.
func (l *Lock) Path() string { return l.path }

// Release drops the lock and closes its underlying file handle. Calling
// Release more than once on the same Lock returns an error rather than
// panicking or silently succeeding.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return errors.New("filelock: release of an unheld lock")
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return fmt.Errorf("filelock: unlock %s: %w", l.path, unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("filelock: close %s: %w", l.path, closeErr)
	}
	return nil
}

// AcquireMany locks every path in paths, always in the same fixed order
// (the lexicographic order of their resolved absolute paths) no matter what
// order the caller lists them in, and de-duplicates overlapping paths. If
// any acquisition fails, every lock already taken by this call is released
// before the error is returned.
func AcquireMany(paths ...string) ([]*Lock, error) {
	abs := make([]string, len(paths))
	for i, p := range paths {
		a, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("filelock: resolve %s: %w", p, err)
		}
		abs[i] = a
	}
	sort.Strings(abs)

	var ordered []string
	var prev string
	for i, p := range abs {
		if i == 0 || p != prev {
			ordered = append(ordered, p)
			prev = p
		}
	}

	locks := make([]*Lock, 0, len(ordered))
	for _, p := range ordered {
		l, err := Acquire(p)
		if err != nil {
			ReleaseAll(locks)
			return nil, err
		}
		locks = append(locks, l)
	}
	return locks, nil
}

// ReleaseAll releases every lock in locks, in the reverse of their
// acquisition order, continuing past failures and returning the first
// error encountered, if any.
func ReleaseAll(locks []*Lock) error {
	var firstErr error
	for i := len(locks) - 1; i >= 0; i-- {
		if err := locks[i].Release(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ServerLockPath is the conventional lock file protecting a single server's
// directory tree.
func ServerLockPath(serverDir string) string {
	return filepath.Join(serverDir, ".msm.lock")
}

// RegistryLockPath is the conventional lock file protecting the shared JAR
// group store below JAR_STORAGE_PATH.
func RegistryLockPath(jarStorageRoot string) string {
	return filepath.Join(jarStorageRoot, ".msm-registry.lock")
}
