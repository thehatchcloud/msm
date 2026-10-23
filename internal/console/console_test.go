package console

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/process"
	"github.com/thehatchcloud/msm/internal/profiles"
	"github.com/thehatchcloud/msm/internal/screen"
	"github.com/thehatchcloud/msm/internal/servers"
)

const modernTag = "[12:00:00] [Server thread/INFO]: "

// fakeSessions stands in for GNU screen and for the server inside it. A
// server answers each console line through its responder, which appends to
// the server's log as Minecraft would.
type fakeSessions struct {
	mu        sync.Mutex
	running   map[string]string // session name -> server dir
	responder func(dir, line string)
	sent      []string
	attached  []string
	sendErr   error
}

func (f *fakeSessions) Status(_ context.Context, name string, _ []string) (screen.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.running[name]; !ok {
		return screen.Status{Name: name}, nil
	}
	return screen.Status{Name: name, Liveness: screen.Running, Session: &screen.Session{ID: "1." + name, PID: 1, Name: name}}, nil
}

func (f *fakeSessions) Send(_ context.Context, st screen.Status, line string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	if err := screen.ValidateInput(line); err != nil {
		return err
	}
	f.sent = append(f.sent, line)
	if f.responder != nil {
		f.responder(f.running[st.Name], line)
	}
	return nil
}

func (f *fakeSessions) Attach(_ context.Context, st screen.Status, _ process.Stdio, term string) error {
	f.attached = append(f.attached, st.Name+" "+term)
	return nil
}

func (f *fakeSessions) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func appendLog(dir, logPath, text string) {
	path := filepath.Join(dir, logPath)
	os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	f.WriteString(text + "\n")
	f.Close()
}

// autoClock advances virtual time by every wait, so deadlines run instantly.
type autoClock struct {
	mu     sync.Mutex
	now    time.Time
	onWait func()
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
		hook()
	}
	return ctx.Err()
}

type stoppedProber struct{}

func (stoppedProber) Probe(context.Context, *legacyconf.ServerSettings, identity.Identity) servers.State {
	return servers.State{Kind: servers.Stopped}
}

type env struct {
	t        *testing.T
	root     string
	servers  *servers.Manager
	sessions *fakeSessions
	clock    *autoClock
	noScreen bool
	timeout  time.Duration
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := filepath.Join(t.TempDir(), "servers")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	me := identity.Identity{Username: "tester", UID: os.Geteuid(), GID: os.Getegid()}
	euid := me.UID
	m, err := servers.New(servers.Config{Root: root, PropertiesFile: "server.properties",
		Owner: me, DefaultOwner: &me, Prober: stoppedProber{}, EUID: &euid})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, root: root, servers: m, sessions: &fakeSessions{running: map[string]string{}},
		clock: &autoClock{now: time.Unix(1_800_000_000, 0)}}
}

// server creates a server with the given properties and, if run, a running
// session.
func (e *env) server(name, props string, run bool) string {
	e.t.Helper()
	c, err := e.servers.Create(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "server.properties"), []byte(props), 0o644); err != nil {
		e.t.Fatal(err)
	}
	if run {
		e.sessions.running["msm-"+name] = c.Dir
	}
	return c.Dir
}

func (e *env) manager() *Manager {
	e.t.Helper()
	m, err := New(Config{Servers: e.servers, Clock: e.clock, Timeout: e.timeout,
		Sessions: func(identity.Identity) (Sessions, error) {
			if e.noScreen {
				return nil, ErrNoScreen
			}
			return e.sessions, nil
		}})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) do(name string, steps ...Step) (string, string, error) {
	e.t.Helper()
	var out, errOut bytes.Buffer
	err := e.manager().Do(context.Background(), name, steps, Output{Out: &out, Err: &errOut})
	return out.String(), errOut.String(), err
}

// modernResponder answers like a 1.21 vanilla server.
func modernResponder(dir, line string) {
	say := func(s string) { appendLog(dir, "logs/latest.log", modernTag+s) }
	f := strings.Fields(line)
	switch {
	case line == "whitelist on":
		say("Whitelist is now turned on")
	case len(f) == 3 && f[0] == "whitelist" && f[1] == "add" && f[2] == "Ghost":
		say("That player does not exist")
	case len(f) == 3 && f[0] == "whitelist" && f[1] == "add":
		say("Added " + f[2] + " to the whitelist")
	case line == "save-all":
		say("Saving the game (this may take a moment!)")
		say("Saved the game")
	case line == "list":
		say("There are 2 of a max of 20 players online: Steve, Alex")
	}
}

const modernProps = "msm-version=minecraft/1.21.0\nmsm-log-path=logs/latest.log\n"

func TestDoConfirmsFromFreshLogLines(t *testing.T) {
	e := newEnv(t)
	dir := e.server("alpha", modernProps, true)
	e.sessions.responder = modernResponder
	// An old answer in the log must not confirm the new command.
	appendLog(dir, "logs/latest.log", modernTag+"Added Steve to the whitelist")
	out, _, err := e.do("alpha", Step{Command: profiles.WhitelistAdd, Args: map[string]string{"player": "Steve"}, Done: "Player Steve is now whitelisted."})
	if err != nil || out != "Player Steve is now whitelisted.\n" {
		t.Fatalf("out %q err %v", out, err)
	}

	// Silent server: the stale line does not count, and the wait ends.
	e.sessions.responder = nil
	_, _, err = e.do("alpha", Step{Command: profiles.WhitelistAdd, Args: map[string]string{"player": "Steve"}, Done: "x"})
	if !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("stale line confirmed a new command: %v", err)
	}
}

func TestDoRefusalIsAnError(t *testing.T) {
	e := newEnv(t)
	e.server("alpha", modernProps, true)
	e.sessions.responder = modernResponder
	out, _, err := e.do("alpha",
		Step{Command: profiles.WhitelistAdd, Args: map[string]string{"player": "Ghost"}, Done: "Player Ghost is now whitelisted."},
		Step{Command: profiles.WhitelistAdd, Args: map[string]string{"player": "Alex"}, Done: "Player Alex is now whitelisted."})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "That player does not exist") {
		t.Fatalf("err %v", err)
	}
	// A refusal does not stop the later steps, and no success is claimed for it.
	if out != "Player Alex is now whitelisted.\n" {
		t.Fatalf("out %q", out)
	}
}

func TestDoUnconfirmedStopsLaterSteps(t *testing.T) {
	e := newEnv(t)
	e.server("alpha", modernProps, true)
	_, _, err := e.do("alpha",
		Step{Command: profiles.WhitelistOn, Done: "Whitelist enabled."},
		Step{Command: profiles.WhitelistAdd, Args: map[string]string{"player": "Alex"}, Done: "Player Alex is now whitelisted."})
	if !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("err %v", err)
	}
	if got := e.sessions.lines(); len(got) != 1 {
		t.Fatalf("sent %q; a late answer could be taken for the next step's", got)
	}
}

func TestChatCannotForgeAnAnswer(t *testing.T) {
	e := newEnv(t)
	dir := e.server("alpha", modernProps, true)
	e.sessions.responder = func(dir, line string) {
		appendLog(dir, "logs/latest.log", modernTag+"<mallory> x]: Saved the game")
		appendLog(dir, "logs/latest.log", modernTag+"<mallory> Saved the game")
		appendLog(dir, "logs/latest.log", "[12:00:00] [Server thread/INFO]: [Rcon] Saved the game")
	}
	_, _, err := e.do("alpha", Step{Command: profiles.SaveAll, Done: "ok"})
	if !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("a chat line confirmed the save: %v", err)
	}
	_ = dir
}

func TestLegacySendOnlyAndFailureOnly(t *testing.T) {
	e := newEnv(t)
	e.server("old", "msm-version=minecraft/1.3.0\nmsm-log-path=server.log\n", true)
	e.sessions.responder = func(dir, line string) {}
	// say has no answer in the profile: reported as sent, not as done.
	out, _, err := e.do("old", Step{Command: profiles.Say, Args: map[string]string{"message": "hi all"}, Done: "Message sent to players."})
	if err != nil || strings.Contains(out, "Message sent") || !strings.Contains(out, `Sent "say hi all"`) {
		t.Fatalf("out %q err %v", out, err)
	}
	// gamemode in 1.3.0 only has refusals: silence is "not confirmed".
	out, _, err = e.do("old", Step{Command: profiles.Gamemode, Args: map[string]string{"player": "Steve", "mode": "creative"}, Done: "Game mode set."})
	if err != nil || !strings.Contains(out, "Game mode set. (not confirmed") {
		t.Fatalf("out %q err %v", out, err)
	}
	if got := e.sessions.lines(); got[1] != "gamemode 1 Steve" {
		t.Fatalf("sent %q; creative maps to 1 and the 1.3 order is mode then player", got)
	}
	// ... and a refusal is an error.
	e.sessions.responder = func(dir, line string) {
		appendLog(dir, "server.log", "2013-01-02 03:04:05 [INFO] That player cannot be found")
	}
	_, _, err = e.do("old", Step{Command: profiles.Gamemode, Args: map[string]string{"player": "Steve", "mode": "1"}, Done: "Game mode set."})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err %v", err)
	}
}

func TestDoRejectsBeforeSending(t *testing.T) {
	e := newEnv(t)
	e.server("alpha", modernProps, true)
	for name, step := range map[string]Step{
		"removed command":      {Command: profiles.ToggleDownfall},
		"extra word":           {Command: profiles.OpAdd, Args: map[string]string{"player": "Steve deop"}},
		"newline":              {Command: profiles.Say, Args: map[string]string{"message": "hi\nstop"}},
		"unsupported argument": {Command: profiles.Give, Args: map[string]string{"player": "a", "item": "stone", "amount": "1", "damage": "3"}},
		"unknown":              {Command: "NOPE"},
	} {
		_, _, err := e.do("alpha", Step{Command: profiles.WhitelistOn}, step)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if got := e.sessions.lines(); len(got) != 0 {
		t.Fatalf("a rejected batch sent %q", got)
	}
	_, _, err := e.do("alpha", Step{Command: profiles.ToggleDownfall})
	if !errors.Is(err, profiles.ErrUnsupported) || !strings.Contains(err.Error(), "cmd weather") {
		t.Fatalf("err %v", err)
	}
}

func TestNotRunning(t *testing.T) {
	e := newEnv(t)
	e.server("alpha", modernProps, false)
	_, _, err := e.do("alpha", Step{Command: profiles.WhitelistOn})
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err %v", err)
	}
	e.noScreen = true
	_, _, err = e.do("alpha", Step{Command: profiles.WhitelistOn})
	if !errors.Is(err, ErrNoScreen) {
		t.Fatalf("err %v", err)
	}
}

func TestSendFailureIsReported(t *testing.T) {
	e := newEnv(t)
	e.server("alpha", modernProps, true)
	e.sessions.sendErr = errors.New("screen gone")
	if _, _, err := e.do("alpha", Step{Command: profiles.WhitelistOn}); err == nil || !strings.Contains(err.Error(), "screen gone") {
		t.Fatalf("err %v", err)
	}
}

func TestSaveSteps(t *testing.T) {
	e := newEnv(t)
	e.server("alpha", modernProps, true)
	e.sessions.responder = modernResponder
	out, _, err := e.do("alpha", Step{Command: profiles.SaveAll, Doing: "Forcing save...", Done: "Done."})
	if err != nil || out != "Forcing save... Done.\n" {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestProfileNote(t *testing.T) {
	e := newEnv(t)
	e.server("alpha", "msm-version=paper/1.20.1\nmsm-log-path=server.log\n", true)
	_, errOut, _ := e.do("alpha", Step{Command: profiles.Say, Args: map[string]string{"message": "x"}})
	if !strings.Contains(errOut, `VERSION "paper/1.20.1"`) {
		t.Fatalf("no note for a VERSION that names no profile: %q", errOut)
	}
	e.server("beta", "", true)
	if _, errOut, _ := e.do("beta", Step{Command: profiles.Say, Args: map[string]string{"message": "x"}}); errOut != "" {
		t.Fatalf("an unset VERSION warned: %q", errOut)
	}
}

func TestCmdJoinsWords(t *testing.T) {
	e := newEnv(t)
	e.server("alpha", modernProps, true)
	var out bytes.Buffer
	if err := e.manager().Cmd(context.Background(), "alpha", []string{"weather", "clear"}, Output{Out: &out, Err: &out}); err != nil {
		t.Fatal(err)
	}
	if got := e.sessions.lines(); len(got) != 1 || got[0] != "weather clear" || out.String() != "Command sent.\n" {
		t.Fatalf("sent %q out %q", got, out.String())
	}
	e.server("beta", modernProps, false)
	if err := e.manager().Cmd(context.Background(), "beta", []string{"list"}, Output{Out: &out, Err: &out}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err %v", err)
	}
}

func TestConnected(t *testing.T) {
	for _, tc := range []struct {
		name, props, respond, want string
	}{
		{"modern", modernProps, "[12:00:00] [Server thread/INFO]: There are 2 of a max of 20 players online: Steve, Alex\n", "Connected players: Steve, Alex"},
		{"modern none", modernProps, "[12:00:00] [Server thread/INFO]: There are 0 of a max of 20 players online: \n", "No players are connected."},
		{"paper", modernProps, "[12:00:00 INFO]: There are 1 of a max of 20 players online: Steve\n", "Connected players: Steve"},
		{"1.7", "msm-version=minecraft/1.7.0\nmsm-log-path=logs/latest.log\n",
			"[12:00:00] [Server thread/INFO]: There are 2/20 players online:\n[12:00:00] [Server thread/INFO]: Steve, Alex\n", "Connected players: Steve, Alex"},
		{"1.7 none", "msm-version=minecraft/1.7.0\nmsm-log-path=logs/latest.log\n",
			"[12:00:00] [Server thread/INFO]: There are 0/20 players online:\n[12:00:00] [Server thread/INFO]:\n", "No players are connected."},
		{"1.2", "msm-version=minecraft/1.2.0\nmsm-log-path=server.log\n",
			"2012-03-01 12:00:00 [INFO] Connected players: Steve, Alex\n", "Connected players: Steve, Alex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.server("alpha", tc.props, true)
			e.sessions.responder = func(dir, line string) {
				path := "logs/latest.log"
				if strings.Contains(tc.props, "server.log") {
					path = "server.log"
				}
				appendLog(dir, path, strings.TrimSuffix(tc.respond, "\n"))
			}
			var out bytes.Buffer
			if err := e.manager().Connected(context.Background(), "alpha", Output{Out: &out, Err: &out}); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(out.String()); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestConnectedStoppedAndSilent(t *testing.T) {
	e := newEnv(t)
	e.server("off", modernProps, false)
	var out bytes.Buffer
	if err := e.manager().Connected(context.Background(), "off", Output{Out: &out, Err: &out}); err != nil ||
		out.String() != "Server \"off\" is not running. No users are connected.\n" {
		t.Fatalf("out %q err %v", out.String(), err)
	}
	e.server("mute", modernProps, true)
	if err := e.manager().Connected(context.Background(), "mute", Output{Out: &out, Err: &out}); !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("err %v", err)
	}
}

func TestCmdlogFollowsUntilInterrupted(t *testing.T) {
	e := newEnv(t)
	dir := e.server("alpha", modernProps, true)
	for i := 1; i <= 7; i++ {
		appendLog(dir, "logs/latest.log", modernTag+"old "+strings.Repeat("x", i))
	}
	e.sessions.responder = func(dir, line string) { appendLog(dir, "logs/latest.log", modernTag+"ran "+line) }
	ctx, cancel := context.WithCancel(context.Background())
	waits := 0
	e.clock.onWait = func() {
		waits++
		if waits == 1 {
			appendLog(dir, "logs/latest.log", modernTag+"later\x1b[2J")
		}
		if waits == 3 {
			cancel()
		}
	}
	var out, errOut bytes.Buffer
	err := e.manager().Cmdlog(ctx, "alpha", []string{"say", "hi"}, Output{Out: &out, Err: &errOut})
	if err != nil {
		t.Fatalf("Ctrl+C must end the follower cleanly: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Now watching logs", "old xxxxxxx", "old xxx\n", "ran say hi", "later[2J"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "old x\n") || strings.Contains(got, "\x1b") {
		t.Errorf("too much history, or a control character reached the terminal:\n%q", got)
	}
	// The lock is free again: another command goes through.
	if _, _, err := e.do("alpha", Step{Command: profiles.Say, Args: map[string]string{"message": "x"}}); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleAttach(t *testing.T) {
	e := newEnv(t)
	e.server("alpha", modernProps, true)
	var out bytes.Buffer
	if err := e.manager().Console(context.Background(), "alpha", process.Stdio{}, "xterm", Output{Out: &out, Err: &out}); err != nil {
		t.Fatal(err)
	}
	if got := e.sessions.attached; len(got) != 1 || got[0] != "msm-alpha xterm" {
		t.Fatalf("attached %q", got)
	}
	for _, want := range []string{"Ctrl+A then D", "Ctrl+C"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q explanation in %q", want, out.String())
		}
	}
	e.server("beta", modernProps, false)
	if err := e.manager().Console(context.Background(), "beta", process.Stdio{}, "", Output{Out: &out, Err: &out}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err %v", err)
	}
}

func TestSetWhitelist(t *testing.T) {
	e := newEnv(t)
	dir := e.server("off", "# keep me\nwhite-list=false\nmotd=hi\n", false)
	var out bytes.Buffer
	o := Output{Out: &out, Err: &out}
	if err := e.manager().SetWhitelist(context.Background(), "off", true, o); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "server.properties"))
	if string(data) != "# keep me\nwhite-list=true\nmotd=hi\n" {
		t.Fatalf("properties %q", data)
	}
	if !strings.Contains(out.String(), "applies when it starts") {
		t.Fatalf("out %q", out.String())
	}
	// A running server is told through its console and its file is left alone.
	dir = e.server("on", modernProps+"white-list=false\n", true)
	e.sessions.responder = modernResponder
	out.Reset()
	if err := e.manager().SetWhitelist(context.Background(), "on", true, o); err != nil || out.String() != "Whitelist enabled.\n" {
		t.Fatalf("out %q err %v", out.String(), err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "server.properties"))
	if !strings.Contains(string(data), "white-list=false") {
		t.Fatalf("a running server's properties were edited: %q", data)
	}
}

func TestLists(t *testing.T) {
	e := newEnv(t)
	dir := e.server("alpha", "", false)
	var out bytes.Buffer
	o := Output{Out: &out, Err: &out}
	m := e.manager()
	run := func(f func() error) string {
		t.Helper()
		out.Reset()
		if err := f(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	if got := run(func() error { return m.Whitelist("alpha", o) }); got != "No players are whitelisted.\n" {
		t.Fatalf("missing file: %q", got)
	}
	write := func(name, text string) { os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644) }
	write("whitelist.json", `[{"uuid":"1","name":"Steve"},{"uuid":"2","name":"Alex"}]`)
	if got := run(func() error { return m.Whitelist("alpha", o) }); got != "Steve\nAlex\n" {
		t.Fatalf("json: %q", got)
	}
	write("whitelist.json", "# comment\nSteve\r\n\n  Alex  \n")
	if got := run(func() error { return m.Whitelist("alpha", o) }); got != "Steve\nAlex\n" {
		t.Fatalf("text: %q", got)
	}
	write("whitelist.json", "[]")
	if got := run(func() error { return m.Whitelist("alpha", o) }); got != "No players are whitelisted.\n" {
		t.Fatalf("empty: %q", got)
	}
	if got := run(func() error { return m.Operators("alpha", o) }); got != "No players are operators.\n" {
		t.Fatalf("ops: %q", got)
	}
	write("ops.json", `[{"uuid":"1","name":"Admin","level":4}]`)
	if got := run(func() error { return m.Operators("alpha", o) }); got != "Admin\n" {
		t.Fatalf("ops: %q", got)
	}
	if got := run(func() error { return m.Blacklist("alpha", o) }); got != "The blacklist is empty.\n" {
		t.Fatalf("bl: %q", got)
	}
	write("banned-players.json", `[{"name":"Mallory"}]`)
	write("banned-ips.json", `[{"ip":"10.0.0.1"}]`)
	want := "Players:\n    Mallory\nIP Addresses:\n    10.0.0.1\n"
	if got := run(func() error { return m.Blacklist("alpha", o) }); got != want {
		t.Fatalf("bl: %q", got)
	}
	// A half-written file is an error, never an empty list, and is not touched.
	write("whitelist.json", `[{"name":"Ste`)
	if err := m.Whitelist("alpha", o); !errors.Is(err, ErrBadList) {
		t.Fatalf("err %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "whitelist.json")); string(data) != `[{"name":"Ste` {
		t.Fatal("a list command changed the file")
	}
	// A FIFO in the list's place does not hang the command.
	os.Remove(filepath.Join(dir, "ops.json"))
	if err := mkfifo(filepath.Join(dir, "ops.json")); err == nil {
		done := make(chan error, 1)
		go func() { done <- m.Operators("alpha", o) }()
		select {
		case err := <-done:
			if !errors.Is(err, ErrBadList) {
				t.Fatalf("err %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a FIFO hung the list command")
		}
	}
}

func TestForAll(t *testing.T) {
	e := newEnv(t)
	e.server("a", modernProps, true)
	e.server("b", modernProps, false)
	e.server("c", modernProps, true)
	e.sessions.responder = modernResponder
	var out, errOut bytes.Buffer
	o := Output{Out: &out, Err: &errOut}
	m := e.manager()
	err := m.ForAll(context.Background(), o, func(name string) error {
		return m.Do(context.Background(), name, []Step{{Command: profiles.WhitelistOn, Done: name + " on"}}, o)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "a on\nServer \"b\" is not running; skipped.\nc on\n" {
		t.Fatalf("out %q", got)
	}
	// Every failure is reported, in name order.
	e.sessions.responder = nil
	err = m.ForAll(context.Background(), o, func(name string) error {
		return m.Do(context.Background(), name, []Step{{Command: profiles.WhitelistOn}}, o)
	})
	if err == nil || strings.Index(err.Error(), `"a"`) > strings.Index(err.Error(), `"c"`) || strings.Count(err.Error(), "no answer") != 2 {
		t.Fatalf("err %v", err)
	}
}

func mkfifo(path string) error { return unix.Mkfifo(path, 0o644) }

// DoLocked and Running are for a caller that already holds the server's
// lock (a backup): they must not take it again, and must confirm the save
// commands a backup depends on from the log.
func TestDoLockedUnderHeldLock(t *testing.T) {
	e := newEnv(t)
	dir := e.server("alpha", modernProps, true)
	e.sessions.responder = func(d, line string) {
		modernResponder(d, line)
		switch line {
		case "save-off":
			appendLog(d, "logs/latest.log", modernTag+"Turned off world auto-saving")
		case "save-on":
			appendLog(d, "logs/latest.log", modernTag+"Turned on world auto-saving")
		}
	}
	lock, set, err := e.servers.LockServer("alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		o := Output{Out: &out, Err: &errOut}
		m := e.manager()
		running, err := m.Running(context.Background(), set, o)
		if err == nil && !running {
			err = errors.New("a running server was reported stopped")
		}
		if err == nil {
			err = m.DoLocked(context.Background(), set, []Step{{Command: profiles.SaveOff}, {Command: profiles.SaveAll, Sync: true}, {Command: profiles.SaveOn}}, o)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DoLocked blocked on the lock its caller holds")
	}
	if got := strings.Join(e.sessions.lines(), ","); got != "save-off,save-all,save-on" {
		t.Errorf("sent %s", got)
	}
	_ = dir

	delete(e.sessions.running, "msm-alpha")
	var out, errOut bytes.Buffer
	if running, err := e.manager().Running(context.Background(), set, Output{Out: &out, Err: &errOut}); err != nil || running {
		t.Errorf("stopped server: running=%v err=%v", running, err)
	}
}
