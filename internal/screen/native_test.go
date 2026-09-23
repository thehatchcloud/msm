package screen

// These tests drive a real GNU screen. They skip when screen is missing,
// unless MSM_REQUIRE_SCREEN=1 (set in CI) makes that a failure; set
// MSM_TEST_SCREEN to test a specific screen executable. The server stand-in
// is this test binary itself (TestFixtureChild), so no Java is needed. All
// sessions live in a private SCREENDIR with a private HOME, so the tests
// neither see nor disturb the user's own sessions or ~/.screenrc.

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/process"
)

// TestFixtureChild is the "server" inside screen: it appends every console
// line to the file named after "--" and exits on "stop".
func TestFixtureChild(t *testing.T) {
	if os.Getenv("MSM_SCREEN_FIXTURE") != "1" {
		return
	}
	out := os.Args[slices.Index(os.Args, "--")+1]
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(3)
		}
		f.WriteString(scanner.Text() + "\n")
		f.Close()
		if scanner.Text() == "stop" {
			return
		}
	}
}

// TestNativeHelper is a separate CLI invocation: it builds a fresh Backend
// from the environment, performs one action, and exits, so the parent test
// proves nothing depends on in-process state.
func TestNativeHelper(t *testing.T) {
	action := os.Getenv("MSM_SCREEN_HELPER")
	if action == "" {
		return
	}
	env := func(k string) string { return os.Getenv("MSM_SCREEN_" + k) }
	b := nativeBackend(t, env("BIN"), env("DIR"), env("HOME"))
	ctx := context.Background()
	want := fixtureArgv(t, env("OUT"))
	switch action {
	case "launch":
		if _, err := b.Launch(ctx, LaunchSpec{Name: env("NAME"), Dir: filepath.Dir(env("OUT")), Argv: want,
			Env: []string{"MSM_SCREEN_FIXTURE=1"}, Timeout: 20 * time.Second}); err != nil {
			t.Fatal(err)
		}
	case "send":
		st, err := b.Status(ctx, env("NAME"), want)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Send(ctx, st, env("LINE")); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown helper action %q", action)
	}
}

func findScreen(t *testing.T) string {
	t.Helper()
	path := os.Getenv("MSM_TEST_SCREEN")
	if path == "" {
		path, _ = exec.LookPath("screen")
	}
	if path == "" {
		if os.Getenv("MSM_REQUIRE_SCREEN") == "1" {
			t.Fatal("MSM_REQUIRE_SCREEN=1 but no screen executable was found")
		}
		t.Skip("GNU screen is not installed; set MSM_REQUIRE_SCREEN=1 to require it")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func nativeBackend(t *testing.T, bin, dir, home string) *Backend {
	t.Helper()
	me, err := identity.Current()
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Config{
		Screen: bin, Owner: me,
		Env:    []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "SCREENDIR=" + dir, "LANG=C.UTF-8"},
		Runner: process.ExecRunner{}, Terminal: process.ExecRunner{}, Processes: SystemProcesses{},
		Clock: clock.Real{}, PollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureArgv(t *testing.T, out string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe, "-test.run=^TestFixtureChild$", "--", out}
}

type nativeEnv struct {
	bin, dir, home, work string
	b                    *Backend
}

// newNativeEnv makes a private socket directory under /tmp, whose short
// path keeps socket names within macOS's 104-byte sun_path limit.
func newNativeEnv(t *testing.T) *nativeEnv {
	t.Helper()
	bin := findScreen(t)
	dir, err := os.MkdirTemp("/tmp", "msmscr")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e := &nativeEnv{bin: bin, dir: dir, home: t.TempDir(), work: t.TempDir()}
	e.b = nativeBackend(t, bin, dir, e.home)
	if _, err := e.b.Version(context.Background()); err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Quit whatever a failed test left behind, then remove the sockets.
		sessions, _ := e.b.Sessions(context.Background())
		for _, s := range sessions {
			e.raw("-S", s.ID, "-X", "quit")
		}
		e.raw("-wipe")
		os.RemoveAll(dir)
	})
	return e
}

// raw runs screen directly, bypassing the Backend, to build situations the
// Backend itself refuses to create.
func (e *nativeEnv) raw(args ...string) (process.Result, error) {
	cmd := e.b.command(e.work, []string{"MSM_SCREEN_FIXTURE=1"}, args...)
	return process.ExecRunner{}.Run(context.Background(), cmd)
}

func (e *nativeEnv) helper(t *testing.T, action, name, out, line string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNativeHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "MSM_SCREEN_HELPER="+action, "MSM_SCREEN_BIN="+e.bin, "MSM_SCREEN_DIR="+e.dir,
		"MSM_SCREEN_HOME="+e.home, "MSM_SCREEN_OUT="+out, "MSM_SCREEN_NAME="+name, "MSM_SCREEN_LINE="+line)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper %s: %v\n%s", action, err, output)
	}
}

// waitFor polls cond in real time; native screen work is asynchronous.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitForLine(t *testing.T, out, line string) {
	t.Helper()
	waitFor(t, "console line "+line[:min(len(line), 40)], func() bool {
		data, _ := os.ReadFile(out)
		return slices.Contains(strings.Split(string(data), "\n"), line)
	})
}

// TestNativeVersionGate checks that the backend accepts or refuses the
// screen under test according to MinimumVersion. CI runs it against macOS's
// bundled /usr/bin/screen, which must be refused rather than half-work.
func TestNativeVersionGate(t *testing.T) {
	bin := findScreen(t)
	b := nativeBackend(t, bin, t.TempDir(), t.TempDir())
	v, err := b.Version(context.Background())
	t.Logf("%s reports screen %s: %v", bin, v, err)
	if want := os.Getenv("MSM_EXPECT_UNSUPPORTED") == "1"; want != errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("MSM_EXPECT_UNSUPPORTED=%v but Version returned %v", want, err)
	}
}

func TestNativeLifecycle(t *testing.T) {
	e := newNativeEnv(t)
	ctx := context.Background()
	v, err := e.b.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("testing %s (screen %s)", e.bin, v)

	const name = "msm-native"
	out := filepath.Join(e.work, "console.log")
	want := fixtureArgv(t, out)

	// Launch from one invocation, which then exits.
	e.helper(t, "launch", name, out, "")

	// Reconnect from this process with no shared state.
	st, err := nativeBackend(t, e.bin, e.dir, e.home).Status(ctx, name, want)
	if err != nil || st.Liveness != Running {
		t.Fatalf("status after launch = %+v, %v", st, err)
	}
	if !slices.Equal(st.Process.Argv, want) || st.Process.UID != os.Getuid() || st.Process.PPID != st.Session.PID {
		t.Fatalf("window process = %+v", st.Process)
	}

	// Screen's command parser must not reinterpret any printable input.
	var printable strings.Builder
	for c := byte(0x20); c < 0x7f; c++ {
		printable.WriteByte(c)
	}
	tricky := printable.String() + ` ${HOME} $PATH é漢🙂 \015 ^M ^C "q" 'q' %d`
	if err := e.b.Send(ctx, st, tricky); err != nil {
		t.Fatal(err)
	}
	waitForLine(t, out, tricky)
	// A maximum-length line of escapable and multibyte characters needs
	// several stuff chunks; it must still arrive as one intact line.
	long := strings.Repeat(`$^\é`, MaxInputBytes/5)
	if err := e.b.Send(ctx, st, long); err != nil {
		t.Fatal(err)
	}
	waitForLine(t, out, long)
	if err := e.b.Send(ctx, st, "two\nlines"); !errors.Is(err, ErrUnsafeInput) {
		t.Fatalf("unsafe input: %v", err)
	}

	// Another fresh invocation can send too.
	e.helper(t, "send", name, out, "from a fresh invocation")
	waitForLine(t, out, "from a fresh invocation")

	// A second launch must not create a duplicate session.
	if _, err := e.b.Launch(ctx, LaunchSpec{Name: name, Dir: e.work, Argv: want, Timeout: 5 * time.Second}); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("duplicate launch: %v", err)
	}

	// Attach through a real terminal, detach with Ctrl-A d, and the server
	// keeps running.
	attachAndDetach(t, e, st)
	st, err = e.b.Status(ctx, name, want)
	if err != nil || st.Liveness != Running || st.Session.State != Detached {
		t.Fatalf("status after detach = %+v, %v", st, err)
	}
	if err := e.b.Send(ctx, st, "after detach"); err != nil {
		t.Fatal(err)
	}
	waitForLine(t, out, "after detach")

	// Stop through the console and wait with a deadline.
	if err := e.b.Send(ctx, st, "stop"); err != nil {
		t.Fatal(err)
	}
	if st, err := e.b.WaitStopped(ctx, name, want, 15*time.Second); err != nil || st.Liveness != Stopped {
		t.Fatalf("stop = %+v, %v", st, err)
	}
	if err := e.b.Send(ctx, st, "too late"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("send to stopped server: %v", err)
	}
}

func attachAndDetach(t *testing.T, e *nativeEnv, st Status) {
	t.Helper()
	primary, replica, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	defer replica.Close()
	if err := unix.IoctlSetWinsize(int(replica.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		t.Fatal(err)
	}
	var screenOut strings.Builder
	var mu sync.Mutex
	go func() { // drain the terminal so screen never blocks writing to it
		buf := make([]byte, 4096)
		for {
			n, err := primary.Read(buf)
			mu.Lock()
			screenOut.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- e.b.Attach(context.Background(), st, process.Stdio{In: replica, Out: replica, Err: replica}, "vt100")
	}()

	waitFor(t, "session to show as attached", func() bool {
		sessions, _ := e.b.Sessions(context.Background())
		return slices.ContainsFunc(sessions, func(s Session) bool { return s.ID == st.Session.ID && s.State == Attached })
	})
	if _, err := primary.Write([]byte{0x01, 'd'}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("attach: %v\nterminal output: %q", err, screenOut.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Ctrl-A d did not detach")
	}
}

func TestNativeAnomalies(t *testing.T) {
	ctx := context.Background()

	t.Run("inaccessible socket directory", func(t *testing.T) {
		e := newNativeEnv(t)
		if err := os.Chmod(e.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := e.b.Status(ctx, "msm-x", []string{"java"}); !errors.Is(err, ErrInaccessible) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("foreign session is reported, never adopted", func(t *testing.T) {
		e := newNativeEnv(t)
		out := filepath.Join(e.work, "console.log")
		other := append(fixtureArgv(t, out), "unexpected-extra-argument")
		if _, err := e.raw(append([]string{"-dmS", "msm-x"}, other...)...); err != nil {
			t.Fatal(err)
		}
		want := fixtureArgv(t, out)
		var st Status
		waitFor(t, "foreign session", func() bool {
			st, _ = e.b.Status(ctx, "msm-x", want)
			return st.Liveness == Foreign
		})
		if err := e.b.Send(ctx, st, "say hi"); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("send: %v", err)
		}
		if _, err := e.b.Launch(ctx, LaunchSpec{Name: "msm-x", Dir: e.work, Argv: want, Timeout: time.Second}); !errors.Is(err, ErrAlreadyRunning) {
			t.Fatalf("launch: %v", err)
		}
		if st, _ := e.b.Status(ctx, "msm-x", other); st.Liveness != Running {
			t.Fatalf("the foreign process was disturbed: %+v", st)
		}
	})

	t.Run("duplicate sessions are refused", func(t *testing.T) {
		e := newNativeEnv(t)
		want := fixtureArgv(t, filepath.Join(e.work, "console.log"))
		for range 2 {
			if _, err := e.raw(append([]string{"-dmS", "msm-x"}, want...)...); err != nil {
				t.Fatal(err)
			}
		}
		waitFor(t, "duplicate detection", func() bool {
			_, err := e.b.Status(ctx, "msm-x", want)
			return errors.Is(err, ErrDuplicate)
		})
		if _, err := e.b.Launch(ctx, LaunchSpec{Name: "msm-x", Dir: e.work, Argv: want, Timeout: time.Second}); !errors.Is(err, ErrDuplicate) {
			t.Fatalf("launch: %v", err)
		}
	})

	t.Run("stale socket is ignored and its orphan is not adopted", func(t *testing.T) {
		e := newNativeEnv(t)
		out := filepath.Join(e.work, "console.log")
		want := fixtureArgv(t, out)
		spec := LaunchSpec{Name: "msm-x", Dir: e.work, Argv: want, Env: []string{"MSM_SCREEN_FIXTURE=1"}, Timeout: 15 * time.Second}
		st, err := e.b.Launch(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		orphan := st.Process.PID
		t.Cleanup(func() { syscall.Kill(orphan, syscall.SIGKILL) })
		if err := syscall.Kill(st.Session.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "dead socket", func() bool {
			st, err = e.b.Status(ctx, "msm-x", want)
			return err == nil && st.Liveness == Stopped && len(st.Stale) == 1
		})
		if st.Process != nil || st.Session != nil {
			t.Fatalf("stale session adopted: %+v", st)
		}
		if _, err := os.Stat(filepath.Join(e.dir, st.Stale[0].ID)); err != nil {
			t.Fatalf("stale socket was removed: %v", err)
		}
		// A new server can start beside the stale socket.
		syscall.Kill(orphan, syscall.SIGKILL)
		st, err = e.b.Launch(ctx, spec)
		if err != nil || len(st.Stale) != 1 {
			t.Fatalf("launch beside stale socket = %+v, %v", st, err)
		}
		if err := e.b.Send(ctx, st, "stop"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.b.WaitStopped(ctx, "msm-x", want, 15*time.Second); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("failed start reports an error before the deadline", func(t *testing.T) {
		e := newNativeEnv(t)
		begin := time.Now()
		_, err := e.b.Launch(ctx, LaunchSpec{Name: "msm-x", Dir: e.work, Argv: []string{filepath.Join(e.work, "missing-java")}, Timeout: 5 * time.Second})
		if !errors.Is(err, ErrExited) && !errors.Is(err, ErrStartTimeout) {
			t.Fatalf("err = %v", err)
		}
		if elapsed := time.Since(begin); elapsed > 10*time.Second {
			t.Fatalf("launch took %v with a 5s deadline", elapsed)
		}
	})
}
