package worlds

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Activate moves an inactive world into active storage (msm <server> worlds
// on <world>). The server must be stopped. The world is not linked into the
// server directory until the next start or worlds load.
func (m *Manager) Activate(ctx context.Context, name, world string, out Output) error {
	return m.change(ctx, name, world, false, true, out)
}

// ActivateAll activates every inactive world of the server (msm all worlds
// on all, for each server).
func (m *Manager) ActivateAll(ctx context.Context, name string, out Output) error {
	return m.change(ctx, name, "", true, true, out)
}

// Deactivate moves an active world out of the server's live storage into
// inactive storage (msm <server> worlds off <world>): it is no longer linked
// into the server, and world-only backups skip it. The server must be
// stopped.
func (m *Manager) Deactivate(ctx context.Context, name, world string, out Output) error {
	return m.change(ctx, name, world, false, false, out)
}

// DeactivateAll deactivates every active world of the server (msm all worlds
// off all, for each server).
func (m *Manager) DeactivateAll(ctx context.Context, name string, out Output) error {
	return m.change(ctx, name, "", true, false, out)
}

func (m *Manager) change(ctx context.Context, name, world string, every, activate bool, out Output) error {
	x, err := m.open(ctx, name)
	if err != nil {
		return err
	}
	defer x.release()
	// Everything below changes the owner's files, so root becomes the owner
	// before the first change.
	if err := m.cfg.Servers.ActAsOwner(x.owner); err != nil {
		return err
	}
	l, err := m.load(x.s)
	if err != nil {
		return err
	}
	for _, w := range l.warnings {
		out.warnf("Warning: %s", w)
	}
	var todo []World
	if every {
		for _, w := range l.worlds {
			if w.Active != activate && !l.ambiguousNames[w.Name] {
				todo = append(todo, w)
			}
		}
		if len(todo) == 0 {
			out.printf("No worlds to %s.", map[bool]string{true: "activate", false: "deactivate"}[activate])
			return nil
		}
	} else {
		w, err := l.find(world)
		if err != nil {
			return err
		}
		if w.Active == activate {
			state := map[bool]string{true: "activate", false: "deactivate"}[activate]
			out.printf("World %q is already %s.", w.Name, state)
			return nil
		}
		todo = []World{w}
	}

	// Each world is moved on its own; one that cannot be does not stop the
	// rest, and the command fails if any did.
	var failed []string
	for _, w := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		if activate {
			err = m.activateOne(l, w, out)
		} else {
			err = m.deactivateOne(l, w, out)
		}
		if err != nil {
			if len(todo) == 1 {
				return err
			}
			out.warnf("Error: %v", err)
			failed = append(failed, w.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d worlds of %q could not be moved: %s", len(failed), len(todo), name, strings.Join(failed, ", "))
	}
	return nil
}

func (m *Manager) activateOne(l *layout, w World, out Output) error {
	fmt.Fprintf(out.Out, "Moving world %q to the active worldstorage directory... ", w.Name)
	if err := m.move(w.Dir, l.active, w.Name, out); err != nil {
		fmt.Fprintln(out.Out)
		return err
	}
	fmt.Fprintln(out.Out, "Done.")
	return nil
}

func (m *Manager) deactivateOne(l *layout, w World, out Output) error {
	if ram := m.ramCopy(l, w); ram != "" {
		return fmt.Errorf("%w: %q has a copy in RAM at %s that may hold newer data than %s; synchronize it to disk first", ErrRAMCopy, w.Name, ram, w.Dir)
	}
	fmt.Fprintf(out.Out, "Moving world %q to the inactive worldstorage directory... ", w.Name)
	if err := m.move(w.Dir, l.inact, w.Name, out); err != nil {
		fmt.Fprintln(out.Out)
		return err
	}
	// Take its link out of the server directory now, rather than leaving a
	// link to a directory that is gone until the next start.
	link := filepath.Join(l.s.Dir, w.Name)
	if info, err := os.Lstat(link); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		if target, err := os.Readlink(link); err == nil && target == w.Dir {
			if err := os.Remove(link); err != nil {
				fmt.Fprintln(out.Out)
				out.warnf("Warning: the world was moved, but its link %s could not be removed: %v", link, err)
				return nil
			}
		}
	}
	fmt.Fprintln(out.Out, "Done.")
	return nil
}

// ramCopy is the path of a world's RAM copy, if there is one.
func (m *Manager) ramCopy(l *layout, w World) string {
	root := m.ramRoot()
	if root == "" {
		return ""
	}
	p := filepath.Join(root, l.s.Name, w.Name)
	if info, err := os.Lstat(p); err == nil && info.IsDir() {
		return p
	}
	return ""
}

// move moves the world directory src to dstDir/name. It never replaces
// anything: a name that is already there is a conflict. On one filesystem it
// is a single rename. Across filesystems (the two storage directories may be
// separate disks) the world is copied to a hidden staging directory beside
// its destination, published with one rename, and only then taken out of
// service at the source, so an interruption leaves the world intact where it
// was; a failure after it was copied removes the copy again.
func (m *Manager) move(src, dstDir, name string, out Output) error {
	dst := filepath.Join(dstDir, name)
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%w: %s", ErrConflict, dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("worlds: inspect %s: %w", dst, err)
	}
	if err := m.ops.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("worlds: create %s: %w; the world was not moved", dstDir, err)
	}
	err := m.ops.Rename(src, dst)
	if err == nil {
		syncDir(dstDir)
		syncDir(filepath.Dir(src))
		return nil
	}
	if !isExdev(err) {
		return fmt.Errorf("worlds: move %s to %s: %w; the world was not moved", src, dst, err)
	}
	return m.copyMove(src, dstDir, name)
}

func (m *Manager) copyMove(src, dstDir, name string) error {
	dst := filepath.Join(dstDir, name)
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	stage := filepath.Join(dstDir, stagePrefix+name+"-"+suffix)
	if err := m.ops.CopyTree(src, stage); err != nil {
		_ = m.ops.RemoveAll(stage)
		return fmt.Errorf("worlds: copy %s to %s: %w; the world was not moved and is still at %s", src, dstDir, err, src)
	}
	if err := m.ops.Rename(stage, dst); err != nil {
		_ = m.ops.RemoveAll(stage)
		return fmt.Errorf("worlds: publish %s: %w; the world was not moved and is still at %s", dst, err, src)
	}
	syncDir(dstDir)
	// Take the original out of service in one step, so the world is never
	// in two places a server could use.
	trash := filepath.Join(filepath.Dir(src), stagePrefix+"old-"+name+"-"+suffix)
	if err := m.ops.Rename(src, trash); err != nil {
		if undo := m.ops.Rename(dst, stage); undo != nil {
			return fmt.Errorf("worlds: could not retire %s (%v) and could not undo the copy: the world is now in both %s and %s; keep %s, remove %s",
				src, err, src, dst, src, dst)
		}
		_ = m.ops.RemoveAll(stage)
		return fmt.Errorf("worlds: retire %s: %w; the world was not moved and is still at %s", src, err, src)
	}
	syncDir(filepath.Dir(src))
	if err := m.ops.RemoveAll(trash); err != nil {
		return fmt.Errorf("worlds: the world was moved to %s, but the old copy %s could not be removed: %w; remove it by hand", dst, trash, err)
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
