// Package servers implements server instance management: listing, creating,
// renaming and deleting the directories below SERVER_STORAGE_PATH, with the
// upstream layout and defaults.
//
// Every mutation holds the storage-root lock and, for an existing server,
// that server's lock; proves the server is stopped under those locks; and
// happens through a staged directory or a single rename(2), so an
// interruption leaves either the original instance or a recoverable staged
// directory that List reports. When invoked as root, the manager drops to
// the manager user (USERNAME) after locking and probing and before touching
// any file, so a privileged process never mutates paths inside a tree an
// unprivileged user can write to.
package servers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/safepath"
	"github.com/thehatchcloud/msm/internal/serverprops"
)

var (
	ErrInvalidName   = errors.New("servers: invalid server name")
	ErrExists        = errors.New("servers: a server with that name already exists")
	ErrCaseCollision = errors.New("servers: a server name differs only in case")
	ErrNotFound      = errors.New("servers: no such server")
	ErrNotDirectory  = errors.New("servers: not a server directory")
	ErrRunning       = errors.New("servers: server is running")
	ErrNotStopped    = errors.New("servers: server cannot be proven stopped")
	ErrChanged       = errors.New("servers: server changed since it was previewed")
)

const (
	// rootLockName serializes creating, renaming and deleting servers.
	rootLockName = ".msm-servers.lock"
	// Staged operations live directly in the storage root under these
	// prefixes. A server name cannot start with '.', so they never collide
	// with a server, and List reports any that an interruption left behind.
	stagePrefix = ".msm-create-"
	trashPrefix = ".msm-delete-"
)

// Config describes one storage root.
type Config struct {
	// Root is SERVER_STORAGE_PATH: an absolute, clean path.
	Root string
	// PropertiesFile is SERVER_PROPERTIES, the Minecraft properties file
	// name inside each server directory.
	PropertiesFile string
	// Global is the imported msm.conf, or nil when there is none.
	Global *legacyconf.File
	// Owner is the manager user (USERNAME) that owns the storage root and
	// every instance it creates.
	Owner identity.Identity
	// DefaultOwner, when set, owns any server whose USERNAME is the
	// built-in default rather than configured. A rootless installation
	// without msm.conf sets it to the invoking user.
	DefaultOwner *identity.Identity
	Prober       Prober
	// CreateRoot lets Create make a missing Root. Only a rootless default
	// sets it; a configured SERVER_STORAGE_PATH must already exist.
	CreateRoot bool

	// EUID is the effective user ID; zero means ask the OS.
	EUID *int
	// DropTo permanently drops a root process to the manager user; nil
	// means identity.DropTo. Tests replace it, since the real one is
	// irrevocable for the whole test binary.
	DropTo func(identity.Identity) error
}

// Manager manages the servers below one storage root.
type Manager struct {
	cfg     Config
	euid    int
	dropTo  func(identity.Identity) error
	ops     fsOps
	dropped bool
}

// New validates cfg and returns a Manager.
func New(cfg Config) (*Manager, error) {
	if err := safepath.ValidateConfiguredRoot(cfg.Root); err != nil {
		return nil, fmt.Errorf("servers: SERVER_STORAGE_PATH: %w", err)
	}
	if p := cfg.PropertiesFile; p == "" || p == "." || p == ".." || filepath.Base(p) != p {
		return nil, fmt.Errorf("servers: SERVER_PROPERTIES %q must be a plain file name", p)
	}
	if cfg.Prober == nil {
		return nil, errors.New("servers: a prober is required")
	}
	m := &Manager{cfg: cfg, euid: os.Geteuid(), dropTo: cfg.DropTo, ops: osOps{}}
	if cfg.EUID != nil {
		m.euid = *cfg.EUID
	}
	if m.dropTo == nil {
		m.dropTo = identity.DropTo
	}
	return m, nil
}

// Root is the storage root this manager manages.
func (m *Manager) Root() string { return m.cfg.Root }

// Entry is one server in a listing.
type Entry struct {
	Name   string
	Dir    string
	Active bool
	State  State
}

// List returns every server below the root, sorted by name, and warnings
// about entries that are not servers: leftover staged operations, symbolic
// links and directories whose names are not valid server names. A missing
// root has no servers.
func (m *Manager) List(ctx context.Context) ([]Entry, []string, error) {
	dirents, err := os.ReadDir(m.cfg.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("servers: list %s: %w", m.cfg.Root, err)
	}
	var entries []Entry
	var warnings []string
	for _, d := range dirents {
		name := d.Name()
		path := filepath.Join(m.cfg.Root, name)
		switch {
		case name == rootLockName:
			continue
		case strings.HasPrefix(name, stagePrefix):
			warnings = append(warnings, fmt.Sprintf("%s is left over from an interrupted server create; nothing uses it, so inspect and remove it", path))
			continue
		case strings.HasPrefix(name, trashPrefix):
			warnings = append(warnings, fmt.Sprintf("%s is a deleted server whose removal did not finish; remove it to reclaim the space", path))
			continue
		case d.Type()&fs.ModeSymlink != 0:
			warnings = append(warnings, fmt.Sprintf("%s is a symbolic link, not a server directory, and is ignored", path))
			continue
		case !d.IsDir():
			continue
		}
		if err := safepath.ValidateName(name); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s is ignored: %v", path, err))
			continue
		}
		s, err := m.settings(name, path)
		if err != nil {
			return nil, nil, err
		}
		warnings = append(warnings, s.Warnings...)
		_, statErr := os.Lstat(s.Get("FLAG_ACTIVE_PATH"))
		entries = append(entries, Entry{
			Name: name, Dir: path, Active: statErr == nil, State: m.state(ctx, s),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, warnings, nil
}

// Settings resolves an existing server's effective settings.
func (m *Manager) Settings(name string) (*legacyconf.ServerSettings, error) {
	dir, err := m.existing(name)
	if err != nil {
		return nil, err
	}
	return m.settings(name, dir)
}

func (m *Manager) settings(name, dir string) (*legacyconf.ServerSettings, error) {
	props, err := serverprops.Load(filepath.Join(dir, m.cfg.PropertiesFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("servers: %w", err)
	}
	return legacyconf.ResolveServer(legacyconf.ServerInput{
		Name: name, Dir: dir, Global: m.cfg.Global, Properties: props,
	})
}

// owner is the OS user a server's process runs as.
func (m *Manager) owner(s *legacyconf.ServerSettings) (identity.Identity, error) {
	if m.cfg.DefaultOwner != nil && s.Source("USERNAME") == legacyconf.FromDefault {
		return *m.cfg.DefaultOwner, nil
	}
	return s.Owner()
}

func (m *Manager) state(ctx context.Context, s *legacyconf.ServerSettings) State {
	owner, err := m.owner(s)
	if err != nil {
		return State{Kind: Unknown, Detail: err.Error()}
	}
	return m.cfg.Prober.Probe(ctx, s, owner)
}

// existing validates name and returns the directory of an existing server.
// A symbolic link or other non-directory is never treated as a server.
func (m *Manager) existing(name string) (string, error) {
	if err := safepath.ValidateName(name); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidName, err)
	}
	dir := filepath.Join(m.cfg.Root, name)
	// Match the name exactly against the directory listing: on a
	// case-insensitive volume (macOS by default) Lstat("OLD") would find
	// "old" and act on a server the caller did not name.
	exact, err := hasEntry(m.cfg.Root, name)
	if err != nil {
		return "", err
	}
	if !exact {
		if other, found, _ := safepath.CaseInsensitiveCollision(m.cfg.Root, name); found {
			return "", fmt.Errorf("%w %q (did you mean %q?)", ErrNotFound, name, other)
		}
		return "", fmt.Errorf("%w %q", ErrNotFound, name)
	}
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%w %q", ErrNotFound, name)
	case err != nil:
		return "", fmt.Errorf("servers: inspect %s: %w", dir, err)
	case info.Mode()&fs.ModeSymlink != 0:
		return "", fmt.Errorf("%w: %s is a symbolic link; only a real directory below %s is a managed server", ErrNotDirectory, dir, m.cfg.Root)
	case !info.IsDir():
		return "", fmt.Errorf("%w: %s is not a directory", ErrNotDirectory, dir)
	}
	return dir, nil
}

// available validates a new server name and checks that nothing below the
// root already has it, in any letter case.
func (m *Manager) available(name string) error {
	if err := safepath.ValidateName(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidName, err)
	}
	dir := filepath.Join(m.cfg.Root, name)
	// Check the exact name and case variants against the listing first, so
	// a case-insensitive volume reports the same error as a sensitive one.
	exact, err := hasEntry(m.cfg.Root, name)
	if err != nil {
		return err
	}
	if exact {
		return fmt.Errorf("%w: %s", ErrExists, dir)
	}
	other, found, err := safepath.CaseInsensitiveCollision(m.cfg.Root, name)
	if err != nil {
		return fmt.Errorf("servers: %w", err)
	}
	if found {
		return fmt.Errorf("%w: %q already exists, and %q would be the same directory on a case-insensitive volume", ErrCaseCollision, other, name)
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("servers: inspect %s: %w", dir, err)
	}
	return nil
}

// hasEntry reports whether dir lists an entry named exactly name. A
// missing dir has no entries.
func hasEntry(dir, name string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("servers: list %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.Name() == name {
			return true, nil
		}
	}
	return false, nil
}

// actAs checks that this process may mutate the storage root. Root may
// (and will drop to the manager user first); otherwise the caller must be
// the manager user, since files are never created as anyone else.
func (m *Manager) actAs() error {
	if m.euid == 0 || m.euid == m.cfg.Owner.UID {
		return nil
	}
	return fmt.Errorf("%w: servers below %s belong to %s (uid %d); run as that user or as root",
		identity.ErrPrivilegeRequired, m.cfg.Root, m.cfg.Owner.Username, m.cfg.Owner.UID)
}

// drop gives up root, once, before the first filesystem mutation.
func (m *Manager) drop() error {
	if m.euid != 0 || m.dropped || m.cfg.Owner.UID == 0 {
		return nil
	}
	if err := m.dropTo(m.cfg.Owner); err != nil {
		return fmt.Errorf("servers: drop privileges to %s: %w", m.cfg.Owner.Username, err)
	}
	m.dropped = true
	return nil
}

// lockFile prepares a lock file for owner. A root process creates it
// exclusively (so an existing symbolic link is never followed) and hands
// it to owner through the open descriptor, so the owner can lock it later
// without root. An existing file is used as is.
func (m *Manager) lockFile(path string, owner identity.Identity) error {
	if m.euid != 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("servers: create lock %s: %w", path, err)
	}
	chownErr := f.Chown(owner.UID, owner.GID)
	closeErr := f.Close()
	if chownErr != nil {
		return fmt.Errorf("servers: hand lock %s to %s: %w", path, owner.Username, chownErr)
	}
	return closeErr
}

// lockRoot takes the storage-root lock, creating the root if necessary.
func (m *Manager) lockRoot() (*filelock.Lock, error) {
	if _, err := os.Stat(m.cfg.Root); errors.Is(err, fs.ErrNotExist) && m.cfg.CreateRoot && m.euid != 0 {
		if err := os.MkdirAll(m.cfg.Root, 0o700); err != nil {
			return nil, fmt.Errorf("servers: create %s: %w", m.cfg.Root, err)
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("servers: SERVER_STORAGE_PATH %s does not exist; create it, owned by %s", m.cfg.Root, m.cfg.Owner.Username)
	}
	path := filepath.Join(m.cfg.Root, rootLockName)
	if err := m.lockFile(path, m.cfg.Owner); err != nil {
		return nil, err
	}
	return filelock.Acquire(path)
}

// lockExisting takes the root and server locks for an existing server, in
// filelock's fixed order, and proves the locked directory is still the one
// at dir.
func (m *Manager) lockExisting(dir string, owner identity.Identity) ([]*filelock.Lock, error) {
	rootPath := filepath.Join(m.cfg.Root, rootLockName)
	serverPath := filelock.ServerLockPath(dir)
	if err := m.lockFile(rootPath, m.cfg.Owner); err != nil {
		return nil, err
	}
	if err := m.lockFile(serverPath, owner); err != nil {
		return nil, err
	}
	locks, err := filelock.AcquireMany(rootPath, serverPath)
	if err != nil {
		return nil, err
	}
	for _, l := range locks {
		if l.Path() == serverPath {
			if err := verifyLock(l); err != nil {
				filelock.ReleaseAll(locks)
				return nil, err
			}
		}
	}
	return locks, nil
}

// LockServer takes one existing server's lock and returns its settings. It
// fails with ErrNotFound if the server was renamed or deleted while the
// caller waited. Lifecycle operations hold this lock; they do not take the
// storage-root lock.
func (m *Manager) LockServer(name string) (*filelock.Lock, *legacyconf.ServerSettings, error) {
	dir, err := m.existing(name)
	if err != nil {
		return nil, nil, err
	}
	s, err := m.settings(name, dir)
	if err != nil {
		return nil, nil, err
	}
	owner, err := m.owner(s)
	if err != nil {
		return nil, nil, err
	}
	if err := m.lockFile(filelock.ServerLockPath(dir), owner); err != nil {
		return nil, nil, err
	}
	l, err := filelock.Acquire(filelock.ServerLockPath(dir))
	if err != nil {
		return nil, nil, err
	}
	if err := verifyLock(l); err != nil {
		l.Release()
		return nil, nil, err
	}
	return l, s, nil
}

func verifyLock(l *filelock.Lock) error {
	held, err := l.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(l.Path())
	if err != nil || !os.SameFile(held, current) {
		return fmt.Errorf("%w: %s was renamed or deleted while waiting for its lock", ErrNotFound, filepath.Dir(l.Path()))
	}
	return nil
}

// requireStopped refuses a server that is not proven stopped.
func requireStopped(name string, st State) error {
	switch st.Kind {
	case Stopped:
		return nil
	case Running:
		return fmt.Errorf("%w: %q (%s); stop it first", ErrRunning, name, st.Detail)
	default:
		return fmt.Errorf("%w: %q is %s (%s)", ErrNotStopped, name, st.Kind, st.Detail)
	}
}

func randomSuffix() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("servers: random name: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// syncDir makes a rename in dir durable where the platform supports it.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

func isExist(err error) bool { return errors.Is(err, fs.ErrExist) }
