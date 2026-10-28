package update

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/thehatchcloud/msm/internal/filelock"
)

const (
	stagePrefix = ".msm-update-"
	lockName    = ".msm-update.lock"
)

// hooks lets tests stop the sequence between steps to prove that every
// interruption leaves a working program. It is nil in production.
type hooks func(step string) error

func (h hooks) at(step string) error {
	if h == nil {
		return nil
	}
	return h(step)
}

// lock serialises updates of one executable and removes what an interrupted
// run left behind. Holding the lock proves no other update owns a staging
// file, so every file with the staging prefix is stale.
func (t *Target) lock() (release func(), err error) {
	l, err := filelock.TryAcquire(filepath.Join(t.Dir, lockName))
	if err != nil {
		if os.IsPermission(err) {
			return nil, permissionOrErr(err, t.Dir)
		}
		return nil, fmt.Errorf("%w: %v", ErrBusy, err)
	}
	if stale, _ := filepath.Glob(filepath.Join(t.Dir, stagePrefix+"*")); len(stale) > 0 {
		for _, s := range stale {
			os.Remove(s)
		}
	}
	return func() {
		os.Remove(filepath.Join(t.Dir, lockName))
		l.Release()
	}, nil
}

// stage writes the new program next to the old one, on the same filesystem,
// with the old one's permission bits and, when running as root, its owner.
func (t *Target) stage(data []byte) (string, error) {
	f, err := os.CreateTemp(t.Dir, stagePrefix+"bin-*")
	if err != nil {
		return "", permissionOrErr(err, t.Dir)
	}
	path := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	mode := t.Mode | 0o500 // the owner must be able to run it
	if err := f.Chmod(mode); err != nil {
		return "", err
	}
	if os.Geteuid() == 0 && t.UID >= 0 {
		if err := f.Chown(t.UID, t.GID); err != nil {
			return "", err
		}
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

// keepPrevious preserves the running program as <path>.previous without ever
// leaving the name half written: a hard link when the filesystem has them
// (no data copied, and the old inode survives the replacement), a copy
// otherwise, each renamed into place.
func (t *Target) keepPrevious() error {
	tmp := filepath.Join(t.Dir, stagePrefix+fmt.Sprintf("prev-%d", os.Getpid()))
	os.Remove(tmp)
	if err := os.Link(t.Path, tmp); err != nil {
		if err := copyFile(t.Path, tmp, t.Mode|0o500); err != nil {
			os.Remove(tmp)
			return permissionOrErr(err, t.Dir)
		}
	}
	if err := os.Rename(tmp, t.PreviousPath()); err != nil {
		os.Remove(tmp)
		return permissionOrErr(err, t.Dir)
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// replace is the whole replacement sequence. The program at t.Path is the old
// one until the final rename, and the new one after it; there is no moment at
// which it is missing or partial. check is run on the staged file before the
// old program is touched.
func (t *Target) replace(data []byte, check func(staged string) error, h hooks) error {
	release, err := t.lock()
	if err != nil {
		return err
	}
	defer release()
	staged, err := t.stage(data)
	if err != nil {
		return err
	}
	defer os.Remove(staged) // a no-op after the rename
	if err := h.at("staged"); err != nil {
		return err
	}
	if check != nil {
		if err := check(staged); err != nil {
			return err
		}
	}
	if err := h.at("checked"); err != nil {
		return err
	}
	if err := t.keepPrevious(); err != nil {
		return err
	}
	if err := h.at("previous-kept"); err != nil {
		return err
	}
	if err := os.Rename(staged, t.Path); err != nil {
		return permissionOrErr(err, t.Dir)
	}
	syncDir(t.Dir)
	return nil
}

// Rollback puts the program kept by the last update back, and keeps the one
// it replaces as the new previous version so the rollback can be undone.
func (t *Target) Rollback() (err error) {
	info, err := os.Lstat(t.PreviousPath())
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w (%s)", ErrNoRollback, t.PreviousPath())
	}
	release, err := t.lock()
	if err != nil {
		return err
	}
	defer release()
	cur := filepath.Join(t.Dir, stagePrefix+"cur")
	if err := os.Link(t.Path, cur); err != nil {
		if err := copyFile(t.Path, cur, t.Mode|0o500); err != nil {
			return permissionOrErr(err, t.Dir)
		}
	}
	defer os.Remove(cur)
	if err := os.Rename(t.PreviousPath(), t.Path); err != nil {
		return permissionOrErr(err, t.Dir)
	}
	if err := os.Rename(cur, t.PreviousPath()); err != nil {
		return err
	}
	syncDir(t.Dir)
	return nil
}
