package maintenance

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/console"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/servers"
	"github.com/thehatchcloud/msm/internal/testutil"
	"github.com/thehatchcloud/msm/internal/worlds"
)

// now is the fixed time of every test.
var now = time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)

type fakeProber struct {
	mu     sync.Mutex
	states map[string]servers.State
}

func (p *fakeProber) Probe(_ context.Context, s *legacyconf.ServerSettings, _ identity.Identity) servers.State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.states[s.Name]
}

type fakeConsole struct {
	running bool
	err     error
}

func (f *fakeConsole) Running(context.Context, *legacyconf.ServerSettings, console.Output) (bool, error) {
	return f.running, f.err
}

type env struct {
	base, root string
	logs       string // LOG_ARCHIVE_PATH
	backups    string // BACKUP_ARCHIVE_PATH
	worlds     string // WORLD_ARCHIVE_PATH
	sm         *servers.Manager
	prober     *fakeProber
	con        *fakeConsole
	clk        *testutil.Clock
	roller     *Roller
	pruner     *Pruner
	out, errw  bytes.Buffer
}

// newEnv builds the managers over a fresh storage root. extra is appended
// to msm.conf; @BASE@ in it is the test's temporary directory.
func newEnv(t *testing.T, extra string) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{base: base, root: filepath.Join(base, "servers"),
		logs: filepath.Join(base, "archives", "logs"), backups: filepath.Join(base, "archives", "backups"),
		worlds: filepath.Join(base, "archives", "worlds"),
		prober: &fakeProber{states: map[string]servers.State{}}, con: &fakeConsole{}, clk: testutil.NewClock(now)}
	if err := os.Mkdir(e.root, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := fmt.Sprintf("SERVER_STORAGE_PATH=%q\nLOG_ARCHIVE_PATH=%q\nBACKUP_ARCHIVE_PATH=%q\nWORLD_ARCHIVE_PATH=%q\n%s\n",
		e.root, e.logs, e.backups, e.worlds, replaceBase(extra, base))
	path := filepath.Join(base, "msm.conf")
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err := legacyconf.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	euid := os.Geteuid()
	owner := identity.Identity{Username: "tester", UID: euid, GID: os.Getegid()}
	e.sm, err = servers.New(servers.Config{Root: e.root, PropertiesFile: "server.properties", Global: global, Owner: owner,
		DefaultOwner: &owner, Prober: e.prober, EUID: &euid, DropTo: func(identity.Identity) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	wm, err := worlds.New(worlds.Config{Servers: e.sm})
	if err != nil {
		t.Fatal(err)
	}
	if e.roller, err = NewRoller(RollConfig{Servers: e.sm, Worlds: wm, Console: e.con, Clock: e.clk}); err != nil {
		t.Fatal(err)
	}
	if e.pruner, err = NewPruner(PruneConfig{Servers: e.sm, Clock: e.clk}); err != nil {
		t.Fatal(err)
	}
	return e
}

func replaceBase(s, base string) string {
	return string(bytes.ReplaceAll([]byte(s), []byte("@BASE@"), []byte(base)))
}

func (e *env) output() Output { return Output{Out: &e.out, Err: &e.errw} }

// server makes a stopped server directory for Minecraft 1.2, whose log is
// server.log; a test of another profile rewrites server.properties.
func (e *env) server(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(e.root, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "server.properties"), "msm-version=minecraft/1.2.0\n")
	e.prober.states[name] = servers.State{Kind: servers.Stopped}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// aged sets a file's modification time to days (and a little) ago.
func aged(t *testing.T, path string, days float64) {
	t.Helper()
	when := now.Add(-time.Duration(days * float64(24*time.Hour)))
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// zipFile writes a finished one-entry ZIP.
func zipFile(t *testing.T, path string, days float64) {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create("world/level.dat")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("data"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, path, b.String())
	aged(t, path, days)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func removeAll(path string) error { return os.RemoveAll(path) }
