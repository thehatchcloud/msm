package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/process"
	"github.com/thehatchcloud/msm/internal/screen"
	"github.com/thehatchcloud/msm/internal/servers"
)

// Server behaviors the fake screen simulates.
const (
	behaveOK    = "ok"    // logs Done at launch, saves and stops on request
	behaveSlow  = "slow"  // runs but never logs Done
	behaveHang  = "hang"  // logs Done but ignores stop
	behaveEULA  = "eula"  // writes eula.txt=false, logs the EULA notice and exits
	behavePort  = "port"  // logs a port bind failure and exits
	behaveCrash = "crash" // exits at once without a hint
)

const (
	doneLine  = `[12:00:00] [Server thread/INFO]: Done (1.0s)! For help, type "help"`
	savedLine = `[12:00:00] [Server thread/INFO]: Saved the world`
)

type fakeSession struct {
	pid, child int
	name, dir  string
	argv       []string
	behavior   string
	pending    string
	// lsLeft, when positive, is how many more listings show the session
	// before it exits on its own.
	lsLeft int
}

// fakeScreen stands in for GNU screen and the process table below a real
// screen.Backend, and for the Minecraft server inside each session.
type fakeScreen struct {
	mu        sync.Mutex
	uid       int
	nextPID   int
	sessions  map[string]*fakeSession
	behaviors map[string]string // by session name; default behaveOK
	sent      map[string][]string
	launches  map[string]int
	calls     [][]string
}

func newFakeScreen() *fakeScreen {
	return &fakeScreen{uid: os.Geteuid(), nextPID: 100, sessions: map[string]*fakeSession{},
		behaviors: map[string]string{}, sent: map[string][]string{}, launches: map[string]int{}}
}

func appendLog(dir, line string) {
	path := filepath.Join(dir, "logs", "latest.log")
	os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	f.WriteString(line + "\n")
	f.Close()
}

func (f *fakeScreen) Run(ctx context.Context, cmd process.Command) (process.Result, error) {
	if err := ctx.Err(); err != nil {
		return process.Result{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), cmd.Args...))
	args := cmd.Args
	switch {
	case args[0] == "-ls":
		var names []string
		for name, s := range f.sessions {
			if s.lsLeft == 1 {
				delete(f.sessions, name)
				continue
			}
			if s.lsLeft > 1 {
				s.lsLeft--
			}
			names = append(names, name)
		}
		if len(names) == 0 {
			return process.Result{Stdout: "No Sockets found in /tmp/fake.\n\n"}, nil
		}
		sort.Strings(names)
		var b strings.Builder
		b.WriteString("There are screens on:\n")
		for _, n := range names {
			fmt.Fprintf(&b, "\t%d.%s\t(Detached)\n", f.sessions[n].pid, n)
		}
		fmt.Fprintf(&b, "%d Sockets in /tmp/fake.\n", len(names))
		return process.Result{Stdout: b.String()}, errors.New("exit status 1")
	case args[0] == "-dmS":
		name := args[1]
		behavior := f.behaviors[name]
		if behavior == "" {
			behavior = behaveOK
		}
		s := &fakeSession{pid: f.nextPID, child: f.nextPID + 1, name: name, dir: cmd.Dir,
			argv: append([]string(nil), args[2:]...), behavior: behavior}
		f.nextPID += 2
		f.sessions[name] = s
		f.launches[name]++
		switch behavior {
		case behaveOK, behaveHang:
			// Minecraft 1.7+ moves the old latest.log aside at startup.
			os.Remove(filepath.Join(cmd.Dir, "logs", "latest.log"))
			appendLog(cmd.Dir, doneLine)
		case behaveEULA:
			os.WriteFile(filepath.Join(cmd.Dir, "eula.txt"), []byte("eula=false\n"), 0o644)
			appendLog(cmd.Dir, "[12:00:00] [main/INFO]: You need to agree to the EULA in order to run the server. Go to eula.txt for more info.")
			s.lsLeft = 2
		case behavePort:
			appendLog(cmd.Dir, "[12:00:00] [Server thread/WARN]: **** FAILED TO BIND TO PORT!")
			s.lsLeft = 2
		case behaveCrash:
			s.lsLeft = 2
		}
		return process.Result{}, nil
	case len(args) == 7 && args[0] == "-S" && args[4] == "-X" && args[5] == "stuff":
		var s *fakeSession
		for _, c := range f.sessions {
			if fmt.Sprintf("%d.%s", c.pid, c.name) == args[1] {
				s = c
			}
		}
		if s == nil {
			return process.Result{Stdout: "No screen session found.\n"}, errors.New("exit status 1")
		}
		s.pending += args[6]
		if !strings.HasSuffix(s.pending, "\r") {
			return process.Result{}, nil
		}
		line := strings.NewReplacer(`\$`, "$", `\^`, "^", `\\`, `\`).Replace(strings.TrimSuffix(s.pending, "\r"))
		s.pending = ""
		f.sent[s.name] = append(f.sent[s.name], line)
		switch {
		case line == "save-all":
			appendLog(s.dir, savedLine)
		case line == "stop" && s.behavior != behaveHang:
			appendLog(s.dir, "[12:00:00] [Server thread/INFO]: Stopping server")
			delete(f.sessions, s.name)
		}
		return process.Result{}, nil
	}
	return process.Result{}, fmt.Errorf("fake screen: unexpected %q", args)
}

func (f *fakeScreen) Get(pid int) (screen.ProcInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.pid == pid {
			return screen.ProcInfo{PID: pid, UID: f.uid, Argv: []string{"SCREEN"}}, nil
		}
	}
	return screen.ProcInfo{}, screen.ErrNoProcess
}

func (f *fakeScreen) Children(ppid int) ([]screen.ProcInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sessions {
		if s.pid == ppid {
			return []screen.ProcInfo{{PID: s.child, PPID: ppid, UID: f.uid, Argv: s.argv}}, nil
		}
	}
	return nil, nil
}

func (f *fakeScreen) running(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.sessions["msm-"+name]
	return ok
}

func (f *fakeScreen) lines(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent["msm-"+name]...)
}

func (f *fakeScreen) launched(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launches["msm-"+name]
}

// crash ends a session as if the server died.
func (f *fakeScreen) crash(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, "msm-"+name)
}

// autoClock advances virtual time by every wait, so countdowns and
// deadlines run instantly and deterministically.
type autoClock struct {
	mu     sync.Mutex
	now    time.Time
	onWait func(time.Duration)
}

func (c *autoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *autoClock) Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	hook := c.onWait
	c.mu.Unlock()
	if hook != nil {
		hook(d)
	}
	return ctx.Err()
}

type stoppedProber struct{}

func (stoppedProber) Probe(context.Context, *legacyconf.ServerSettings, identity.Identity) servers.State {
	return servers.State{Kind: servers.Stopped}
}

type env struct {
	t       *testing.T
	root    string
	path    string
	servers *servers.Manager
	screen  *fakeScreen
	clock   *autoClock
	noScrn  bool
	timeout time.Duration
	jobs    int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{t: t, root: filepath.Join(base, "servers"), path: filepath.Join(base, "bin"),
		screen: newFakeScreen(), clock: &autoClock{now: time.Unix(1_800_000_000, 0)}}
	for _, d := range []string{e.root, e.path} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(e.path, "java"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	me := identity.Identity{Username: "tester", UID: os.Geteuid(), GID: os.Getegid()}
	euid := me.UID
	m, err := servers.New(servers.Config{Root: e.root, PropertiesFile: "server.properties",
		Owner: me, DefaultOwner: &me, Prober: stoppedProber{}, EUID: &euid})
	if err != nil {
		t.Fatal(err)
	}
	e.servers = m
	return e
}

func (e *env) manager() *Manager {
	e.t.Helper()
	euid := os.Geteuid()
	m, err := New(Config{
		Servers: e.servers, Path: e.path, Clock: e.clock, Timeout: e.timeout, Jobs: e.jobs, EUID: &euid,
		Sessions: func(owner identity.Identity) (Sessions, error) {
			if e.noScrn {
				return nil, ErrNoScreen
			}
			return screen.New(screen.Config{Screen: "/usr/bin/screen", Owner: owner, Env: []string{},
				Runner: e.screen, Terminal: process.ExecRunner{}, Processes: e.screen, Clock: e.clock})
		},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// server creates a server with a valid JAR and the given properties.
func (e *env) server(name, behavior, props string) string {
	e.t.Helper()
	c, err := e.servers.Create(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "server.jar"), []byte("PK\x03\x04jar"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "server.properties"), []byte(props), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.screen.behaviors["msm-"+name] = behavior
	return c.Dir
}

func (e *env) active(name string) bool {
	_, err := os.Lstat(filepath.Join(e.root, name, "active"))
	return err == nil
}

func (e *env) setActive(name string, active bool) {
	path := filepath.Join(e.root, name, "active")
	if active {
		os.WriteFile(path, nil, 0o644)
	} else {
		os.Remove(path)
	}
}

// run starts a server directly in the fake, as if it had been running.
func (e *env) run(name string) {
	e.t.Helper()
	var out strings.Builder
	intent := e.active(name)
	if err := e.manager().Start(context.Background(), name, &out); err != nil {
		e.t.Fatalf("start %s: %v\n%s", name, err, out.String())
	}
	e.setActive(name, intent)
	e.screen.mu.Lock()
	e.screen.sent["msm-"+name] = nil
	e.screen.mu.Unlock()
}
