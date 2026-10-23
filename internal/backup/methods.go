package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// methods are the backup methods a world backup makes, from the manager's
// settings. Each enabled method is made for every active world, with the
// same save, staging and recovery steps; none is dropped because another is
// available.
type methods struct {
	zip, rdiff, rsync bool
	// Per-server directories of each method, and the configured roots
	// they are below.
	zipBase, rdiffBase, rsyncBase string
	roots                         []string
	rotation                      int
	nice                          int
	tool                          *rdiffTool
}

func (ms *methods) any() bool { return ms.zip || ms.rdiff || ms.rsync }

// stageBase is where a live backup stages its copy: beside the first
// enabled method's output.
func (ms *methods) stageBase() string {
	switch {
	case ms.zip:
		return ms.zipBase
	case ms.rsync:
		return ms.rsyncBase
	}
	return ms.rdiffBase
}

// bases are the directories of the enabled methods.
func (ms *methods) bases() []string {
	var out []string
	for _, b := range []struct {
		on  bool
		dir string
	}{{ms.zip, ms.zipBase}, {ms.rdiff, ms.rdiffBase}, {ms.rsync, ms.rsyncBase}} {
		if b.on {
			out = append(out, b.dir)
		}
	}
	return out
}

// flag reads a true/false setting. Anything else is an error: a setting
// that is silently read as "off" is a backup that silently does not happen.
func (m *Manager) flag(name, def string) (bool, error) {
	switch v := m.global(name, def); v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%w: %s is %q; it must be true or false", ErrSettings, name, v)
	}
}

func (m *Manager) integer(name, def string, lo, hi int) (int, error) {
	v := m.global(name, def)
	n, err := strconv.Atoi(v)
	if err != nil || strconv.Itoa(n) != strings.TrimPrefix(v, "+") || n < lo || n > hi {
		return 0, fmt.Errorf("%w: %s is %q; it must be a whole number from %d to %d", ErrSettings, name, v, lo, hi)
	}
	return n, nil
}

// worldMethods reads and checks the settings of a world backup, without
// touching the disk or the server.
func (m *Manager) worldMethods(name string) (*methods, error) {
	ms := &methods{}
	var err error
	if ms.zip, err = m.flag("WORLD_ARCHIVE_ENABLED", "true"); err != nil {
		return nil, err
	}
	if ms.rdiff, err = m.flag("RDIFF_BACKUP_ENABLED", "false"); err != nil {
		return nil, err
	}
	if ms.rsync, err = m.flag("RSYNC_BACKUP_ENABLED", "false"); err != nil {
		return nil, err
	}
	if !ms.any() {
		return nil, fmt.Errorf("%w: WORLD_ARCHIVE_ENABLED, RDIFF_BACKUP_ENABLED and RSYNC_BACKUP_ENABLED are all false, so no backup would be made", ErrDisabled)
	}
	parent := filepath.Dir(m.cfg.Servers.Root())
	if ms.zip {
		root, err := m.archiveRoot("WORLD_ARCHIVE_PATH", defaultWorldArchivePath, filepath.Join(parent, "archives", "worlds"))
		if err != nil {
			return nil, err
		}
		ms.zipBase = filepath.Join(root, name)
		ms.roots = append(ms.roots, root)
	}
	if ms.rdiff {
		root, err := m.archiveRoot("WORLD_RDIFF_PATH", defaultRdiffPath, filepath.Join(parent, "rdiff-backup", "worlds"))
		if err != nil {
			return nil, err
		}
		ms.rdiffBase = filepath.Join(root, name)
		ms.roots = append(ms.roots, root)
		if ms.rotation, err = m.integer("RDIFF_BACKUP_ROTATION", "7", 1, 36500); err != nil {
			return nil, err
		}
		if ms.nice, err = m.integer("RDIFF_BACKUP_NICE", "19", -20, 19); err != nil {
			return nil, err
		}
		if ms.nice < 0 && m.cfg.EUID() != 0 {
			return nil, fmt.Errorf("%w: RDIFF_BACKUP_NICE is %d, but only root can raise a priority and the backup runs as the server's owner; use 0 or more", ErrSettings, ms.nice)
		}
	}
	if ms.rsync {
		root, err := m.archiveRoot("WORLD_RSYNC_PATH", defaultRsyncPath, filepath.Join(parent, "rsync", "worlds"))
		if err != nil {
			return nil, err
		}
		ms.rsyncBase = filepath.Join(root, name)
		ms.roots = append(ms.roots, root)
	}
	for i := range ms.roots {
		for _, other := range ms.roots[i+1:] {
			if overlaps(ms.roots[i], other) {
				return nil, fmt.Errorf("%w: the backup directories %s and %s overlap; each method needs its own", ErrSettings, ms.roots[i], other)
			}
		}
	}
	return ms, nil
}

// overlaps is whether one path is the other or lies inside it, after
// resolving links in the parts that exist.
func overlaps(a, b string) bool {
	a, b = resolveExisting(a), resolveExisting(b)
	return within(a, b) || within(b, a) || a == b
}

// resolveExisting resolves links in the longest existing prefix of p.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// prepareTools checks the programs the enabled methods need and the
// repositories they will write to, before the server is touched.
func (m *Manager) prepareTools(ctx context.Context, ms *methods, units []unit) error {
	if !ms.rdiff {
		return nil
	}
	tool, err := m.findRdiff(ctx, ms.nice)
	if err != nil {
		return err
	}
	ms.tool = tool
	for _, u := range units {
		if overlaps(u.rdiffDir, u.root) {
			return fmt.Errorf("%w: the repository %s and the world %s overlap", ErrSettings, u.rdiffDir, u.root)
		}
		if err := m.checkRepository(ctx, tool, u.rdiffDir); err != nil {
			return err
		}
	}
	return nil
}

var snapshotPartial = regexp.MustCompile(`^` + regexp.QuoteMeta(tmpPrefix+snapshotTmpKind) + `[0-9a-f]{16}` + regexp.QuoteMeta(tmpSuffix) + `$`)

// restoreMeta gives a staged tree the modes and times of what it copies,
// for a program that records them. Directories come last, deepest first.
func restoreMeta(stageDir string, entries []entry) error {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		dst := filepath.Join(stageDir, filepath.FromSlash(e.Name))
		switch e.Kind {
		case kindLink:
			continue
		case kindFile:
			if err := os.Chmod(dst, e.Info.Mode().Perm()); err != nil {
				return fmt.Errorf("stage %s: %w", e.Src, err)
			}
		case kindDir:
			if err := os.Chmod(dst, e.Info.Mode().Perm()); err != nil {
				return fmt.Errorf("stage %s: %w", e.Src, err)
			}
			if err := os.Chtimes(dst, e.Info.ModTime(), e.Info.ModTime()); err != nil {
				return fmt.Errorf("stage %s: %w", e.Src, err)
			}
		}
	}
	return nil
}
