package jars

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Selected is the result of choosing a server's JAR.
type Selected struct {
	Server, Link, JAR string
	// Download is set when the newest version was downloaded first.
	Download *Download
}

// Select points a server's JAR_PATH at a JAR in a group (msm <server> jar
// <group> [<file>]), as server_set_jar does.
//
// With a file, that file in the group is used; it may be a timestamped
// download or a JAR the administrator placed there by hand, and it must be
// a JAR (ZIP) file. Without one, the group's newest version is downloaded
// first and the newest timestamped JAR is used; if the download fails, the
// server keeps its current JAR.
//
// JAR_PATH is replaced only when it is missing or already a symbolic link,
// atomically; a regular file there (a JAR copied in by hand) is never
// overwritten. The change takes effect at the server's next start.
func (m *Manager) Select(ctx context.Context, server, group, file string) (Selected, error) {
	settings, err := m.cfg.Servers.Settings(server)
	if err != nil {
		return Selected{}, err
	}
	owner, err := m.cfg.Servers.Owner(settings)
	if err != nil {
		return Selected{}, err
	}
	if _, err := m.existing(group); err != nil {
		return Selected{}, fmt.Errorf("%w; there is no jargroup named %q", err, group)
	}
	if file != "" && (file == "." || file == ".." || strings.ContainsAny(file, "/\\") || strings.HasPrefix(file, stagePrefix)) {
		return Selected{}, fmt.Errorf("%w: %q is not a file name", ErrNoJAR, file)
	}
	if m.euid == 0 && file == "" && owner.UID != m.cfg.Owner.UID {
		return Selected{}, fmt.Errorf("%w: server %q belongs to %s, the jar store to %s; download with msm jargroup getlatest %s, then select the file by name",
			ErrOwner, server, owner.Username, m.cfg.Owner.Username, group)
	}

	registry, err := m.lock(false)
	if err != nil {
		return Selected{}, err
	}
	defer registry.Release()
	serverLock, settings, err := m.cfg.Servers.LockServer(server)
	if err != nil {
		return Selected{}, err
	}
	defer serverLock.Release()
	dir, err := m.existing(group)
	if err != nil {
		return Selected{}, err
	}
	link := settings.Get("JAR_PATH")
	if err := replaceable(link); err != nil {
		return Selected{}, fmt.Errorf("server %q: %w", server, err)
	}

	sel := Selected{Server: server, Link: link}
	if file == "" {
		// Downloads land in the store as its owner, and the link is made as
		// the same user, the server's owner (checked above for root).
		if err := m.drop(m.cfg.Owner); err != nil {
			return Selected{}, err
		}
		d, err := m.getLatest(ctx, dir)
		if err != nil {
			return Selected{}, fmt.Errorf("%w; server %q still uses its current JAR", err, server)
		}
		sel.Download = &d
		if sel.JAR, err = latestFile(dir); err != nil {
			return Selected{}, err
		}
		if sel.JAR == "" {
			return Selected{}, fmt.Errorf("%w: jar group %q has no downloaded JAR", ErrNoJAR, group)
		}
	} else {
		sel.JAR = filepath.Join(dir, file)
		info, err := os.Lstat(sel.JAR)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return Selected{}, fmt.Errorf("%w: there is no jar named %q in jargroup %q", ErrNoJAR, file, group)
		case err != nil:
			return Selected{}, fmt.Errorf("jars: inspect %s: %w", sel.JAR, err)
		case !info.Mode().IsRegular():
			return Selected{}, fmt.Errorf("%w: %s is not a regular file", ErrNoJAR, sel.JAR)
		}
		if err := checkJAR(sel.JAR); err != nil {
			return Selected{}, fmt.Errorf("%w: %s is not a JAR (ZIP) file", ErrInvalidJAR, sel.JAR)
		}
		if err := m.drop(owner); err != nil {
			return Selected{}, err
		}
	}
	if err := replaceLink(link, sel.JAR); err != nil {
		return Selected{}, fmt.Errorf("jars: point %s at %s: %w", link, sel.JAR, err)
	}
	return sel, nil
}

// replaceable allows replacing JAR_PATH only when it is missing or a link.
func replaceable(link string) error {
	info, err := os.Lstat(link)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if _, err := os.Stat(filepath.Dir(link)); err != nil {
			return fmt.Errorf("jars: JAR_PATH %s: %w", link, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("jars: inspect %s: %w", link, err)
	case info.Mode()&fs.ModeSymlink != 0:
		return nil
	default:
		return fmt.Errorf("%w: %s is a %s, not a link, so it is left alone; move it into a jar group's directory below JAR_STORAGE_PATH and select it by name",
			ErrNotLink, link, describeMode(info.Mode()))
	}
}

func describeMode(mode fs.FileMode) string {
	switch {
	case mode.IsRegular():
		return "regular file"
	case mode.IsDir():
		return "directory"
	default:
		return "special file"
	}
}
