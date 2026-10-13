// Package jars implements JAR groups: directories below JAR_STORAGE_PATH
// that hold timestamped server JARs and a target file naming where new
// versions are downloaded from, plus selecting a group's JAR for a server
// (its JAR_PATH link).
//
// Every mutation of the store, and every change of a server's JAR link,
// holds the store's registry lock, so a rename or delete never races a
// selection. When run as root, the manager drops to the manager user
// (USERNAME) after locking and checking and before changing any file, as
// internal/servers does.
package jars

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/thehatchcloud/msm/internal/atomicfile"
	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/safepath"
	"github.com/thehatchcloud/msm/internal/servers"
)

var (
	ErrInvalidName   = errors.New("jars: invalid jar group name")
	ErrExists        = errors.New("jars: a jar group with that name already exists")
	ErrNotFound      = errors.New("jars: no such jar group")
	ErrNoTarget      = errors.New("jars: jar group has no download target")
	ErrInvalidTarget = errors.New("jars: invalid download target")
	ErrInsecureURL   = errors.New("jars: download target is not https")
	ErrInUse         = errors.New("jars: jar group is in use")
	ErrNoJAR         = errors.New("jars: no such JAR in the jar group")
	ErrInvalidJAR    = errors.New("jars: not a JAR file")
	ErrDownload      = errors.New("jars: download failed")
	ErrTooLarge      = errors.New("jars: download is too large")
	ErrVerify        = errors.New("jars: download does not match its published hash")
	ErrNotLink       = errors.New("jars: server JAR is not a link")
	ErrOwner         = errors.New("jars: server and jar store belong to different users")
)

const (
	// DefaultMaxBytes bounds one downloaded JAR.
	DefaultMaxBytes = 512 << 20
	// DefaultTimeout bounds one whole download.
	DefaultTimeout = 10 * time.Minute
	// DefaultManifestURL is Mojang's version manifest, which the
	// "minecraft" and "minecraft-snapshot" targets use.
	DefaultManifestURL = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"

	// stagePrefix names in-progress downloads in the download directory and
	// in-progress groups in the store; a group name cannot start with '.'.
	stagePrefix = ".msm-"
)

// Config describes one JAR store.
type Config struct {
	// Root is JAR_STORAGE_PATH: an absolute, clean path.
	Root string
	// Target and DownloadDir are JARGROUP_TARGET and JARGROUP_DOWNLOAD_DIR,
	// plain file names inside each group.
	Target, DownloadDir string
	// Owner is the manager user (USERNAME) that owns the store.
	Owner identity.Identity
	// Servers finds the servers whose JAR links point into the store.
	Servers *servers.Manager
	// CreateRoot lets Create make a missing Root (rootless default only).
	CreateRoot bool

	// HTTP downloads targets; nil means a client with verified TLS and the
	// timeouts below. Tests supply one that trusts a local test server.
	HTTP *http.Client
	// ManifestURL replaces DefaultManifestURL (tests).
	ManifestURL string
	// MaxBytes and Timeout replace DefaultMaxBytes and DefaultTimeout.
	MaxBytes int64
	Timeout  time.Duration
	Clock    clock.Clock

	// EUID is the effective user ID; nil means ask the OS. DropTo replaces
	// identity.DropTo (tests).
	EUID   *int
	DropTo func(identity.Identity) error
}

// Manager manages one JAR store.
type Manager struct {
	cfg     Config
	euid    int
	dropped bool
}

// New validates cfg and returns a Manager.
func New(cfg Config) (*Manager, error) {
	if err := safepath.ValidateConfiguredRoot(cfg.Root); err != nil {
		return nil, fmt.Errorf("jars: JAR_STORAGE_PATH: %w", err)
	}
	for setting, name := range map[string]string{"JARGROUP_TARGET": cfg.Target, "JARGROUP_DOWNLOAD_DIR": cfg.DownloadDir} {
		if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.HasPrefix(name, stagePrefix) {
			return nil, fmt.Errorf("jars: %s %q must be a plain file name", setting, name)
		}
	}
	if cfg.Servers == nil || cfg.Clock == nil {
		return nil, errors.New("jars: servers and clock are required")
	}
	if cfg.ManifestURL == "" {
		cfg.ManifestURL = DefaultManifestURL
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.HTTP == nil {
		cfg.HTTP = newHTTPClient()
	}
	if cfg.DropTo == nil {
		cfg.DropTo = identity.DropTo
	}
	m := &Manager{cfg: cfg, euid: os.Geteuid()}
	if cfg.EUID != nil {
		m.euid = *cfg.EUID
	}
	return m, nil
}

// Root is the store this manager manages.
func (m *Manager) Root() string { return m.cfg.Root }

// Group is one JAR group in a listing.
type Group struct {
	Name string
	// JARs are the group's timestamped JAR files, oldest first.
	JARs []string
}

// timestamped matches the names getlatest gives downloads.
var timestamped = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}-`)

// List returns every group, sorted by name, with its timestamped JARs, as
// jargroup_list shows them. A missing store has no groups.
func (m *Manager) List() ([]Group, error) {
	entries, err := os.ReadDir(m.cfg.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jars: list %s: %w", m.cfg.Root, err)
	}
	var groups []Group
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		g := Group{Name: e.Name()}
		files, err := os.ReadDir(filepath.Join(m.cfg.Root, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("jars: list %s: %w", e.Name(), err)
		}
		for _, f := range files {
			if f.Type().IsRegular() && timestamped.MatchString(f.Name()) {
				g.JARs = append(g.JARs, f.Name())
			}
		}
		sort.Strings(g.JARs)
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	return groups, nil
}

// dir validates a group name and returns its directory, whether or not it
// exists.
func (m *Manager) dir(name string) (string, error) {
	if err := safepath.ValidateName(name); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidName, err)
	}
	return filepath.Join(m.cfg.Root, name), nil
}

// existing returns the directory of an existing group, matched exactly
// (not by case) and never through a symbolic link.
func (m *Manager) existing(name string) (string, error) {
	dir, err := m.dir(name)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(m.cfg.Root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("jars: list %s: %w", m.cfg.Root, err)
	}
	for _, e := range entries {
		if e.Name() == name {
			if !e.IsDir() {
				return "", fmt.Errorf("%w %q: %s is not a directory", ErrNotFound, name, dir)
			}
			return dir, nil
		}
	}
	return "", fmt.Errorf("%w %q", ErrNotFound, name)
}

// available checks that no entry below the root has name, in any case.
func (m *Manager) available(name string) (string, error) {
	dir, err := m.dir(name)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(dir); err == nil {
		return "", fmt.Errorf("%w: %s", ErrExists, dir)
	}
	if other, found, err := safepath.CaseInsensitiveCollision(m.cfg.Root, name); err == nil && found {
		return "", fmt.Errorf("%w: %q differs from %q only in letter case", ErrExists, name, other)
	}
	return dir, nil
}

// lock takes the registry lock, creating the store first when allowed.
func (m *Manager) lock(create bool) (*filelock.Lock, error) {
	if m.euid != 0 && m.euid != m.cfg.Owner.UID {
		return nil, fmt.Errorf("%w: the jar store %s belongs to %s (uid %d); run as that user or as root",
			identity.ErrPrivilegeRequired, m.cfg.Root, m.cfg.Owner.Username, m.cfg.Owner.UID)
	}
	if _, err := os.Stat(m.cfg.Root); errors.Is(err, fs.ErrNotExist) {
		if !create || !m.cfg.CreateRoot || m.euid == 0 {
			return nil, fmt.Errorf("jars: JAR_STORAGE_PATH %s does not exist; create it, owned by %s", m.cfg.Root, m.cfg.Owner.Username)
		}
		if err := os.MkdirAll(m.cfg.Root, 0o755); err != nil {
			return nil, fmt.Errorf("jars: create %s: %w", m.cfg.Root, err)
		}
	}
	path := filelock.RegistryLockPath(m.cfg.Root)
	if m.euid == 0 {
		// Created exclusively, never through a link, and handed to the owner.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			chownErr := f.Chown(m.cfg.Owner.UID, m.cfg.Owner.GID)
			f.Close()
			if chownErr != nil {
				return nil, fmt.Errorf("jars: hand lock %s to %s: %w", path, m.cfg.Owner.Username, chownErr)
			}
		} else if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("jars: create lock %s: %w", path, err)
		}
	}
	return filelock.Acquire(path)
}

// drop gives up root, once, before the first change to the store.
func (m *Manager) drop(to identity.Identity) error {
	if m.euid != 0 || m.dropped || to.UID == 0 {
		return nil
	}
	if err := m.cfg.DropTo(to); err != nil {
		return fmt.Errorf("jars: drop privileges to %s: %w", to.Username, err)
	}
	m.dropped = true
	return nil
}

// Create makes a new group with a download target (jargroup create).
func (m *Manager) Create(name, target string) error {
	dir, err := m.available(name)
	if err != nil {
		return err
	}
	if _, err := ParseTarget(target); err != nil {
		return err
	}
	lock, err := m.lock(true)
	if err != nil {
		return err
	}
	defer lock.Release()
	if _, err := m.available(name); err != nil {
		return err
	}
	if err := m.drop(m.cfg.Owner); err != nil {
		return err
	}
	// Assemble the group under a staging name and publish it in one rename,
	// so an interruption never leaves a group without its target file.
	stage := filepath.Join(m.cfg.Root, stagePrefix+"group-"+name+"-"+randomSuffix())
	if err := os.Mkdir(stage, 0o755); err != nil {
		return fmt.Errorf("jars: create %s: %w", stage, err)
	}
	if err := os.WriteFile(filepath.Join(stage, m.cfg.Target), []byte(target+"\n"), 0o644); err != nil {
		os.RemoveAll(stage)
		return fmt.Errorf("jars: write %s: %w", m.cfg.Target, err)
	}
	if err := os.Rename(stage, dir); err != nil {
		os.RemoveAll(stage)
		return fmt.Errorf("jars: create %s: %w", dir, err)
	}
	syncDir(m.cfg.Root)
	return nil
}

// ChangeURL replaces an existing group's download target (jargroup
// changeurl). The group must already have a target file.
func (m *Manager) ChangeURL(name, target string) error {
	dir, err := m.existing(name)
	if err != nil {
		return err
	}
	if _, err := ParseTarget(target); err != nil {
		return err
	}
	lock, err := m.lock(false)
	if err != nil {
		return err
	}
	defer lock.Release()
	path := filepath.Join(dir, m.cfg.Target)
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: could not find URL target file %q", ErrNoTarget, path)
	}
	if err := m.drop(m.cfg.Owner); err != nil {
		return err
	}
	return atomicfile.Write(path, []byte(target+"\n"), 0o644)
}

// Target reads a group's download target: the first non-empty line of its
// target file. More than one line is refused, as the legacy manager failed
// when its target downloaded several files.
func (m *Manager) Target(name string) (Target, error) {
	dir, err := m.existing(name)
	if err != nil {
		return Target{}, err
	}
	return m.readTarget(dir)
}

func (m *Manager) readTarget(dir string) (Target, error) {
	path := filepath.Join(dir, m.cfg.Target)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Target{}, fmt.Errorf("%w: %s; set one with msm jargroup changeurl %s <download-url>", ErrNoTarget, path, filepath.Base(dir))
	}
	if info.Size() > 64<<10 {
		return Target{}, fmt.Errorf("%w: %s is too large", ErrInvalidTarget, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Target{}, fmt.Errorf("jars: read %s: %w", path, err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	switch len(lines) {
	case 0:
		return Target{}, fmt.Errorf("%w: %s is empty", ErrNoTarget, path)
	case 1:
		return ParseTarget(lines[0])
	default:
		return Target{}, fmt.Errorf("%w: %s names %d URLs; a jar group downloads one file", ErrInvalidTarget, path, len(lines))
	}
}

// Reference is a server whose JAR link points into a group.
type Reference struct {
	Server string
	// Link is the server's JAR_PATH; JAR is the file it points to.
	Link, JAR string
	owner     identity.Identity
}

// References lists the servers whose JAR_PATH is a symbolic link into the
// named group, in server name order.
func (m *Manager) References(group string) ([]Reference, error) {
	dir := filepath.Join(m.cfg.Root, group)
	names, err := m.cfg.Servers.Names()
	if err != nil {
		return nil, err
	}
	var refs []Reference
	for _, name := range names {
		s, err := m.cfg.Servers.Settings(name)
		if err != nil {
			return nil, err
		}
		link := s.Get("JAR_PATH")
		target, err := os.Readlink(link)
		if err != nil {
			continue // not a link: not a reference
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(link), target)
		}
		target = filepath.Clean(target)
		if target == dir || strings.HasPrefix(target, dir+string(filepath.Separator)) {
			owner, err := m.cfg.Servers.Owner(s)
			if err != nil {
				return nil, err
			}
			refs = append(refs, Reference{Server: name, Link: link, JAR: target, owner: owner})
		}
	}
	return refs, nil
}

// Delete removes a group that no server uses (jargroup delete). The caller
// confirms first.
func (m *Manager) Delete(name string) error {
	dir, err := m.existing(name)
	if err != nil {
		return err
	}
	lock, err := m.lock(false)
	if err != nil {
		return err
	}
	defer lock.Release()
	if dir, err = m.existing(name); err != nil {
		return err
	}
	refs, err := m.References(name)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return fmt.Errorf("%w: %s; select another JAR for %s first", ErrInUse, describe(refs), serverList(refs))
	}
	if err := m.drop(m.cfg.Owner); err != nil {
		return err
	}
	// Leave service in one step, then remove without following links.
	trash := filepath.Join(m.cfg.Root, stagePrefix+"delete-"+name+"-"+randomSuffix())
	if err := os.Rename(dir, trash); err != nil {
		return fmt.Errorf("jars: delete %s: %w", dir, err)
	}
	if err := os.RemoveAll(trash); err != nil {
		return fmt.Errorf("jars: jar group %q is gone, but removing %s failed: %w", name, trash, err)
	}
	return nil
}

// Rename renames a group and points every server that used it at the same
// JAR under the new name (DEV-009). If any step fails, the links already
// changed are restored and the group is moved back.
func (m *Manager) Rename(from, to string) ([]Reference, error) {
	oldDir, err := m.existing(from)
	if err != nil {
		return nil, err
	}
	if from != to && strings.EqualFold(from, to) {
		return nil, fmt.Errorf("%w: renaming %q to %q changes only letter case; rename it to another name first", ErrExists, from, to)
	}
	newDir, err := m.available(to)
	if err != nil {
		return nil, err
	}
	lock, err := m.lock(false)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	if _, err := m.existing(from); err != nil {
		return nil, err
	}
	if _, err := m.available(to); err != nil {
		return nil, err
	}
	refs, err := m.References(from)
	if err != nil {
		return nil, err
	}
	if m.euid == 0 {
		for _, r := range refs {
			if r.owner.UID != m.cfg.Owner.UID {
				return nil, fmt.Errorf("%w: server %q belongs to %s, the jar store to %s; as root the manager changes links only as the store's user, so run the rename as %s after making them the same, or re-point %q by hand",
					ErrOwner, r.Server, r.owner.Username, m.cfg.Owner.Username, m.cfg.Owner.Username, r.Server)
			}
		}
	}
	if err := m.drop(m.cfg.Owner); err != nil {
		return nil, err
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return nil, fmt.Errorf("jars: rename %s: %w", oldDir, err)
	}
	var done []Reference
	for _, r := range refs {
		newJAR := newDir + strings.TrimPrefix(r.JAR, oldDir)
		if err := replaceLink(r.Link, newJAR); err != nil {
			return nil, m.undoRename(fmt.Errorf("re-point server %q: %w", r.Server, err), oldDir, newDir, done)
		}
		done = append(done, r)
	}
	syncDir(m.cfg.Root)
	for i := range refs {
		refs[i].JAR = newDir + strings.TrimPrefix(refs[i].JAR, oldDir)
	}
	return refs, nil
}

func (m *Manager) undoRename(cause error, oldDir, newDir string, done []Reference) error {
	var failed []string
	for _, r := range done {
		if err := replaceLink(r.Link, r.JAR); err != nil {
			failed = append(failed, fmt.Sprintf("%s still points into %s: %v", r.Link, newDir, err))
		}
	}
	if err := os.Rename(newDir, oldDir); err != nil {
		failed = append(failed, fmt.Sprintf("the group is still at %s: %v", newDir, err))
	}
	if len(failed) > 0 {
		return fmt.Errorf("jars: rename failed (%v) and could not be fully undone: %s", cause, strings.Join(failed, "; "))
	}
	return fmt.Errorf("jars: %w; nothing was renamed", cause)
}

// symlink is os.Symlink; tests replace it to fail a chosen step.
var symlink = os.Symlink

// replaceLink atomically points the link at path to target.
func replaceLink(path, target string) error {
	tmp := filepath.Join(filepath.Dir(path), stagePrefix+"link-"+randomSuffix())
	if err := symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func describe(refs []Reference) string {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = fmt.Sprintf("server %q uses %s", r.Server, r.JAR)
	}
	return strings.Join(parts, ", ")
}

func serverList(refs []Reference) string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = fmt.Sprintf("%q", r.Server)
	}
	if len(names) == 1 {
		return "server " + names[0]
	}
	return "servers " + strings.Join(names, ", ")
}

func randomSuffix() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}
