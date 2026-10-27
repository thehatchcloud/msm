// Package worlds implements the world commands of a server: listing its
// worlds, linking the active ones into the server directory, and activating
// or deactivating a world by moving it between the active and inactive
// world storage directories.
//
// A server keeps its worlds in WORLD_STORAGE_PATH (active) and
// WORLD_STORAGE_INACTIVE_PATH (inactive). Only active worlds are linked into
// the server directory, where Minecraft finds them; inactive worlds are
// excluded from world-only backups. Every change holds the server's lock and
// first proves the server is stopped. Links are only ever created or
// replaced where the name is free or already a symbolic link, never over a
// real file or directory.
package worlds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/servers"
)

var (
	ErrNoWorld   = errors.New("worlds: no such world")
	ErrAmbiguous = errors.New("worlds: ambiguous world")
	ErrConflict  = errors.New("worlds: a world with that name is already in the way")
	ErrLayout    = errors.New("worlds: invalid world storage layout")
	ErrLinks     = errors.New("worlds: some worlds could not be linked")
	ErrRAMCopy   = errors.New("worlds: world has a RAM copy")
)

// stagePrefix names the hidden directories a cross-filesystem move leaves
// behind if it is interrupted; List reports them.
const stagePrefix = ".msm-world-"

// Output is where a command writes.
type Output struct{ Out, Err io.Writer }

func (o Output) printf(format string, a ...any) {
	if o.Out != nil {
		fmt.Fprintf(o.Out, format+"\n", a...)
	}
}

func (o Output) warnf(format string, a ...any) {
	if o.Err != nil {
		fmt.Fprintf(o.Err, format+"\n", a...)
	}
}

// Config connects worlds to the servers.
type Config struct {
	Servers *servers.Manager
	// Saver, when set, lets ToDisk save a running server around a
	// synchronization. Without it ToDisk needs a stopped server.
	Saver Saver
}

// Manager manages the worlds of the servers of one storage root.
type Manager struct {
	cfg Config
	ops fsOps
	// ramCheck proves a path is on a RAM-backed filesystem and free reports
	// the space left on a filesystem; tests replace them.
	ramCheck func(path string) error
	free     func(path string) (uint64, error)
}

// New returns a Manager.
func New(cfg Config) (*Manager, error) {
	if cfg.Servers == nil {
		return nil, errors.New("worlds: a server manager is required")
	}
	m := &Manager{cfg: cfg, ops: osOps{}, free: freeBytes}
	m.ramCheck = func(path string) error { return checkRAMBacked(path, execProgram) }
	return m, nil
}

// WithSaver returns a manager that also saves running servers, as ToDisk
// needs.
func (m *Manager) WithSaver(c Saver) *Manager {
	cp := *m
	cp.cfg.Saver = c
	return &cp
}

// World is one world directory of a server.
type World struct {
	Name   string
	Dir    string
	Active bool
	// InRAM is whether the world carries the RAM flag file.
	InRAM bool
}

// layout is a server's resolved world storage.
type layout struct {
	s              *legacyconf.ServerSettings
	active, inact  string
	flag           string
	worlds         []World
	warnings       []string
	ambiguousNames map[string]bool
}

func (m *Manager) resolve(s *legacyconf.ServerSettings) (*layout, error) {
	l := &layout{s: s, active: filepath.Clean(s.Get("WORLD_STORAGE_PATH")), inact: filepath.Clean(s.Get("WORLD_STORAGE_INACTIVE_PATH")),
		flag: s.Get("WORLDS_FLAG_INRAM")}
	dir := filepath.Clean(s.Dir)
	switch {
	case l.active == l.inact:
		return nil, fmt.Errorf("%w: WORLD_STORAGE_PATH and WORLD_STORAGE_INACTIVE_PATH are both %s; they must differ", ErrLayout, l.active)
	case l.active == dir || l.inact == dir:
		return nil, fmt.Errorf("%w: world storage may not be the server directory %s itself", ErrLayout, dir)
	case within(l.active, l.inact) || within(l.inact, l.active):
		return nil, fmt.Errorf("%w: %s and %s may not contain one another", ErrLayout, l.active, l.inact)
	}
	if f := l.flag; f == "" || f == "." || f == ".." || filepath.Base(f) != f {
		return nil, fmt.Errorf("%w: WORLDS_FLAG_INRAM %q must be a plain file name", ErrLayout, f)
	}
	return l, nil
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validWorldName accepts a directory name Minecraft and the server
// directory can both hold: one path element, not hidden, no control
// characters.
func validWorldName(name string) bool {
	if name == "" || name[0] == '.' || len(name) > 255 || strings.ContainsRune(name, '/') {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// discover lists the worlds in both storage directories. Only real
// directories are worlds: a symbolic link, a file and a hidden or oddly
// named directory are ignored (and reported), so a link planted in storage
// never makes the manager touch what it points at. A storage directory that
// is itself a symbolic link, such as one pointing at another disk, is
// followed; one that is missing has no worlds.
func (l *layout) discover() error {
	seen := map[string][]bool{}
	for _, dir := range []struct {
		path   string
		active bool
	}{{l.active, true}, {l.inact, false}} {
		entries, err := os.ReadDir(dir.path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("worlds: list %s: %w", dir.path, err)
		}
		for _, e := range entries {
			name, path := e.Name(), filepath.Join(dir.path, e.Name())
			switch {
			case strings.HasPrefix(name, stagePrefix):
				l.warnings = append(l.warnings, fmt.Sprintf("%s is left over from an interrupted world move; the world it belongs to is still in its original place, so inspect and remove it", path))
				continue
			case e.Type()&fs.ModeSymlink != 0:
				l.warnings = append(l.warnings, fmt.Sprintf("%s is a symbolic link, not a world directory, and is ignored", path))
				continue
			case !e.IsDir():
				continue
			case !validWorldName(name):
				l.warnings = append(l.warnings, fmt.Sprintf("%s is ignored: %q is not a usable world name", path, name))
				continue
			}
			_, flagErr := os.Stat(filepath.Join(path, l.flag))
			l.worlds = append(l.worlds, World{Name: name, Dir: path, Active: dir.active, InRAM: flagErr == nil})
			seen[name] = append(seen[name], dir.active)
		}
	}
	sort.SliceStable(l.worlds, func(i, j int) bool {
		if l.worlds[i].Name != l.worlds[j].Name {
			return l.worlds[i].Name < l.worlds[j].Name
		}
		return l.worlds[i].Active && !l.worlds[j].Active
	})
	l.ambiguousNames = map[string]bool{}
	for name, where := range seen {
		if len(where) > 1 {
			l.ambiguousNames[name] = true
			l.warnings = append(l.warnings, fmt.Sprintf("world %q exists in both %s and %s; it is not linked, activated or deactivated until you remove or rename one", name, l.active, l.inact))
		}
	}
	return nil
}

func (l *layout) find(name string) (World, error) {
	var found []World
	for _, w := range l.worlds {
		if w.Name == name {
			found = append(found, w)
		}
	}
	switch len(found) {
	case 0:
		return World{}, fmt.Errorf("%w with the name %q", ErrNoWorld, name)
	case 1:
		return found[0], nil
	}
	return World{}, fmt.Errorf("%w: %q exists in both %s and %s; remove or rename one", ErrAmbiguous, name, l.active, l.inact)
}

// load resolves a server's settings and layout and discovers its worlds.
func (m *Manager) load(s *legacyconf.ServerSettings) (*layout, error) {
	l, err := m.resolve(s)
	if err != nil {
		return nil, err
	}
	return l, l.discover()
}

// List prints a server's worlds, active and inactive, as worlds list does:
// one line each, with "RAM" in front of a world flagged to live in RAM.
// Hidden leftovers and ignored entries are reported on stderr.
func (m *Manager) List(ctx context.Context, name string, out Output) error {
	s, err := m.cfg.Servers.Settings(name)
	if err != nil {
		return err
	}
	l, err := m.load(s)
	if err != nil {
		return err
	}
	for _, w := range l.warnings {
		out.warnf("Warning: %s", w)
	}
	if len(l.worlds) == 0 {
		out.printf("There are no worlds in worldstorage.")
		return nil
	}
	for _, w := range l.worlds {
		if w.InRAM {
			out.printf("RAM  %s", w.Name)
		} else {
			out.printf("     %s", w.Name)
		}
	}
	return nil
}

// Names lists a server's world names, for completion and for "all".
func (m *Manager) Names(name string) ([]string, error) {
	s, err := m.cfg.Servers.Settings(name)
	if err != nil {
		return nil, err
	}
	l, err := m.load(s)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, w := range l.worlds {
		if len(names) == 0 || names[len(names)-1] != w.Name {
			names = append(names, w.Name)
		}
	}
	return names, nil
}

// session is a server locked and proven stopped.
type session struct {
	lock  *filelock.Lock
	s     *legacyconf.ServerSettings
	owner identity.Identity
}

func (x *session) release() { x.lock.Release() }

// open locks a server and proves it is stopped.
func (m *Manager) open(ctx context.Context, name string) (*session, error) {
	lock, s, err := m.cfg.Servers.LockServer(name)
	if err != nil {
		return nil, err
	}
	owner, err := m.cfg.Servers.Owner(s)
	if err != nil {
		lock.Release()
		return nil, err
	}
	if err := servers.RequireStopped(name, m.cfg.Servers.Probe(ctx, s)); err != nil {
		lock.Release()
		return nil, err
	}
	return &session{lock: lock, s: s, owner: owner}, nil
}

// ForAll runs fn for every server in name order, one at a time, with each
// line of its output prefixed by the server's name. It attempts every server
// and fails if any failed, with one error naming each failure in name order,
// so the status does not depend on what happened to run first.
func (m *Manager) ForAll(ctx context.Context, out Output, fn func(name string, out Output) error) error {
	names, err := m.cfg.Servers.Names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		out.printf("There are no servers.")
		return nil
	}
	var failures []string
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		o := Output{Out: &prefixWriter{out: out.Out, prefix: name + ": "}, Err: &prefixWriter{out: out.Err, prefix: name + ": "}}
		err := fn(name, o)
		o.Out.(*prefixWriter).flush()
		o.Err.(*prefixWriter).flush()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d servers failed:\n  %s", len(failures), len(names), strings.Join(failures, "\n  "))
	}
	return nil
}

// prefixWriter writes whole lines with a prefix.
type prefixWriter struct {
	out    io.Writer
	prefix string
	buf    []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.emit(w.buf[:i+1])
		w.buf = w.buf[i+1:]
	}
}

func (w *prefixWriter) emit(line []byte) {
	if w.out == nil {
		return
	}
	io.WriteString(w.out, w.prefix)
	w.out.Write(line)
}

func (w *prefixWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(append(w.buf, '\n'))
		w.buf = nil
	}
}

// Inventory is what a server's world storage holds, for the backup
// commands. Ambiguous names, which exist in both storage directories, are
// listed in Worlds twice; a caller that needs one world skips them.
type Inventory struct {
	ActiveDir, InactiveDir string
	Worlds                 []World
	Ambiguous              map[string]bool
	// Warnings describe entries that are ignored or left over.
	Warnings []string
}

// Inventory discovers a server's worlds without changing anything.
func (m *Manager) Inventory(s *legacyconf.ServerSettings) (*Inventory, error) {
	l, err := m.load(s)
	if err != nil {
		return nil, err
	}
	return &Inventory{ActiveDir: l.active, InactiveDir: l.inact, Worlds: l.worlds,
		Ambiguous: l.ambiguousNames, Warnings: l.warnings}, nil
}

// HasRAMCopy reports whether the world is served from a RAM copy that may
// hold newer data than its directory on disk: RAM worlds are enabled, the
// world is flagged and the copy exists.
func (m *Manager) HasRAMCopy(s *legacyconf.ServerSettings, w World) bool {
	root := m.ramRoot()
	if root == "" || !w.InRAM {
		return false
	}
	_, err := os.Stat(filepath.Join(root, s.Name, w.Name))
	return err == nil
}

func execProgram(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
