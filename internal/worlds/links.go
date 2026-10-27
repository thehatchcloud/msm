package worlds

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/serverprops"
)

const defaultRAMPath = "/dev/shm/msm"

// Skip is a world that was not linked, and why.
type Skip struct {
	Name, Reason string
}

// LinkReport says what EnsureLinks did.
type LinkReport struct {
	// Created are links made where there was none; Replaced are links
	// re-pointed; Removed are links of inactive worlds taken away.
	Created, Replaced, Removed []string
	Skipped                    []Skip
}

// Load reconciles the links from the server directory to its active worlds
// (msm <server> worlds load). The server must be stopped, since replacing a
// link under a running server would change the world it is using.
func (m *Manager) Load(ctx context.Context, name string, out Output) error {
	x, err := m.open(ctx, name)
	if err != nil {
		return err
	}
	defer x.release()
	rep, err := m.EnsureLinks(ctx, x.s, out)
	if err != nil {
		return err
	}
	if n := len(rep.Skipped); n > 0 {
		return fmt.Errorf("%w: %d of the worlds of %q were left unlinked (see above)", ErrLinks, n, name)
	}
	return nil
}

// EnsureLinks makes the symbolic links in the server directory match the
// worlds: an active world is linked under its own name to its directory (or
// to its RAM copy, when it is flagged for RAM, RAM worlds are usable and the
// copy was made or checked now), and the link of an inactive world is
// removed. It also creates the directory for the
// server's level-name in active storage, so a new world is generated there
// and not in the server directory.
//
// The caller holds the server's lock and has proven the server stopped. A
// name that is already a real file or directory is never replaced: that world
// is reported in Skipped and the rest are still linked. Links are changed
// through a descriptor for the server directory opened without following a
// symbolic link, so a root caller cannot be redirected by the server's
// owner; new links are handed to the owner.
func (m *Manager) EnsureLinks(ctx context.Context, s *legacyconf.ServerSettings, out Output) (*LinkReport, error) {
	owner, err := m.cfg.Servers.Owner(s)
	if err != nil {
		return nil, err
	}
	euid := m.cfg.Servers.EUID()
	if euid != 0 && euid != owner.UID {
		return nil, fmt.Errorf("%w: server %q belongs to %s (uid %d); run as that user or as root",
			identity.ErrPrivilegeRequired, s.Name, owner.Username, owner.UID)
	}
	l, err := m.resolve(s)
	if err != nil {
		return nil, err
	}
	dirFD, err := unix.Open(s.Dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("worlds: open server directory %s: %w", s.Dir, err)
	}
	defer unix.Close(dirFD)

	out.printf("Maintaining world symbolic links...")
	if err := m.ensureLevelDir(l, dirFD, owner, euid, out); err != nil {
		return nil, err
	}
	if err := l.discover(); err != nil {
		return nil, err
	}
	for _, w := range l.warnings {
		out.warnf("Warning: %s", w)
	}

	rep := &LinkReport{}
	var served ramServed
	for _, w := range l.worlds {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if l.ambiguousNames[w.Name] {
			if w.Active {
				rep.Skipped = append(rep.Skipped, Skip{w.Name, "it exists in both active and inactive storage"})
			}
			continue
		}
		var st unix.Stat_t
		statErr := unix.Fstatat(dirFD, w.Name, &st, unix.AT_SYMLINK_NOFOLLOW)
		missing := errors.Is(statErr, unix.ENOENT)
		if statErr != nil && !missing {
			return rep, fmt.Errorf("worlds: inspect %s: %w", filepath.Join(s.Dir, w.Name), statErr)
		}
		isLink := !missing && st.Mode&unix.S_IFMT == unix.S_IFLNK
		link := filepath.Join(s.Dir, w.Name)

		if !w.Active {
			if isLink {
				if err := m.ops.Unlinkat(dirFD, w.Name); err != nil && !errors.Is(err, unix.ENOENT) {
					return rep, fmt.Errorf("worlds: remove link %s: %w", link, err)
				}
				rep.Removed = append(rep.Removed, w.Name)
			}
			continue
		}
		if !missing && !isLink {
			rep.Skipped = append(rep.Skipped, Skip{w.Name, fmt.Sprintf("%s already exists and is not a symbolic link, so it was not replaced; remove it or rename the world", link)})
			continue
		}
		target := w.Dir
		if w.InRAM {
			target = m.ramTarget(ctx, &served, l, w, out)
		}
		if isLink {
			cur, err := readlinkat(dirFD, w.Name)
			if err != nil {
				return rep, fmt.Errorf("worlds: read link %s: %w", link, err)
			}
			if cur == target {
				continue
			}
		}
		if err := m.putLink(dirFD, w.Name, target, owner, euid); err != nil {
			return rep, fmt.Errorf("worlds: link %s: %w", link, err)
		}
		if isLink {
			rep.Replaced = append(rep.Replaced, w.Name)
		} else {
			rep.Created = append(rep.Created, w.Name)
		}
	}
	for _, sk := range rep.Skipped {
		out.warnf("    Error: Could not link world %q: %s", sk.Name, sk.Reason)
	}
	out.printf("Done.")
	return rep, nil
}

// ramRoot is RAMDISK_STORAGE_PATH when msm.conf enables RAM worlds, else "".
func (m *Manager) ramRoot() string {
	g := m.cfg.Servers.Global()
	if g == nil {
		return ""
	}
	if v, ok := g.Lookup("RAMDISK_STORAGE_ENABLED"); !ok || v != "true" {
		return ""
	}
	p := defaultRAMPath
	if v, ok := g.Lookup("RAMDISK_STORAGE_PATH"); ok {
		p = v
	}
	if !filepath.IsAbs(p) {
		return ""
	}
	return filepath.Clean(p)
}

// putLink creates the link name -> target in the directory dirFD, replacing
// an existing link atomically: the new link is made under a temporary name,
// handed to the owner, and renamed over the old one.
func (m *Manager) putLink(dirFD int, name, target string, owner identity.Identity, euid int) error {
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	tmp := stagePrefix + "link-" + suffix
	if err := m.ops.Symlinkat(target, dirFD, tmp); err != nil {
		return err
	}
	if euid == 0 && owner.UID != 0 {
		if err := unix.Fchownat(dirFD, tmp, owner.UID, owner.GID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = m.ops.Unlinkat(dirFD, tmp)
			return fmt.Errorf("hand link to %s: %w", owner.Username, err)
		}
	}
	if err := m.ops.Renameat(dirFD, tmp, name); err != nil {
		_ = m.ops.Unlinkat(dirFD, tmp)
		return err
	}
	return nil
}

func readlinkat(dirFD int, name string) (string, error) {
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(dirFD, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// ensureLevelDir creates active-storage/<level-name>, if the server's
// properties name a level, so the server writes a new world to storage
// through its link. Existing data is never disturbed: nothing is created
// when the level's directory is already in the server directory (a world the
// manager does not yet manage) or sits in inactive storage (deliberately
// switched off).
func (m *Manager) ensureLevelDir(l *layout, dirFD int, owner identity.Identity, euid int, out Output) error {
	s := l.s
	props, err := serverprops.Load(filepath.Join(s.Dir, m.cfg.Servers.PropertiesFile()))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("worlds: %w", err)
	}
	level, ok := props.Get("level-name")
	if !ok || level == "" {
		return nil
	}
	if !validWorldName(level) {
		out.warnf("Warning: level-name %q is not a usable world name, so no world directory was prepared for it", level)
		return nil
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirFD, level, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil && st.Mode&unix.S_IFMT != unix.S_IFLNK {
		out.warnf("Warning: %s is a real directory in the server directory, not in %s; move it there to manage it as a world", filepath.Join(s.Dir, level), l.active)
		return nil
	}
	if _, err := os.Lstat(filepath.Join(l.inact, level)); err == nil {
		return nil
	}
	if _, err := os.Lstat(filepath.Join(l.active, level)); err == nil {
		return nil
	}
	if filepath.Dir(l.active) == filepath.Clean(s.Dir) {
		return m.mkdirIn(dirFD, filepath.Base(l.active), level, owner, euid)
	}
	if euid == 0 && owner.UID != 0 {
		out.warnf("Warning: world storage %s is not directly inside the server directory, so as root the manager does not create %q in it; run as %s or create it yourself", l.active, level, owner.Username)
		return nil
	}
	if err := m.ops.MkdirAll(filepath.Join(l.active, level), 0o755); err != nil {
		return fmt.Errorf("worlds: create %s: %w", filepath.Join(l.active, level), err)
	}
	return nil
}

// mkdirIn creates storage/level below the server directory (descriptor
// dirFD) without following links, handing new directories to the owner.
func (m *Manager) mkdirIn(dirFD int, storage, level string, owner identity.Identity, euid int) error {
	if err := m.makeDir(dirFD, storage, owner, euid); err != nil {
		return err
	}
	sfd, err := unix.Openat(dirFD, storage, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("worlds: open %s: %w", storage, err)
	}
	defer unix.Close(sfd)
	return m.makeDir(sfd, level, owner, euid)
}

func (m *Manager) makeDir(dirFD int, name string, owner identity.Identity, euid int) error {
	err := m.ops.Mkdirat(dirFD, name, 0o755)
	if errors.Is(err, unix.EEXIST) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("worlds: create %s: %w", name, err)
	}
	if euid == 0 && owner.UID != 0 {
		if err := unix.Fchownat(dirFD, name, owner.UID, owner.GID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("worlds: hand %s to %s: %w", name, owner.Username, err)
		}
	}
	return nil
}

func randomSuffix() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("worlds: random name: %w", err)
	}
	return hex.EncodeToString(b), nil
}
