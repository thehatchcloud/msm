package servers

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/thehatchcloud/msm/internal/filelock"
)

// Renamed describes a completed rename.
type Renamed struct {
	From, To, Dir string
	// Links are the symbolic links in the server directory, such as world
	// links, retargeted from the old directory to the new one.
	Links []string
	// Notes list data the rename deliberately leaves under the old name.
	Notes []string
}

// Rename renames a stopped server. The directory moves with one rename(2);
// absolute symbolic links in the server directory that pointed into the old
// directory (the world links) are then retargeted. If retargeting fails,
// the links already changed are restored and the directory is moved back,
// so a failed rename leaves the server under its old name.
func (m *Manager) Rename(ctx context.Context, from, to string) (*Renamed, error) {
	if err := m.actAs(); err != nil {
		return nil, err
	}
	oldDir, err := m.existing(from)
	if err != nil {
		return nil, err
	}
	if from != to && strings.EqualFold(from, to) {
		return nil, fmt.Errorf("%w: renaming %q to %q changes only letter case, which cannot be done safely on a case-insensitive volume; rename it to another name first", ErrCaseCollision, from, to)
	}
	if err := m.available(to); err != nil {
		return nil, err
	}
	s, err := m.settings(from, oldDir)
	if err != nil {
		return nil, err
	}
	owner, err := m.owner(s)
	if err != nil {
		return nil, err
	}
	locks, err := m.lockExisting(oldDir, owner)
	if err != nil {
		return nil, err
	}
	defer filelock.ReleaseAll(locks)
	if err := m.available(to); err != nil {
		return nil, err
	}
	if err := requireStopped(from, m.state(ctx, s)); err != nil {
		return nil, err
	}
	if err := m.drop(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	newDir := filepath.Join(m.cfg.Root, to)
	links, err := linksInto(oldDir)
	if err != nil {
		return nil, err
	}
	if err := m.ops.Rename(oldDir, newDir); err != nil {
		return nil, fmt.Errorf("servers: rename %s to %s: %w; nothing was renamed", oldDir, newDir, err)
	}
	syncDir(m.cfg.Root)

	r := &Renamed{From: from, To: to, Dir: newDir}
	for i, l := range links {
		path := filepath.Join(newDir, l.name)
		if err := m.replaceLink(path, newDir+l.rest); err != nil {
			cause := fmt.Errorf("servers: retarget %s: %w", path, err)
			return nil, m.undoRename(cause, oldDir, newDir, links[:i])
		}
		r.Links = append(r.Links, path)
	}
	r.Notes = renameNotes(s.Get, oldDir)
	return r, nil
}

// link is a top-level symbolic link whose absolute target is inside the
// server directory: target == dir+rest.
type link struct {
	name, target, rest string
}

// linksInto finds the links a rename must retarget. The legacy manager
// creates world links in the server directory with absolute targets below
// it, so only top-level links are examined; a link that points elsewhere
// (such as server.jar into the JAR store) is left alone.
func linksInto(dir string) ([]link, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("servers: list %s: %w", dir, err)
	}
	prefixes := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		prefixes = append(prefixes, real)
	}
	var links []link
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("servers: read link %s: %w", e.Name(), err)
		}
		for _, p := range prefixes {
			if target == p || strings.HasPrefix(target, p+string(filepath.Separator)) {
				links = append(links, link{name: e.Name(), target: target, rest: target[len(p):]})
				break
			}
		}
	}
	return links, nil
}

// replaceLink atomically points the link at path to target.
func (m *Manager) replaceLink(path, target string) error {
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), ".msm-link-"+suffix)
	if err := m.ops.Symlink(target, tmp); err != nil {
		return err
	}
	if err := m.ops.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// undoRename restores the links already retargeted and moves the directory
// back. It reports whatever it could not undo, with the location of the
// server, instead of claiming the rename either succeeded or never happened.
func (m *Manager) undoRename(cause error, oldDir, newDir string, done []link) error {
	var failed []error
	for _, l := range done {
		if err := m.replaceLink(filepath.Join(newDir, l.name), l.target); err != nil {
			failed = append(failed, fmt.Errorf("restore link %s: %w", l.name, err))
		}
	}
	if err := m.ops.Rename(newDir, oldDir); err != nil {
		failed = append(failed, fmt.Errorf("move %s back to %s: %w", newDir, oldDir, err))
		return fmt.Errorf("%w; rollback failed, the server is now at %s and its world links may need repair: %w", cause, newDir, errors.Join(failed...))
	}
	syncDir(m.cfg.Root)
	if len(failed) > 0 {
		return fmt.Errorf("%w; the server is back at %s but some links could not be restored: %w", cause, oldDir, errors.Join(failed...))
	}
	return fmt.Errorf("%w; nothing was renamed", cause)
}

// renameNotes lists what stays under the old name, as in the legacy
// manager: archives are keyed by server name, and absolute per-server paths
// are the administrator's to change.
func renameNotes(get func(string) string, oldDir string) []string {
	notes := []string{"world backups, complete backups and archived logs made under the old name keep that name"}
	for _, setting := range []string{"WORLD_STORAGE_PATH", "WORLD_STORAGE_INACTIVE_PATH", "LOG_PATH", "JAR_PATH",
		"WHITELIST_PATH", "BANNED_PLAYERS_PATH", "BANNED_IPS_PATH", "OPS_PATH", "FLAG_ACTIVE_PATH"} {
		if v := get(setting); v == oldDir || strings.HasPrefix(v, oldDir+string(filepath.Separator)) {
			continue
		} else if strings.Contains(v, string(filepath.Separator)+filepath.Base(oldDir)+string(filepath.Separator)) {
			notes = append(notes, fmt.Sprintf("%s is the absolute path %s, which still names the old server; update it if it should move", setting, v))
		}
	}
	return notes
}
