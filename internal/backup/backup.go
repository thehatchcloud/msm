// Package backup makes ZIP backups of a server's worlds and of a whole
// server directory with Go's archive/zip, in the layout the legacy manager
// used:
//
//	<WORLD_ARCHIVE_PATH>/<server>/<world>/<YYYY-MM-DD-HH-MM-SS>.zip
//	    holding <world>/...   (one archive per active world)
//	<BACKUP_ARCHIVE_PATH>/<server>/<YYYY-MM-DD-HH-MM-SS>.zip
//	    holding <server>/...  (the whole server directory)
//
// Both are timestamped archives that WorldEdit's snapshot database accepts
// (see the acceptance test, which mirrors its source).
//
// A world backup of a running server saves the worlds with the profile's
// own commands, confirms each one in the log, copies a stable image of the
// worlds to a staging directory beside the archives, turns saving back on,
// and only then compresses the copy. Saving is re-enabled on every error,
// on cancellation and on timeout; a marker file in the server directory
// records the operation so that the next backup re-enables saving if the
// manager itself was killed.
//
// Archives are written under a temporary name and published with one
// atomic link only when complete, so a full disk, a permission error, a
// timeout or a killed process never leaves something that looks like a
// finished archive. A backup never replaces an existing archive and never
// includes its own archive directory.
package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/console"
	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/profiles"
	"github.com/thehatchcloud/msm/internal/safepath"
	"github.com/thehatchcloud/msm/internal/servers"
	"github.com/thehatchcloud/msm/internal/worlds"
)

var (
	ErrNotProven   = errors.New("backup: server state cannot be proven")
	ErrNoWorlds    = errors.New("backup: the server has no active worlds to back up")
	ErrUnsupported = errors.New("backup: a configured backup method is not available")
	ErrDisabled    = errors.New("backup: world archives are disabled")
	ErrSettings    = errors.New("backup: invalid setting")
	ErrRAMWorld    = errors.New("backup: a world lives in RAM")
	ErrSavingLeft  = errors.New("backup: saving may still be disabled")
)

const (
	// DefaultTimeout bounds compressing the archives.
	DefaultTimeout = 2 * time.Hour
	// DefaultSaveWindow bounds how long saving stays off while the
	// worlds are copied.
	DefaultSaveWindow = 15 * time.Minute
	// restoreTimeout bounds turning saving back on after an error.
	restoreTimeout = 2 * time.Minute

	defaultWorldArchivePath  = "/opt/msm/archives/worlds"
	defaultServerArchivePath = "/opt/msm/archives/backups"
)

// Console is the part of the console manager a backup uses. The caller
// holds the server's lock throughout, so both calls are the lock-free
// forms.
type Console interface {
	Running(ctx context.Context, set *legacyconf.ServerSettings, out console.Output) (bool, error)
	DoLocked(ctx context.Context, set *legacyconf.ServerSettings, steps []console.Step, out console.Output) error
}

var _ Console = (*console.Manager)(nil)

// Output is where a command writes.
type Output = worlds.Output

// Config connects backups to the host.
type Config struct {
	Servers *servers.Manager
	Worlds  *worlds.Manager
	Console Console
	Clock   clock.Clock
	// Timeout bounds compressing; zero means DefaultTimeout.
	Timeout time.Duration
	// SaveWindow bounds how long saving is off; zero means
	// DefaultSaveWindow.
	SaveWindow time.Duration
	// JarRoot is JAR_STORAGE_PATH. A server JAR is normally a link into it,
	// so a followed link there is not "outside the server".
	JarRoot string
	// AllowExternal permits followed links to targets outside the server
	// after the administrator has reviewed them.
	AllowExternal bool
	// SyncRAM, when set, writes a server's RAM worlds to disk. Without it
	// a backup refuses a server whose world lives in RAM, since the copy
	// on disk may be stale.
	SyncRAM func(ctx context.Context, set *legacyconf.ServerSettings) error
}

// Manager makes backups.
type Manager struct {
	cfg Config
	ops fsOps
}

// New returns a Manager.
func New(cfg Config) (*Manager, error) {
	if cfg.Servers == nil || cfg.Worlds == nil || cfg.Console == nil || cfg.Clock == nil {
		return nil, errors.New("backup: servers, worlds, console and clock are required")
	}
	if cfg.Timeout < 0 || cfg.SaveWindow < 0 {
		return nil, errors.New("backup: timeouts must not be negative")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.SaveWindow == 0 {
		cfg.SaveWindow = DefaultSaveWindow
	}
	return &Manager{cfg: cfg, ops: osOps{}}, nil
}

type mode int

const (
	modeWorlds mode = iota
	modeComplete
)

func (md mode) String() string {
	if md == modeWorlds {
		return "worlds"
	}
	return "complete"
}

// Worlds backs up a server's active worlds, one archive each.
func (m *Manager) Worlds(ctx context.Context, name string, out Output) error {
	return m.run(ctx, name, modeWorlds, out)
}

// Server backs up a server's whole directory, inactive worlds included.
func (m *Manager) Server(ctx context.Context, name string, out Output) error {
	return m.run(ctx, name, modeComplete, out)
}

// ForAll runs fn for every server, as the world commands do.
func (m *Manager) ForAll(ctx context.Context, out Output, fn func(name string, out Output) error) error {
	return m.cfg.Worlds.ForAll(ctx, out, fn)
}

func cout(o Output) console.Output { return console.Output{Out: o.Out, Err: o.Err} }

func printf(w io.Writer, format string, a ...any) {
	if w != nil {
		fmt.Fprintf(w, format+"\n", a...)
	}
}

// job is a backup, resolved and checked, before the server is touched.
type job struct {
	mode  mode
	set   *legacyconf.ServerSettings
	base  string // the server's archive directory
	units []unit
	// exclude are the directories no archive may include.
	exclude []os.FileInfo
	policy  policy
	// warnings are shown before the backup starts.
	warnings []string
	ramSync  bool
}

// unit is one archive.
type unit struct {
	name    string // world name, or server name
	root    string // directory to archive
	dir     string // where its archive is published
	entries []entry
}

func (m *Manager) global(name, def string) string {
	if g := m.cfg.Servers.Global(); g != nil {
		if v, ok := g.Lookup(name); ok {
			return v
		}
	}
	return def
}

// archiveRoot is a configured archive directory. With no msm.conf it is
// beside the servers, so a rootless installation keeps its backups with its
// data.
func (m *Manager) archiveRoot(setting, def, sibling string) (string, error) {
	p := def
	if m.cfg.Servers.Global() == nil {
		p = filepath.Join(filepath.Dir(m.cfg.Servers.Root()), "archives", sibling)
	}
	p = m.global(setting, p)
	if err := safepath.ValidateConfiguredRoot(p); err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrSettings, setting, err)
	}
	return p, nil
}

func (m *Manager) bases(set *legacyconf.ServerSettings) (worldsBase, serverBase string, err error) {
	w, err := m.archiveRoot("WORLD_ARCHIVE_PATH", defaultWorldArchivePath, "worlds")
	if err != nil {
		return "", "", err
	}
	s, err := m.archiveRoot("BACKUP_ARCHIVE_PATH", defaultServerArchivePath, "backups")
	if err != nil {
		return "", "", err
	}
	return filepath.Join(w, set.Name), filepath.Join(s, set.Name), nil
}

func (m *Manager) run(ctx context.Context, name string, md mode, out Output) (err error) {
	started := m.cfg.Clock.Now()
	lock, set, err := m.cfg.Servers.LockServer(name)
	if err != nil {
		return err
	}
	defer lock.Release()
	owner, err := m.cfg.Servers.Owner(set)
	if err != nil {
		return err
	}
	if err := m.cfg.Servers.ActAsOwner(owner); err != nil {
		return err
	}
	running, err := m.cfg.Console.Running(ctx, set, cout(out))
	if err != nil {
		return err
	}
	if !running {
		if err := servers.RequireStopped(name, m.cfg.Servers.Probe(ctx, set)); err != nil {
			return fmt.Errorf("%w: %v", ErrNotProven, err)
		}
	}
	if err := m.recover(ctx, set, running, out); err != nil {
		return err
	}
	j, err := m.prepare(set, md)
	if err != nil {
		return err
	}
	for _, w := range j.warnings {
		printf(out.Err, "Warning: %s", w)
	}
	if running && j.ramSync && m.cfg.SyncRAM == nil {
		return fmt.Errorf("%w: its copy on disk may be stale, and this build cannot write RAM worlds to disk yet", ErrRAMWorld)
	}
	defer func() {
		if err == nil {
			printf(out.Out, "Backup took %d seconds.", int(m.cfg.Clock.Now().Sub(started).Seconds()))
		}
	}()
	if err := writePending(set.Dir, pending{Operation: md.String(), Server: name, PID: os.Getpid(), Started: started}); err != nil {
		return err
	}
	if !running {
		printf(out.Out, "Server %q is stopped; its files cannot change during the backup.", name)
		return m.finish(ctx, j, nil, out)
	}
	return m.live(ctx, j, out)
}

// recover re-enables saving and removes what an interrupted backup left,
// before this backup starts. Holding the server's lock proves the earlier
// backup's process is gone.
func (m *Manager) recover(ctx context.Context, set *legacyconf.ServerSettings, running bool, out Output) error {
	p, found, err := readPending(set.Dir)
	if err != nil {
		return err
	}
	if found {
		printf(out.Err, "Warning: an earlier %s backup of %q was interrupted at %s; recovering.", p.Operation, set.Name, p.Started.Format(time.RFC3339))
		if p.SavingOff && running {
			if err := m.cfg.Console.DoLocked(ctx, set, []console.Step{{Command: profiles.SaveOn, Done: "Saving re-enabled."}}, cout(out)); err != nil {
				return fmt.Errorf("%w: could not re-enable saving after an interrupted backup: %v", ErrSavingLeft, err)
			}
		}
	}
	w, s, err := m.bases(set)
	if err != nil {
		return err
	}
	var errs []error
	for _, base := range []string{w, s} {
		removed, err := sweep(base)
		errs = append(errs, err)
		for _, r := range removed {
			printf(out.Err, "Removed %s, left by an interrupted backup.", r)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("clean up an interrupted backup: %w", err)
	}
	if found {
		return removePending(set.Dir)
	}
	return nil
}

// prepare checks the configuration and the tree, and makes the archive
// directories, without touching the server.
func (m *Manager) prepare(set *legacyconf.ServerSettings, md mode) (*job, error) {
	worldsBase, serverBase, err := m.bases(set)
	if err != nil {
		return nil, err
	}
	j := &job{mode: md, set: set}
	if md == modeWorlds {
		if err := m.requireZipOnly(); err != nil {
			return nil, err
		}
		j.base = worldsBase
	} else {
		j.base = serverBase
	}
	if err := os.MkdirAll(j.base, archiveDirPm); err != nil {
		return nil, fmt.Errorf("create the archive directory: %w", err)
	}
	base, err := os.Stat(j.base)
	if err != nil {
		return nil, err
	}
	j.exclude = []os.FileInfo{base}
	j.policy = policy{Exclude: j.exclude, ExcludeNames: []string{pendingName, filepath.Base(filelock.ServerLockPath(set.Dir))}}

	if md == modeWorlds {
		return j, m.prepareWorlds(j)
	}
	return j, m.prepareComplete(j)
}

// requireZipOnly refuses a configuration that asks for a backup method this
// build does not make, instead of dropping it silently.
func (m *Manager) requireZipOnly() error {
	for _, name := range []string{"RDIFF_BACKUP_ENABLED", "RSYNC_BACKUP_ENABLED"} {
		if m.global(name, "false") == "true" {
			return fmt.Errorf("%w: %s=true, but rdiff-backup and rsync backups are not part of the ZIP backups (P11 adds them); set it to false to make ZIP backups only", ErrUnsupported, name)
		}
	}
	if m.global("WORLD_ARCHIVE_ENABLED", "true") != "true" {
		return fmt.Errorf("%w: WORLD_ARCHIVE_ENABLED is %q, so no world archive would be made", ErrDisabled, m.global("WORLD_ARCHIVE_ENABLED", "true"))
	}
	return nil
}

func (m *Manager) prepareWorlds(j *job) error {
	inv, err := m.cfg.Worlds.Inventory(j.set)
	if err != nil {
		return err
	}
	j.warnings = append(j.warnings, inv.Warnings...)
	for _, w := range inv.Worlds {
		if !w.Active || inv.Ambiguous[w.Name] {
			continue
		}
		if m.cfg.Worlds.HasRAMCopy(j.set, w) {
			j.ramSync = true
		}
		dir := filepath.Join(j.base, w.Name)
		if err := os.MkdirAll(dir, archiveDirPm); err != nil {
			return fmt.Errorf("create the archive directory for world %q: %w", w.Name, err)
		}
		j.units = append(j.units, unit{name: w.Name, root: w.Dir, dir: dir})
	}
	if len(j.units) == 0 {
		return ErrNoWorlds
	}
	// World archives never follow links (see DEV-025).
	j.policy.Follow = false
	return m.replan(j)
}

func (m *Manager) prepareComplete(j *job) error {
	set := j.set
	j.units = []unit{{name: set.Name, root: set.Dir, dir: j.base}}
	j.policy.Follow = set.Get("COMPLETE_BACKUP_FOLLOW_SYMLINKS") == "true"
	j.policy.AllowExternal = m.cfg.AllowExternal
	allowed := []string{set.Dir, set.Get("WORLD_STORAGE_PATH"), set.Get("WORLD_STORAGE_INACTIVE_PATH"), m.cfg.JarRoot}
	for _, a := range allowed {
		if a == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(a); err == nil {
			j.policy.Allowed = append(j.policy.Allowed, real)
		}
	}
	for _, dir := range []string{set.Get("WORLD_STORAGE_PATH"), set.Get("WORLD_STORAGE_INACTIVE_PATH")} {
		if !within(set.Dir, dir) {
			j.warnings = append(j.warnings, fmt.Sprintf("%s is outside the server directory, so the worlds in it are not part of this backup; back them up with the worlds command", dir))
		}
	}
	return m.replan(j)
}

func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// replan walks every unit's tree. It runs before the server is touched, to
// refuse a bad tree early, and again after saving, so the archive holds
// what the server wrote.
func (m *Manager) replan(j *job) error {
	for i := range j.units {
		p, err := buildPlan(j.units[i].root, j.units[i].name, j.policy)
		if err != nil {
			return fmt.Errorf("%s: %w", j.units[i].root, err)
		}
		j.units[i].entries = p.Entries
		for _, w := range p.Warnings {
			if !contains(j.warnings, w) {
				j.warnings = append(j.warnings, w)
			}
		}
	}
	return nil
}

func newStageDir(base string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(base, stagingPrefix+hex.EncodeToString(b[:]))
	return dir, os.Mkdir(dir, archiveDirPm)
}

// live backs up a running server.
func (m *Manager) live(ctx context.Context, j *job, out Output) (err error) {
	set := j.set
	if j.mode == modeWorlds {
		printf(out.Out, "Server %q is running: its worlds are saved and copied together, but files outside the worlds (plugins, configuration, logs) are not part of this backup. For a consistent backup of those, stop the server and run the complete backup.", set.Name)
	} else {
		printf(out.Out, "Server %q is running: its worlds are saved and copied together, but plugin data, configuration and logs are copied while it runs and may not match each other. Only a backup of a stopped server is consistent throughout.", set.Name)
	}
	startMsg, endMsg := "MESSAGE_WORLD_BACKUP_STARTED", "MESSAGE_WORLD_BACKUP_FINISHED"
	if j.mode == modeComplete {
		startMsg, endMsg = "MESSAGE_COMPLETE_BACKUP_STARTED", "MESSAGE_COMPLETE_BACKUP_FINISHED"
	}
	m.say(ctx, j, set.Get(startMsg), out)

	stageDir, err := newStageDir(j.base)
	if err != nil {
		return fmt.Errorf("create the staging directory: %w", err)
	}
	saving := false // whether this backup may have left saving off
	defer func() {
		if saving {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
			defer cancel()
			if rerr := m.saveOn(rctx, set, out); rerr != nil {
				// The marker stays, so the next backup tries again.
				err = errors.Join(err, fmt.Errorf("%w: %v; the next backup of this server will try again", ErrSavingLeft, rerr))
				os.RemoveAll(stageDir)
				return
			}
		}
		os.RemoveAll(stageDir)
		if err != nil {
			removePending(set.Dir)
		}
	}()

	window, cancel := context.WithTimeout(ctx, m.cfg.SaveWindow)
	defer cancel()
	saving = true
	if err := writePending(set.Dir, pending{Operation: j.mode.String(), Server: set.Name, PID: os.Getpid(), Started: m.cfg.Clock.Now(), SavingOff: true}); err != nil {
		return err
	}
	err = m.cfg.Console.DoLocked(window, set, []console.Step{{Command: profiles.SaveOff, Doing: "Disabling saving...", Done: "Done."}}, cout(out))
	if errors.Is(err, console.ErrRefused) {
		// The server said saving was not changed (on current servers:
		// "already off"), so it is not ours to turn back on.
		saving = false
		return fmt.Errorf("the server refused to disable saving, so it was left as it was: %w", err)
	}
	if err != nil {
		return fmt.Errorf("disable saving before the backup: %w", err)
	}
	if err := m.cfg.Console.DoLocked(window, set, []console.Step{{Command: profiles.SaveAll, Doing: "Saving the worlds...", Done: "Done.", Sync: true}}, cout(out)); err != nil {
		return fmt.Errorf("save the server before the backup: %w", err)
	}
	if m.cfg.SyncRAM != nil {
		if err := m.cfg.SyncRAM(window, set); err != nil {
			return fmt.Errorf("write RAM worlds to disk: %w", err)
		}
	}
	if err := m.replan(j); err != nil {
		return err
	}
	staged := make([][]entry, len(j.units))
	for i, u := range j.units {
		printf(out.Out, "Copying %s...", u.name)
		if staged[i], err = stage(window, m.ops, stageDir, u.entries); err != nil {
			return fmt.Errorf("copy %s while saving was off: %w", u.name, err)
		}
	}
	if err := window.Err(); err != nil {
		return context.Cause(window)
	}
	// Saving goes back on before the slow part.
	if err := m.saveOn(ctx, set, out); err != nil {
		return fmt.Errorf("%w: %v", ErrSavingLeft, err)
	}
	saving = false
	if err := writePending(set.Dir, pending{Operation: j.mode.String(), Server: set.Name, PID: os.Getpid(), Started: m.cfg.Clock.Now()}); err != nil {
		return err
	}
	if err := m.finish(ctx, j, staged, out); err != nil {
		return err
	}
	m.say(ctx, j, set.Get(endMsg), out)
	return nil
}

func (m *Manager) saveOn(ctx context.Context, set *legacyconf.ServerSettings, out Output) error {
	return m.cfg.Console.DoLocked(ctx, set, []console.Step{{Command: profiles.SaveOn, Doing: "Enabling saving...", Done: "Done."}}, cout(out))
}

// say tells the players; failing to is only worth a warning.
func (m *Manager) say(ctx context.Context, j *job, msg string, out Output) {
	if msg == "" {
		return
	}
	err := m.cfg.Console.DoLocked(ctx, j.set, []console.Step{{Command: profiles.Say, Args: map[string]string{"message": msg}}}, console.Output{Out: io.Discard, Err: out.Err})
	if err != nil {
		printf(out.Err, "Warning: could not tell the players: %v", err)
	}
}

// finish compresses each unit and removes the marker. staged is the staged
// copy of each unit's entries, or nil to archive the files in place (the
// server is stopped).
func (m *Manager) finish(ctx context.Context, j *job, staged [][]entry, out Output) error {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()
	set := j.set
	var published []string
	for i, u := range j.units {
		entries := u.entries
		if staged != nil {
			entries = staged[i]
		}
		printf(out.Out, "Compressing %s...", u.name)
		final, err := writeArchive(ctx, m.ops, u.dir, m.cfg.Clock.Now(), entries)
		if err != nil {
			removePending(set.Dir)
			if len(published) > 0 {
				err = fmt.Errorf("%w (already published: %s)", err, strings.Join(published, ", "))
			}
			if ctx.Err() != nil {
				err = fmt.Errorf("backup of %s did not finish: %w", u.name, err)
			}
			return err
		}
		published = append(published, final)
		printf(out.Out, "Wrote %s", final)
	}
	return removePending(set.Dir)
}
