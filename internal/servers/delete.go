package servers

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thehatchcloud/msm/internal/filelock"
)

// ErrCleanup reports a delete that removed the server from service but
// could not remove all of its files.
var ErrCleanup = errors.New("servers: deleted server was not fully removed")

// DeletePlan is what Delete will remove, for the caller to show before
// asking for confirmation.
type DeletePlan struct {
	Name, Dir string
	// Entries are the names directly inside Dir.
	Entries []string
	// Files and Bytes count the regular files below Dir.
	Files int
	Bytes int64
	// ExternalLinks are symbolic links below Dir whose targets lie outside
	// it, as "link -> target". Delete removes the links, never the targets.
	ExternalLinks []string
	State         State

	info fs.FileInfo
}

// PlanDelete inspects a server for deletion. It never follows a symbolic
// link.
func (m *Manager) PlanDelete(ctx context.Context, name string) (*DeletePlan, error) {
	dir, err := m.existing(name)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("servers: inspect %s: %w", dir, err)
	}
	s, err := m.settings(name, dir)
	if err != nil {
		return nil, err
	}
	p := &DeletePlan{Name: name, Dir: dir, info: info, State: m.state(ctx, s)}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == filelock.ServerLockPath(dir) {
			return nil
		}
		if filepath.Dir(path) == dir {
			p.Entries = append(p.Entries, d.Name())
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			abs := target
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(filepath.Dir(path), abs)
			}
			if abs = filepath.Clean(abs); abs != dir && !strings.HasPrefix(abs, dir+string(filepath.Separator)) {
				p.ExternalLinks = append(p.ExternalLinks, path+" -> "+target)
			}
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			p.Files++
			p.Bytes += fi.Size()
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("servers: inspect %s: %w", dir, err)
	}
	sort.Strings(p.Entries)
	return p, nil
}

// Delete removes a stopped server previously inspected by PlanDelete. It
// refuses if the directory was replaced since the plan was made. The
// directory is first renamed out of the server namespace in one step, then
// removed without following symbolic links. If removal fails, the server
// is already gone from the listing and ErrCleanup names the leftover
// directory, which List keeps reporting.
func (m *Manager) Delete(ctx context.Context, p *DeletePlan) error {
	if err := m.actAs(); err != nil {
		return err
	}
	s, err := m.settings(p.Name, p.Dir)
	if err != nil {
		return err
	}
	owner, err := m.owner(s)
	if err != nil {
		return err
	}
	locks, err := m.lockExisting(p.Dir, owner)
	if err != nil {
		return err
	}
	defer filelock.ReleaseAll(locks)
	if info, err := os.Lstat(p.Dir); err != nil || !os.SameFile(info, p.info) {
		return fmt.Errorf("%w: %s", ErrChanged, p.Dir)
	}
	if err := requireStopped(p.Name, m.state(ctx, s)); err != nil {
		return err
	}
	if err := m.drop(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	trash := filepath.Join(m.cfg.Root, trashPrefix+p.Name+"-"+suffix)
	if err := m.ops.Rename(p.Dir, trash); err != nil {
		return fmt.Errorf("servers: remove %s from service: %w; nothing was deleted", p.Dir, err)
	}
	syncDir(m.cfg.Root)
	if err := m.ops.RemoveAll(trash); err != nil {
		return fmt.Errorf("%w: %q is deleted, but removing its files from %s stopped: %v; remove that directory by hand", ErrCleanup, p.Name, trash, err)
	}
	return nil
}
