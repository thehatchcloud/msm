package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/console"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/profiles"
	"github.com/thehatchcloud/msm/internal/servers"
	"github.com/thehatchcloud/msm/internal/testutil"
	"github.com/thehatchcloud/msm/internal/worlds"
)

// stamp is the file name the fixed test clock gives an archive.
const stamp = "2026-01-02-03-04-05"

type fakeProber struct {
	mu     sync.Mutex
	states map[string]servers.State
}

func (p *fakeProber) Probe(_ context.Context, s *legacyconf.ServerSettings, _ identity.Identity) servers.State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.states[s.Name]
}

// fakeConsole stands in for the server's console. It records every command
// in order and lets a test run code at a command, to play the part of the
// server (or of the world changing after saving resumes).
type fakeConsole struct {
	mu      sync.Mutex
	running bool
	runErr  error
	log     []string
	// fail returns an error for a command, once per entry in failures.
	fail map[string]error
	// hook runs when a command is sent.
	hook func(ctx context.Context, command string)
}

func (f *fakeConsole) Running(context.Context, *legacyconf.ServerSettings, console.Output) (bool, error) {
	return f.running, f.runErr
}

func (f *fakeConsole) DoLocked(ctx context.Context, _ *legacyconf.ServerSettings, steps []console.Step, _ console.Output) error {
	for _, s := range steps {
		f.mu.Lock()
		f.log = append(f.log, s.Command)
		err := f.fail[s.Command]
		hook := f.hook
		f.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if hook != nil {
			hook(ctx, s.Command)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeConsole) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeConsole) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = nil
}

type env struct {
	base, root string
	archives   string
	sm         *servers.Manager
	wm         *worlds.Manager
	bm         *Manager
	con        *fakeConsole
	prober     *fakeProber
	clk        *testutil.Clock
	out, errw  bytes.Buffer
}

// newEnv builds a manager over a fresh storage root. extra is added to
// msm.conf, which always sets the archive directories under the test's
// temporary directory.
func newEnv(t *testing.T, extra string) *env {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "servers")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	archives := filepath.Join(base, "archives")
	// @BASE@ in extra is the test's temporary directory.
	conf := fmt.Sprintf("SERVER_STORAGE_PATH=%q\nWORLD_ARCHIVE_PATH=%q\nBACKUP_ARCHIVE_PATH=%q\n%s\n",
		root, filepath.Join(archives, "worlds"), filepath.Join(archives, "backups"), strings.ReplaceAll(extra, "@BASE@", base))
	path := filepath.Join(base, "msm.conf")
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err := legacyconf.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{base: base, root: root, archives: archives, prober: &fakeProber{states: map[string]servers.State{}},
		con: &fakeConsole{}, clk: testutil.NewClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local))}
	euid := os.Geteuid()
	owner := identity.Identity{Username: "tester", UID: euid, GID: os.Getegid()}
	e.sm, err = servers.New(servers.Config{Root: root, PropertiesFile: "server.properties", Global: global, Owner: owner,
		DefaultOwner: &owner, Prober: e.prober, EUID: &euid, DropTo: func(identity.Identity) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if e.wm, err = worlds.New(worlds.Config{Servers: e.sm}); err != nil {
		t.Fatal(err)
	}
	e.bm, err = New(Config{Servers: e.sm, Worlds: e.wm, Console: e.con, Clock: e.clk, JarRoot: filepath.Join(base, "jars")})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// rebuild replaces the backup manager with one whose configuration mod
// has changed.
func (e *env) rebuild(t *testing.T, mod func(*Config)) {
	t.Helper()
	cfg := Config{Servers: e.sm, Worlds: e.wm, Console: e.con, Clock: e.clk, JarRoot: filepath.Join(e.base, "jars")}
	mod(&cfg)
	var err error
	if e.bm, err = New(cfg); err != nil {
		t.Fatal(err)
	}
}

func (e *env) output() Output {
	e.out.Reset()
	e.errw.Reset()
	return Output{Out: &e.out, Err: &e.errw}
}

func (e *env) create(t *testing.T, name string) string {
	t.Helper()
	c, err := e.sm.Create(context.Background(), name)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return c.Dir
}

func write(t *testing.T, path, data string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// world makes a world directory shaped like Minecraft's.
func world(t *testing.T, storage, name string) string {
	t.Helper()
	dir := filepath.Join(storage, name)
	write(t, filepath.Join(dir, "level.dat"), "level "+name, 0o644)
	write(t, filepath.Join(dir, "region", "r.0.0.mca"), "overworld "+name, 0o640)
	write(t, filepath.Join(dir, "DIM-1", "region", "r.0.0.mca"), "nether "+name, 0o644)
	write(t, filepath.Join(dir, "DIM1", "region", "r.0.0.mca"), "end "+name, 0o644)
	return dir
}

// zipNames lists an archive's entries.
func zipNames(t *testing.T, path string) []string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer r.Close()
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	return names
}

func zipFile(t *testing.T, path, name string) *zip.File {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { r.Close() })
	for _, f := range r.File {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("%s has no entry %q (has %v)", path, name, zipNames(t, path))
	return nil
}

func zipContent(t *testing.T, path, name string) string {
	t.Helper()
	rc, err := zipFile(t, path, name).Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func archives(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// noLeftovers fails if an archive directory holds anything but finished
// archives and the per-world directories.
func noLeftovers(t *testing.T, root string) {
	t.Helper()
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), tmpPrefix) {
			t.Errorf("leftover %s", p)
		}
		return nil
	})
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// CT-CMD-031: a world backup of a stopped server makes one archive per
// active world, laid out as <world>/..., with modes and times kept, and
// skips inactive worlds.
func TestWorldBackupLayout(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	world(t, filepath.Join(dir, "worldstorage"), "beta")
	world(t, filepath.Join(dir, "worldstorage_inactive"), "gamma")
	write(t, filepath.Join(dir, "worldstorage", "alpha", "run.sh"), "#!/bin/sh\n", 0o755)
	mtime := time.Date(2024, 5, 6, 7, 8, 10, 0, time.UTC)
	os.Chtimes(filepath.Join(dir, "worldstorage", "alpha", "level.dat"), mtime, mtime)

	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("Worlds: %v\n%s", err, e.errw.String())
	}
	base := filepath.Join(e.archives, "worlds", "srv")
	alpha := filepath.Join(base, "alpha", stamp+".zip")
	beta := filepath.Join(base, "beta", stamp+".zip")
	for _, p := range []string{alpha, beta} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing archive: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "gamma")); err == nil {
		t.Errorf("an inactive world was backed up")
	}
	want := []string{"alpha/", "alpha/DIM-1/", "alpha/DIM-1/region/", "alpha/DIM-1/region/r.0.0.mca", "alpha/DIM1/", "alpha/DIM1/region/",
		"alpha/DIM1/region/r.0.0.mca", "alpha/level.dat", "alpha/region/", "alpha/region/r.0.0.mca", "alpha/run.sh"}
	if got := zipNames(t, alpha); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("entries\n got %v\nwant %v", got, want)
	}
	if got := zipContent(t, alpha, "alpha/region/r.0.0.mca"); got != "overworld alpha" {
		t.Errorf("content = %q", got)
	}
	for name, mode := range map[string]fs.FileMode{"alpha/run.sh": 0o755, "alpha/region/r.0.0.mca": 0o640, "alpha/level.dat": 0o644, "alpha/": fs.ModeDir | 0o755} {
		if got := zipFile(t, alpha, name).Mode(); got != mode {
			t.Errorf("%s mode = %v, want %v", name, got, mode)
		}
	}
	if got := zipFile(t, alpha, "alpha/level.dat").Modified; !got.Equal(mtime) {
		t.Errorf("modified = %v, want %v", got, mtime)
	}
	if info, _ := os.Stat(alpha); info.Mode().Perm() != 0o600 {
		t.Errorf("archive mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(pendingPath(dir)); err == nil {
		t.Errorf("the pending marker was left behind")
	}
	noLeftovers(t, e.archives)
	if cmds := e.con.commands(); len(cmds) != 0 {
		t.Errorf("a stopped server was sent %v", cmds)
	}
	if !strings.Contains(e.out.String(), "stopped") {
		t.Errorf("output does not say the server was stopped:\n%s", e.out.String())
	}
}

// A symbolic link in a world is stored as a link: its target, which may be
// outside the world, is not captured, and a link back into the world does
// not make a cycle.
func TestWorldBackupStoresLinks(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	w := world(t, filepath.Join(dir, "worldstorage"), "alpha")
	secret := filepath.Join(e.base, "secret.txt")
	write(t, secret, "do not archive", 0o600)
	os.Symlink(secret, filepath.Join(w, "leak"))
	os.Symlink(".", filepath.Join(w, "loop"))
	os.Symlink("level.dat", filepath.Join(w, "rel"))

	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("Worlds: %v", err)
	}
	a := filepath.Join(e.archives, "worlds", "srv", "alpha", stamp+".zip")
	f := zipFile(t, a, "alpha/leak")
	if f.Mode()&fs.ModeSymlink == 0 || zipContent(t, a, "alpha/leak") != secret {
		t.Errorf("leak is %v %q, want a link to %s", f.Mode(), zipContent(t, a, "alpha/leak"), secret)
	}
	if zipContent(t, a, "alpha/loop") != "." || zipContent(t, a, "alpha/rel") != "level.dat" {
		t.Errorf("relative links not kept")
	}
	raw, _ := os.ReadFile(a)
	if bytes.Contains(raw, []byte("do not archive")) {
		t.Errorf("the link's target was captured")
	}
}

// CT-CMD-035: a complete backup holds the server directory including
// inactive worlds, without its lock and marker files, and does not include
// its own archive directory even when that is inside the server.
func TestCompleteBackupLayoutAndSelfExclusion(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	// Put the archives inside the server directory.
	conf := fmt.Sprintf("SERVER_STORAGE_PATH=%q\nBACKUP_ARCHIVE_PATH=%q\n", e.root, filepath.Join(dir, "backups"))
	e2 := rebuild(t, e, conf)
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	world(t, filepath.Join(dir, "worldstorage_inactive"), "gamma")
	write(t, filepath.Join(dir, "plugins", "p.jar"), "jar", 0o644)
	write(t, filepath.Join(dir, "server.properties"), "level-name=alpha\n", 0o600)

	for i := 0; i < 2; i++ {
		e2.clk.Advance(time.Second)
		if err := e2.bm.Server(context.Background(), "srv", e2.output()); err != nil {
			t.Fatalf("Server #%d: %v\n%s", i, err, e2.errw.String())
		}
	}
	list, _ := filepath.Glob(filepath.Join(dir, "backups", "srv", "*.zip"))
	sort.Strings(list)
	if len(list) != 2 {
		t.Fatalf("archives = %v", list)
	}
	names := zipNames(t, list[1])
	for _, want := range []string{"srv/", "srv/worldstorage_inactive/gamma/level.dat", "srv/worldstorage/alpha/level.dat", "srv/plugins/p.jar", "srv/server.properties"} {
		if !has(names, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, n := range names {
		if (strings.Contains(n, "backups") && n != "srv/backups/") || strings.Contains(n, ".zip") || strings.Contains(n, pendingName) || strings.Contains(n, ".msm.lock") {
			t.Errorf("archive includes %s", n)
		}
	}
	if got := zipFile(t, list[1], "srv/server.properties").Mode(); got != 0o600 {
		t.Errorf("server.properties mode = %v", got)
	}
	noLeftovers(t, dir)
}

// rebuild returns an environment over the same storage with a different
// msm.conf.
func rebuild(t *testing.T, old *env, conf string) *env {
	t.Helper()
	path := filepath.Join(old.base, "msm2.conf")
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err := legacyconf.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e := *old
	euid := os.Geteuid()
	owner := identity.Identity{Username: "tester", UID: euid, GID: os.Getegid()}
	e.sm, err = servers.New(servers.Config{Root: old.root, PropertiesFile: "server.properties", Global: global, Owner: owner,
		DefaultOwner: &owner, Prober: old.prober, EUID: &euid, DropTo: func(identity.Identity) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	e.wm, _ = worlds.New(worlds.Config{Servers: e.sm})
	e.bm, err = New(Config{Servers: e.sm, Worlds: e.wm, Console: e.con, Clock: e.clk, JarRoot: filepath.Join(old.base, "jars")})
	if err != nil {
		t.Fatal(err)
	}
	return &e
}

// DEV-011: without COMPLETE_BACKUP_FOLLOW_SYMLINKS, links are stored as
// links.
func TestCompleteBackupStoresLinksByDefault(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	outside := filepath.Join(e.base, "outside.txt")
	write(t, outside, "outside data", 0o644)
	os.Symlink(outside, filepath.Join(dir, "out"))
	os.Symlink("worldstorage", filepath.Join(dir, "ws"))
	if err := e.bm.Server(context.Background(), "srv", e.output()); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(e.archives, "backups", "srv", stamp+".zip")
	if zipFile(t, a, "srv/out").Mode()&fs.ModeSymlink == 0 || zipContent(t, a, "srv/out") != outside {
		t.Errorf("out is not stored as a link")
	}
	raw, _ := os.ReadFile(a)
	if bytes.Contains(raw, []byte("outside data")) {
		t.Errorf("target captured")
	}
}

func followEnv(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte("msm-complete-backup-follow-symlinks=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return e, dir
}

// DEV-011: following links captures targets inside the server and the JAR
// store, refuses targets outside, and refuses a cycle.
func TestFollowSymlinks(t *testing.T) {
	t.Run("inside and jar store are followed", func(t *testing.T) {
		e, dir := followEnv(t)
		write(t, filepath.Join(dir, "real", "data.txt"), "inside", 0o644)
		os.Symlink("real", filepath.Join(dir, "alias"))
		write(t, filepath.Join(e.base, "jars", "minecraft", "s.jar"), "jar bytes", 0o644)
		os.Symlink(filepath.Join(e.base, "jars", "minecraft", "s.jar"), filepath.Join(dir, "server.jar"))
		if err := e.bm.Server(context.Background(), "srv", e.output()); err != nil {
			t.Fatalf("%v", err)
		}
		a := filepath.Join(e.archives, "backups", "srv", stamp+".zip")
		if zipContent(t, a, "srv/alias/data.txt") != "inside" || zipContent(t, a, "srv/server.jar") != "jar bytes" {
			t.Errorf("followed links not captured")
		}
		if zipFile(t, a, "srv/server.jar").Mode()&fs.ModeSymlink != 0 {
			t.Errorf("a followed link was stored as a link")
		}
	})
	t.Run("outside target refused", func(t *testing.T) {
		e, dir := followEnv(t)
		outside := filepath.Join(e.base, "private")
		write(t, filepath.Join(outside, "key"), "secret", 0o600)
		os.Symlink(outside, filepath.Join(dir, "leak"))
		err := e.bm.Server(context.Background(), "srv", e.output())
		if !errors.Is(err, ErrExternalLinks) || !strings.Contains(err.Error(), outside) {
			t.Fatalf("err = %v, want ErrExternalLinks naming %s", err, outside)
		}
		if got := archives(t, filepath.Join(e.archives, "backups", "srv")); len(got) != 0 {
			t.Errorf("archive published: %v", got)
		}
		if _, err := os.Stat(pendingPath(dir)); err == nil {
			t.Errorf("marker left")
		}
	})
	t.Run("outside target allowed after review", func(t *testing.T) {
		e, dir := followEnv(t)
		e.bm.cfg.AllowExternal = true
		outside := filepath.Join(e.base, "shared.txt")
		write(t, outside, "shared", 0o644)
		os.Symlink(outside, filepath.Join(dir, "shared"))
		if err := e.bm.Server(context.Background(), "srv", e.output()); err != nil {
			t.Fatal(err)
		}
		if zipContent(t, filepath.Join(e.archives, "backups", "srv", stamp+".zip"), "srv/shared") != "shared" {
			t.Errorf("not captured")
		}
	})
	t.Run("cycle refused", func(t *testing.T) {
		e, dir := followEnv(t)
		write(t, filepath.Join(dir, "a", "f"), "x", 0o644)
		os.Symlink("..", filepath.Join(dir, "a", "up"))
		err := e.bm.Server(context.Background(), "srv", e.output())
		if !errors.Is(err, ErrLinkCycle) {
			t.Fatalf("err = %v, want ErrLinkCycle", err)
		}
		if got := archives(t, filepath.Join(e.archives, "backups", "srv")); len(got) != 0 {
			t.Errorf("archive published")
		}
	})
	t.Run("link to the archive directory is not archived", func(t *testing.T) {
		e, dir := followEnv(t)
		os.MkdirAll(filepath.Join(e.archives, "backups", "srv"), 0o755)
		os.Symlink(filepath.Join(e.archives, "backups", "srv"), filepath.Join(dir, "old-backups"))
		e.bm.cfg.AllowExternal = true
		if err := e.bm.Server(context.Background(), "srv", e.output()); err != nil {
			t.Fatal(err)
		}
		for _, n := range zipNames(t, filepath.Join(e.archives, "backups", "srv", stamp+".zip")) {
			if strings.Contains(n, "old-backups/") {
				t.Errorf("archive includes %s", n)
			}
		}
	})
}

// CT-CMD-031: a live backup saves, copies a stable image, resumes saving
// and only then compresses; the archive holds what was saved, not what the
// running server wrote afterwards.
func TestLiveWorldBackupSequence(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	w := world(t, filepath.Join(dir, "worldstorage"), "alpha")
	base := filepath.Join(e.archives, "worlds", "srv")
	e.con.running = true
	e.con.hook = func(_ context.Context, c string) {
		switch c {
		case profiles.SaveAll:
			write(t, filepath.Join(w, "level.dat"), "saved state", 0o644)
		case profiles.SaveOn:
			if got := archives(t, filepath.Join(base, "alpha")); len(got) != 0 {
				t.Errorf("an archive existed before saving resumed: %v", got)
			}
			if m, err := os.ReadFile(pendingPath(dir)); err != nil || !strings.Contains(string(m), `"saving_off":true`) {
				t.Errorf("marker during save-off = %q, %v", m, err)
			}
			// The server goes on writing once saving is back on.
			write(t, filepath.Join(w, "level.dat"), "written after resume", 0o644)
		}
	}
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("Worlds: %v\n%s", err, e.errw.String())
	}
	cmds := e.con.commands()
	want := []string{profiles.Say, profiles.SaveOff, profiles.SaveAll, profiles.SaveOn, profiles.Say}
	if strings.Join(cmds, ",") != strings.Join(want, ",") {
		t.Errorf("commands = %v, want %v", cmds, want)
	}
	if got := zipContent(t, filepath.Join(base, "alpha", stamp+".zip"), "alpha/level.dat"); got != "saved state" {
		t.Errorf("archived level.dat = %q", got)
	}
	if _, err := os.Stat(pendingPath(dir)); err == nil {
		t.Errorf("marker left")
	}
	noLeftovers(t, e.archives)
	if !strings.Contains(e.out.String(), "plugins, configuration, logs") {
		t.Errorf("live limits not stated:\n%s", e.out.String())
	}
}

// A server that refuses save-off (it is already off) is left as it was: the
// backup stops and does not turn saving on.
func TestSaveOffRefusedIsLeftAlone(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	e.con.running = true
	e.con.fail = map[string]error{profiles.SaveOff: console.ErrRefused}
	err := e.bm.Worlds(context.Background(), "srv", e.output())
	if !errors.Is(err, console.ErrRefused) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range e.con.commands() {
		if c == profiles.SaveOn {
			t.Errorf("saving was turned on: %v", e.con.commands())
		}
	}
	if _, err := os.Stat(pendingPath(dir)); err == nil {
		t.Errorf("marker left")
	}
}

func TestLiveCompleteBackupStatesLimits(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	e.con.running = true
	if err := e.bm.Server(context.Background(), "srv", e.output()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "Only a backup of a stopped server is consistent") {
		t.Errorf("limits not stated:\n%s", e.out.String())
	}
	got := e.con.commands()
	if got[1] != profiles.SaveOff || got[len(got)-2] != profiles.SaveOn {
		t.Errorf("commands = %v", got)
	}
}

// Saving is turned back on, and nothing is published, when any step fails
// or the operation is canceled.
func TestSavingReenabledOnFailure(t *testing.T) {
	cases := []struct {
		name string
		prep func(t *testing.T, e *env, dir string, cancel context.CancelFunc)
		want error
	}{
		{"save-all refused", func(t *testing.T, e *env, _ string, _ context.CancelFunc) {
			e.con.fail = map[string]error{profiles.SaveAll: console.ErrRefused}
		}, console.ErrRefused},
		{"save-off unanswered", func(t *testing.T, e *env, _ string, _ context.CancelFunc) {
			e.con.fail = map[string]error{profiles.SaveOff: console.ErrUnconfirmed}
		}, console.ErrUnconfirmed},
		{"canceled while saving", func(t *testing.T, e *env, _ string, cancel context.CancelFunc) {
			e.con.hook = func(_ context.Context, c string) {
				if c == profiles.SaveAll {
					cancel()
				}
			}
			e.con.fail = map[string]error{profiles.SaveAll: context.Canceled}
		}, context.Canceled},
		{"copy fails", func(t *testing.T, e *env, dir string, _ context.CancelFunc) {
			e.bm.ops = faultOps{open: func(p string) (io.ReadCloser, error) {
				if strings.HasSuffix(p, "r.0.0.mca") {
					return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EACCES}
				}
				return os.Open(p)
			}}
		}, fs.ErrPermission},
		{"canceled while copying", func(t *testing.T, e *env, _ string, cancel context.CancelFunc) {
			e.bm.ops = faultOps{open: func(p string) (io.ReadCloser, error) {
				cancel()
				return os.Open(p)
			}}
		}, context.Canceled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, "")
			dir := e.create(t, "srv")
			world(t, filepath.Join(dir, "worldstorage"), "alpha")
			e.con.running = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.prep(t, e, dir, cancel)
			// Saving back on must work even though ctx is canceled.
			saveOnFail := e.con.fail
			e.con.fail = map[string]error{}
			for k, v := range saveOnFail {
				e.con.fail[k] = v
			}
			err := e.bm.Worlds(ctx, "srv", e.output())
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			cmds := e.con.commands()
			if cmds[len(cmds)-1] != profiles.SaveOn {
				t.Errorf("commands = %v; saving was not re-enabled last", cmds)
			}
			if got := archives(t, filepath.Join(e.archives, "worlds", "srv", "alpha")); len(got) != 0 {
				t.Errorf("archive published: %v", got)
			}
			if _, err := os.Stat(pendingPath(dir)); err == nil {
				t.Errorf("marker left after saving was restored")
			}
			noLeftovers(t, e.archives)
		})
	}
}

// Saving that cannot be turned back on leaves the marker, and the next
// backup (here after the manager "died") turns it on before anything else
// and clears what the dead operation left.
func TestRecoveryAfterKilledManager(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	e.con.running = true
	e.con.fail = map[string]error{profiles.SaveOn: console.ErrUnconfirmed}
	err := e.bm.Worlds(context.Background(), "srv", e.output())
	if !errors.Is(err, ErrSavingLeft) {
		t.Fatalf("err = %v, want ErrSavingLeft", err)
	}
	p, found, rerr := readPending(dir)
	if rerr != nil || !found || !p.SavingOff {
		t.Fatalf("marker = %+v, found=%v, %v", p, found, rerr)
	}

	// A killed manager also leaves its partial files and staging directory.
	archDir := filepath.Join(e.archives, "worlds", "srv")
	write(t, filepath.Join(archDir, "alpha", tmpPrefix+"deadbeef"+tmpSuffix), "half", 0o600)
	write(t, filepath.Join(archDir, stagingPrefix+"cafe", "alpha", "level.dat"), "copy", 0o600)

	e.con.fail = nil
	e.con.reset()
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if cmds := e.con.commands(); cmds[0] != profiles.SaveOn {
		t.Errorf("commands = %v; recovery did not start with save-on", cmds)
	}
	if !strings.Contains(e.errw.String(), "interrupted") {
		t.Errorf("recovery not reported:\n%s", e.errw.String())
	}
	noLeftovers(t, e.archives)
	if _, err := os.Stat(pendingPath(dir)); err == nil {
		t.Errorf("marker left")
	}
	if len(archives(t, filepath.Join(archDir, "alpha"))) != 1 {
		t.Errorf("no archive after recovery")
	}
}

// A marker nobody can read is treated as saving left off.
func TestRecoveryWithUnreadableMarker(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	write(t, pendingPath(dir), "{not json", 0o600)
	e.con.running = true
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatal(err)
	}
	if cmds := e.con.commands(); cmds[0] != profiles.SaveOn {
		t.Errorf("commands = %v", cmds)
	}
}

// A marker's contents never choose what recovery deletes.
func TestRecoveryIgnoresMarkerPaths(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	victim := filepath.Join(e.base, "victim")
	write(t, filepath.Join(victim, "keep"), "keep", 0o644)
	write(t, pendingPath(dir), fmt.Sprintf(`{"version":1,"operation":"worlds","staging":%q,"archive_dirs":[%q],"saving_off":false}`, victim, victim), 0o600)
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Errorf("recovery removed %s", victim)
	}
}

// faultOps injects the failures a real disk and real permissions produce.
type faultOps struct {
	create func(dir string) (sink, string, error)
	open   func(path string) (io.ReadCloser, error)
	link   func(oldname, newname string) error
}

func (f faultOps) Link(oldname, newname string) error {
	if f.link != nil {
		return f.link(oldname, newname)
	}
	return os.Link(oldname, newname)
}

func (f faultOps) CreateTemp(dir string) (sink, string, error) {
	if f.create != nil {
		return f.create(dir)
	}
	return osOps{}.CreateTemp(dir)
}

func (f faultOps) Open(path string) (io.ReadCloser, error) {
	if f.open != nil {
		return f.open(path)
	}
	return os.Open(path)
}

// limitedSink fails with ENOSPC once limit bytes were written.
type limitedSink struct {
	sink
	limit, n int
}

func (l *limitedSink) Write(p []byte) (int, error) {
	if l.n+len(p) > l.limit {
		room := l.limit - l.n
		if room > 0 {
			l.sink.Write(p[:room])
		}
		l.n = l.limit
		return max(room, 0), syscall.ENOSPC
	}
	l.n += len(p)
	return l.sink.Write(p)
}

func bigWorld(t *testing.T, e *env, dir string) {
	t.Helper()
	w := world(t, filepath.Join(dir, "worldstorage"), "alpha")
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(data) // incompressible, so the disk limit is reached
	if err := os.WriteFile(filepath.Join(w, "region", "big.mca"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// CT-CMD-031 failure modes: a full disk, a permission error, a timeout and
// cancellation never publish an archive, and leave no partial file.
func TestIncompleteArchiveNeverPublished(t *testing.T) {
	cases := []struct {
		name string
		ops  func(cancel context.CancelFunc) faultOps
		cfg  func(*Manager)
		want error
	}{
		{"disk full", func(context.CancelFunc) faultOps {
			return faultOps{create: func(dir string) (sink, string, error) {
				s, p, err := osOps{}.CreateTemp(dir)
				return &limitedSink{sink: s, limit: 100 << 10}, p, err
			}}
		}, nil, syscall.ENOSPC},
		{"unreadable file", func(context.CancelFunc) faultOps {
			return faultOps{open: func(p string) (io.ReadCloser, error) {
				if strings.HasSuffix(p, "big.mca") {
					return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EACCES}
				}
				return os.Open(p)
			}}
		}, nil, fs.ErrPermission},
		{"canceled", func(cancel context.CancelFunc) faultOps {
			return faultOps{open: func(p string) (io.ReadCloser, error) {
				if strings.HasSuffix(p, "big.mca") {
					cancel()
				}
				return os.Open(p)
			}}
		}, nil, context.Canceled},
		{"timeout", func(context.CancelFunc) faultOps {
			return faultOps{open: func(p string) (io.ReadCloser, error) {
				if strings.HasSuffix(p, "big.mca") {
					time.Sleep(20 * time.Millisecond)
				}
				return os.Open(p)
			}}
		}, func(m *Manager) { m.cfg.Timeout = 5 * time.Millisecond }, context.DeadlineExceeded},
	}
	for _, mode := range []string{"stopped", "live"} {
		for _, c := range cases {
			t.Run(mode+"/"+c.name, func(t *testing.T) {
				e := newEnv(t, "")
				dir := e.create(t, "srv")
				bigWorld(t, e, dir)
				e.con.running = mode == "live"
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				e.bm.ops = c.ops(cancel)
				if c.cfg != nil {
					c.cfg(e.bm)
				}
				err := e.bm.Worlds(ctx, "srv", e.output())
				if !errors.Is(err, c.want) {
					t.Fatalf("err = %v, want %v", err, c.want)
				}
				if got := archives(t, filepath.Join(e.archives, "worlds", "srv", "alpha")); len(got) != 0 {
					t.Errorf("archive published: %v", got)
				}
				noLeftovers(t, e.archives)
				if _, err := os.Stat(pendingPath(dir)); err == nil {
					t.Errorf("marker left")
				}
				if mode == "live" {
					cmds := e.con.commands()
					if cmds[len(cmds)-1] != profiles.SaveOn && cmds[len(cmds)-2] != profiles.SaveOn {
						t.Errorf("commands = %v", cmds)
					}
				}
			})
		}
	}
}

// A failed second world does not unpublish nor hide the first, and says so.
func TestPartialSetIsReported(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	world(t, filepath.Join(dir, "worldstorage"), "beta")
	e.bm.ops = faultOps{open: func(p string) (io.ReadCloser, error) {
		if strings.Contains(p, "beta") {
			return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EACCES}
		}
		return os.Open(p)
	}}
	err := e.bm.Worlds(context.Background(), "srv", e.output())
	if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "already published") {
		t.Fatalf("err = %v", err)
	}
	if len(archives(t, filepath.Join(e.archives, "worlds", "srv", "alpha"))) != 1 {
		t.Errorf("the finished archive was removed")
	}
	if len(archives(t, filepath.Join(e.archives, "worlds", "srv", "beta"))) != 0 {
		t.Errorf("the failed archive was published")
	}
}

// An archive that already exists is never replaced.
func TestExistingArchiveIsKept(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	existing := filepath.Join(e.archives, "worlds", "srv", "alpha", stamp+".zip")
	write(t, existing, "older archive", 0o600)
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "older archive" {
		t.Errorf("existing archive replaced")
	}
	if zipContent(t, filepath.Join(filepath.Dir(existing), stamp+"-1.zip"), "alpha/level.dat") != "level alpha" {
		t.Errorf("new archive not written beside it")
	}
}

func TestRefusals(t *testing.T) {
	t.Run("no method enabled", func(t *testing.T) {
		e := newEnv(t, "WORLD_ARCHIVE_ENABLED=false")
		dir := e.create(t, "srv")
		world(t, filepath.Join(dir, "worldstorage"), "alpha")
		if err := e.bm.Worlds(context.Background(), "srv", e.output()); !errors.Is(err, ErrDisabled) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no active worlds", func(t *testing.T) {
		e := newEnv(t, "")
		dir := e.create(t, "srv")
		world(t, filepath.Join(dir, "worldstorage_inactive"), "gamma")
		e.con.running = true
		if err := e.bm.Worlds(context.Background(), "srv", e.output()); !errors.Is(err, ErrNoWorlds) {
			t.Errorf("err = %v", err)
		}
		if len(e.con.commands()) != 0 {
			t.Errorf("the server was touched: %v", e.con.commands())
		}
	})
	t.Run("state cannot be proven", func(t *testing.T) {
		e := newEnv(t, "")
		dir := e.create(t, "srv")
		world(t, filepath.Join(dir, "worldstorage"), "alpha")
		e.prober.states["srv"] = servers.State{Kind: servers.Unknown}
		err := e.bm.Worlds(context.Background(), "srv", e.output())
		if !errors.Is(err, ErrNotProven) {
			t.Errorf("err = %v", err)
		}
		if _, statErr := os.Stat(pendingPath(dir)); statErr == nil {
			t.Errorf("marker written")
		}
		runErr := errors.New("screen exploded")
		e.con.runErr = runErr
		if err := e.bm.Worlds(context.Background(), "srv", e.output()); !errors.Is(err, runErr) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("relative archive path", func(t *testing.T) {
		e := newEnv(t, "WORLD_ARCHIVE_PATH=archives")
		dir := e.create(t, "srv")
		world(t, filepath.Join(dir, "worldstorage"), "alpha")
		if err := e.bm.Worlds(context.Background(), "srv", e.output()); !errors.Is(err, ErrSettings) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no such server", func(t *testing.T) {
		e := newEnv(t, "")
		if err := e.bm.Worlds(context.Background(), "nope", e.output()); !errors.Is(err, servers.ErrNotFound) {
			t.Errorf("err = %v", err)
		}
	})
}

// A world that lives in RAM is only backed up after its RAM copy is
// written to disk; without a way to do that the backup refuses.
func TestRAMWorlds(t *testing.T) {
	setup := func(t *testing.T) (*env, string) {
		e := newEnv(t, "")
		ram := filepath.Join(e.base, "ram")
		e = rebuild(t, e, fmt.Sprintf("SERVER_STORAGE_PATH=%q\nWORLD_ARCHIVE_PATH=%q\nRAMDISK_STORAGE_ENABLED=true\nRAMDISK_STORAGE_PATH=%q\n",
			e.root, filepath.Join(e.archives, "worlds"), ram))
		dir := e.create(t, "srv")
		w := world(t, filepath.Join(dir, "worldstorage"), "alpha")
		write(t, filepath.Join(w, "inram"), "", 0o644)
		world(t, ram, "srv/alpha")
		return e, dir
	}
	t.Run("refused live without a sync", func(t *testing.T) {
		e, _ := setup(t)
		e.con.running = true
		if err := e.bm.Worlds(context.Background(), "srv", e.output()); !errors.Is(err, ErrRAMWorld) {
			t.Fatalf("err = %v", err)
		}
		if len(e.con.commands()) != 0 {
			t.Errorf("the server was touched")
		}
	})
	t.Run("synced after save-all", func(t *testing.T) {
		e, _ := setup(t)
		e.con.running = true
		var order []string
		e.con.hook = func(_ context.Context, c string) { order = append(order, c) }
		e.bm.cfg.SyncRAM = func(context.Context, *legacyconf.ServerSettings) error {
			order = append(order, "ram-sync")
			return nil
		}
		if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(order, ","); !strings.Contains(got, "SAVE_ALL,ram-sync,SAVE_ON") {
			t.Errorf("order = %s", got)
		}
	})
}

// Backing up all servers continues past a failing one and reports it.
func TestForAll(t *testing.T) {
	e := newEnv(t, "")
	for _, n := range []string{"one", "two"} {
		dir := e.create(t, n)
		if n == "one" {
			world(t, filepath.Join(dir, "worldstorage"), "alpha")
		}
	}
	err := e.bm.ForAll(context.Background(), e.output(), func(name string, o Output) error { return e.bm.Worlds(context.Background(), name, o) })
	if !errors.Is(err, ErrNoWorlds) && (err == nil || !strings.Contains(err.Error(), "two")) {
		t.Fatalf("err = %v", err)
	}
	if len(archives(t, filepath.Join(e.archives, "worlds", "one", "alpha"))) != 1 {
		t.Errorf("the working server was not backed up")
	}
}
