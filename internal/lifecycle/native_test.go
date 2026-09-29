package lifecycle

// These tests run servers in a real GNU screen. They skip when screen is
// missing unless MSM_REQUIRE_SCREEN=1 (set in CI); MSM_TEST_SCREEN selects
// the executable. The "Minecraft server" is this test binary running
// TestFakeMinecraft, so no Java is needed. Sessions live in a private
// SCREENDIR and HOME.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/process"
	"github.com/thehatchcloud/msm/internal/screen"
	"github.com/thehatchcloud/msm/internal/servers"
)

// TestFakeMinecraft behaves like a Minecraft 1.7+ server in its working
// directory: it moves latest.log aside, logs Done, answers save-all and
// say, and exits on stop. In "eula" mode it refuses to start.
func TestFakeMinecraft(t *testing.T) {
	i := slices.Index(os.Args, "fake-minecraft")
	if i < 0 || i+1 >= len(os.Args) {
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
	if os.Args[i+1] == "eula" {
		os.WriteFile("eula.txt", []byte("eula=false\n"), 0o644)
		logf("You need to agree to the EULA in order to run the server. Go to eula.txt for more info.")
		os.Exit(0)
	}
	time.Sleep(300 * time.Millisecond)
	logf(`Done (0.3s)! For help, type "help"`)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch line := strings.TrimSpace(scanner.Text()); {
		case line == "save-all":
			logf("Saved the world")
		case strings.HasPrefix(line, "say "):
			logf("[Server] %s", strings.TrimPrefix(line, "say "))
		case line == "stop":
			logf("Stopping server")
			os.Exit(0)
		}
	}
	os.Exit(0)
}

type nativeEnv struct {
	root, bin, sockets, home string
	servers                  *servers.Manager
	m                        *Manager
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
	// A short socket directory keeps within macOS's sun_path limit.
	sockets, err := os.MkdirTemp("/tmp", "msmlc")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(sockets, 0o700)
	me, err := identity.Current()
	if err != nil {
		t.Fatal(err)
	}
	e := &nativeEnv{root: t.TempDir(), bin: bin, sockets: sockets, home: t.TempDir()}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + e.home, "SCREENDIR=" + sockets, "LANG=C.UTF-8"}
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
	e.m, err = New(Config{Servers: e.servers, Path: os.Getenv("PATH"), Clock: clock.Real{}, Timeout: 20 * time.Second,
		EUID: &euid, Sessions: func(owner identity.Identity) (Sessions, error) { return backend(owner) }})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *nativeEnv) server(t *testing.T, name, mode, props string) string {
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
	props += fmt.Sprintf("msm-invocation='%s' '-test.run=^TestFakeMinecraft$' fake-minecraft %s\n", exe, mode)
	if err := os.WriteFile(filepath.Join(c.Dir, "server.properties"), []byte(props), 0o644); err != nil {
		t.Fatal(err)
	}
	return c.Dir
}

func logHas(t *testing.T, dir, text string) bool {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, "logs", "latest.log"))
	return strings.Contains(string(data), text)
}

func TestNativeServerLifecycle(t *testing.T) {
	e := newNativeEnv(t)
	ctx := context.Background()
	a := e.server(t, "a", "ok", "msm-stop-delay=1\n")
	b := e.server(t, "b", "ok", "")
	var out strings.Builder
	status := func(name string) string {
		t.Helper()
		var s strings.Builder
		if err := e.m.Status(ctx, name, &s); err != nil {
			t.Fatal(err)
		}
		return s.String()
	}

	if err := e.m.Start(ctx, "a", &out); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	if !strings.Contains(status("a"), "running") {
		t.Fatal("not running after start")
	}
	// A warned stop: say, countdown, save, stop.
	if err := e.m.Stop(ctx, "a", false, &out); err != nil {
		t.Fatalf("stop: %v\n%s", err, out.String())
	}
	for _, line := range []string{"[Server] SERVER SHUTTING DOWN IN 1 SECONDS!", "Saved the world", "Stopping server"} {
		if !logHas(t, a, line) {
			t.Errorf("log lacks %q", line)
		}
	}
	if !strings.Contains(status("a"), "stopped") || isActive(filepath.Join(a, "active")) {
		t.Fatal("stop left a running or active")
	}

	// Ctrl+C during a countdown aborts it and tells the players.
	if err := e.m.Start(ctx, "a", &out); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(a, "server.properties"), []byte(strings.Replace(
		mustRead(t, filepath.Join(a, "server.properties")), "msm-stop-delay=1", "msm-stop-delay=60", 1)), 0o644)
	abortCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := e.m.Stop(abortCtx, "a", false, &out)
	cancel()
	if !errors.Is(err, ErrAborted) || !strings.Contains(status("a"), "running") || !isActive(filepath.Join(a, "active")) {
		t.Fatalf("abort: %v\n%s", err, out.String())
	}
	waitLog(t, a, "[Server] Server shut down aborted.")

	// Global restart now: a (active) comes back, b (inactive) stays down.
	if err := e.m.RestartAll(ctx, true, Output{Out: &out, Err: &out}); err != nil {
		t.Fatalf("restart all: %v\n%s", err, out.String())
	}
	if !strings.Contains(status("a"), "running") || !strings.Contains(status("b"), "stopped") {
		t.Fatalf("after restart all:\n%s", out.String())
	}
	// msm all start marks b active and starts it; the global stop keeps intent.
	if err := e.m.Each(ctx, VerbStart, false, Output{Out: &out, Err: &out}); err != nil {
		t.Fatalf("all start: %v\n%s", err, out.String())
	}
	if err := e.m.StopAll(ctx, true, Output{Out: &out, Err: &out}); err != nil {
		t.Fatalf("stop all: %v\n%s", err, out.String())
	}
	if !strings.Contains(status("a"), "stopped") || !strings.Contains(status("b"), "stopped") ||
		!isActive(filepath.Join(a, "active")) || !isActive(filepath.Join(b, "active")) {
		t.Fatalf("after stop all:\n%s", out.String())
	}
}

func TestNativeEULARefusal(t *testing.T) {
	e := newNativeEnv(t)
	dir := e.server(t, "c", "eula", "")
	var out strings.Builder
	if err := e.m.Start(context.Background(), "c", &out); !errors.Is(err, ErrEULA) {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(mustRead(t, filepath.Join(dir, "eula.txt")), "eula=false") {
		t.Fatal("the manager changed eula.txt")
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func waitLog(t *testing.T, dir, text string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if logHas(t, dir, text) {
			return
		}
	}
	t.Fatalf("log never showed %q", text)
}
