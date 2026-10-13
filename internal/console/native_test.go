package console

// These tests send console commands to a server running in a real GNU
// screen. They skip when screen is missing unless MSM_REQUIRE_SCREEN=1 (set
// in CI); MSM_TEST_SCREEN selects the executable. The "Minecraft server" is
// this test binary running TestFakeMinecraft, so no Java is needed.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/lifecycle"
	"github.com/thehatchcloud/msm/internal/process"
	"github.com/thehatchcloud/msm/internal/profiles"
	"github.com/thehatchcloud/msm/internal/screen"
	"github.com/thehatchcloud/msm/internal/servers"
)

// TestFakeMinecraft behaves like a modern Minecraft server in its working
// directory: it logs Done, answers the console commands MSM sends in the
// vanilla 1.21 wording, echoes "echo" lines back verbatim and exits on stop.
func TestFakeMinecraft(t *testing.T) {
	i := slices.Index(os.Args, "fake-minecraft")
	if i < 0 {
		return
	}
	os.MkdirAll("logs", 0o755)
	os.Rename(filepath.Join("logs", "latest.log"), filepath.Join("logs", "previous.log"))
	logf := func(format string, args ...any) {
		f, err := os.OpenFile(filepath.Join("logs", "latest.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			os.Exit(3)
		}
		fmt.Fprintf(f, "[%s] [Server thread/INFO]: %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
		f.Close()
	}
	time.Sleep(300 * time.Millisecond)
	logf(`Done (0.3s)! For help, type "help"`)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		f := strings.Fields(line)
		switch {
		case line == "save-all":
			logf("Saving the game (this may take a moment!)")
			logf("Saved the game")
		case line == "list":
			logf("There are 2 of a max of 20 players online: Steve, Alex")
		case len(f) == 3 && f[0] == "whitelist" && f[1] == "add" && f[2] == "Ghost":
			logf("That player does not exist")
		case len(f) == 3 && f[0] == "whitelist" && f[1] == "add":
			logf("Added %s to the whitelist", f[2])
		case strings.HasPrefix(line, "echo "):
			logf("Echo: <%s>", strings.TrimPrefix(line, "echo "))
		case line == "stop":
			logf("Stopping server")
			os.Exit(0)
		}
	}
	os.Exit(0)
}

type nativeEnv struct {
	root, sockets string
	servers       *servers.Manager
	lifecycle     *lifecycle.Manager
	console       *Manager
}

func newNativeEnv(t *testing.T) *nativeEnv {
	t.Helper()
	bin := os.Getenv("MSM_TEST_SCREEN")
	if bin == "" {
		bin, _ = exec.LookPath("screen")
	}
	if bin == "" {
		if os.Getenv("MSM_REQUIRE_SCREEN") == "1" {
			t.Fatal("MSM_REQUIRE_SCREEN=1 but no screen executable was found")
		}
		t.Skip("GNU screen is not installed; set MSM_REQUIRE_SCREEN=1 to require it")
	}
	bin, _ = filepath.Abs(bin)
	sockets, err := os.MkdirTemp("/tmp", "msmco")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(sockets, 0o700)
	me, err := identity.Current()
	if err != nil {
		t.Fatal(err)
	}
	e := &nativeEnv{root: t.TempDir(), sockets: sockets}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "SCREENDIR=" + sockets, "LANG=C.UTF-8"}
	backend := func(owner identity.Identity) (*screen.Backend, error) {
		return screen.New(screen.Config{Screen: bin, Owner: owner, Env: env, Runner: process.ExecRunner{},
			Terminal: process.ExecRunner{}, Processes: screen.SystemProcesses{}, Clock: clock.Real{},
			PollInterval: 50 * time.Millisecond})
	}
	b, err := backend(me)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Version(context.Background()); err != nil {
		os.RemoveAll(sockets)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sessions, _ := b.Sessions(context.Background())
		for _, s := range sessions {
			exec.Command(bin, "-S", s.ID, "-X", "quit").Run()
		}
		os.RemoveAll(sockets)
	})
	euid := me.UID
	e.servers, err = servers.New(servers.Config{Root: e.root, PropertiesFile: "server.properties",
		Owner: me, DefaultOwner: &me, Prober: stoppedProber{}, EUID: &euid})
	if err != nil {
		t.Fatal(err)
	}
	e.lifecycle, err = lifecycle.New(lifecycle.Config{Servers: e.servers, Path: os.Getenv("PATH"), Clock: clock.Real{},
		Timeout: 20 * time.Second, EUID: &euid,
		Sessions: func(owner identity.Identity) (lifecycle.Sessions, error) { return backend(owner) }})
	if err != nil {
		t.Fatal(err)
	}
	e.console, err = New(Config{Servers: e.servers, Clock: clock.Real{}, Timeout: 10 * time.Second,
		Sessions: func(owner identity.Identity) (Sessions, error) { return backend(owner) }})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *nativeEnv) server(t *testing.T, name string) string {
	t.Helper()
	c, err := e.servers.Create(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(exe, "'") {
		t.Skip("test binary path contains a quote")
	}
	os.WriteFile(filepath.Join(c.Dir, "server.jar"), []byte("PK\x03\x04"), 0o644)
	props := fmt.Sprintf("msm-version=minecraft/1.21.0\nmsm-invocation='%s' '-test.run=^TestFakeMinecraft$' fake-minecraft\n", exe)
	if err := os.WriteFile(filepath.Join(c.Dir, "server.properties"), []byte(props), 0o644); err != nil {
		t.Fatal(err)
	}
	return c.Dir
}

func TestNativeConsoleCommands(t *testing.T) {
	e := newNativeEnv(t)
	ctx := context.Background()
	dir := e.server(t, "a")
	var buf bytes.Buffer
	if err := e.lifecycle.Start(ctx, "a", &buf); err != nil {
		t.Fatalf("start: %v\n%s", err, buf.String())
	}
	t.Cleanup(func() { e.lifecycle.Stop(ctx, "a", true, &bytes.Buffer{}) })
	out := Output{Out: &buf, Err: &buf}

	// Answered commands, a refusal and the player list, through real screen.
	buf.Reset()
	err := e.console.Do(ctx, "a", []Step{
		{Command: profiles.WhitelistAdd, Args: map[string]string{"player": "Steve"}, Done: "Player Steve is now whitelisted."},
		{Command: profiles.WhitelistAdd, Args: map[string]string{"player": "Ghost"}, Done: "Player Ghost is now whitelisted."},
		{Command: profiles.SaveAll, Doing: "Forcing save...", Done: "Done."},
	}, out)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "That player does not exist") {
		t.Fatalf("err %v\n%s", err, buf.String())
	}
	if want := "Player Steve is now whitelisted.\nForcing save... Done.\n"; buf.String() != want {
		t.Fatalf("out %q want %q", buf.String(), want)
	}
	buf.Reset()
	if err := e.console.Connected(ctx, "a", out); err != nil || buf.String() != "Connected players: Steve, Alex\n" {
		t.Fatalf("connected: %q %v", buf.String(), err)
	}

	// Raw commands reach the server byte for byte: nothing is expanded,
	// unescaped or split, whatever screen's own parser would make of it.
	const raw = `$HOME ${PATH} ^M \n \\ \015 "quoted" 'single' a;b | c > d $(x) ` + "`y`" + ` é✓`
	if err := e.console.Cmd(ctx, "a", []string{"echo", raw}, out); err != nil {
		t.Fatal(err)
	}
	waitLog(t, dir, "Echo: <"+raw+">")

	// cmdlog shows the answer and ends at Ctrl+C without touching the server.
	cctx, cancel := context.WithCancel(ctx)
	var wmu sync.Mutex
	var watched bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- e.console.Cmdlog(cctx, "a", []string{"echo", "marker"}, Output{Out: &syncWriter{&wmu, &watched}, Err: &syncWriter{&wmu, &watched}})
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		wmu.Lock()
		got := watched.String()
		wmu.Unlock()
		if strings.Contains(got, "Echo: <marker>") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cmdlog never showed the answer:\n%s", got)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("cmdlog: %v", err)
	}
	var s strings.Builder
	if err := e.lifecycle.Status(ctx, "a", &s); err != nil || !strings.Contains(s.String(), "running") {
		t.Fatalf("Ctrl+C in cmdlog stopped the server: %q %v", s.String(), err)
	}

	// A stopped server is reported, not guessed at.
	if err := e.lifecycle.Stop(ctx, "a", true, &buf); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := e.console.Do(ctx, "a", []Step{{Command: profiles.SaveAll}}, out); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err %v", err)
	}
	if err := e.console.Connected(ctx, "a", out); err != nil || !strings.Contains(buf.String(), "No users are connected") {
		t.Fatalf("connected on a stopped server: %q %v", buf.String(), err)
	}
}

type syncWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func waitLog(t *testing.T, dir, text string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		data, _ := os.ReadFile(filepath.Join(dir, "logs", "latest.log"))
		if strings.Contains(string(data), text) {
			return
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "logs", "latest.log"))
	t.Fatalf("log never showed %q:\n%s", text, data)
}
