package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/legacyconf"
)

var bg = context.Background()

// CT-CMD-020, CT-CMD-021: per-server start marks the server active and
// waits for a fresh start line; stop warns, counts down, saves, stops and
// marks it inactive.
func TestStartAndStop(t *testing.T) {
	e := newEnv(t)
	e.server("a", behaveOK, "")
	m := e.manager()
	var out strings.Builder
	if err := m.Start(bg, "a", &out); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	if !e.active("a") || !e.screen.running("a") || !strings.Contains(out.String(), `Server "a" started.`) {
		t.Fatalf("after start: active %v running %v\n%s", e.active("a"), e.screen.running("a"), out.String())
	}
	out.Reset()
	if err := m.Start(bg, "a", &out); err != nil || e.screen.launched("a") != 1 || !strings.Contains(out.String(), "already running") {
		t.Fatalf("second start: %v, %d launches\n%s", err, e.screen.launched("a"), out.String())
	}

	var waits []time.Duration
	e.clock.onWait = func(d time.Duration) {
		if d >= time.Second {
			waits = append(waits, d)
		}
	}
	out.Reset()
	if err := m.Stop(bg, "a", false, &out); err != nil {
		t.Fatalf("stop: %v\n%s", err, out.String())
	}
	want := []string{"say SERVER SHUTTING DOWN IN 10 SECONDS!", "save-all", "stop"}
	if got := e.screen.lines("a"); !slices.Equal(got, want) {
		t.Fatalf("console lines %q, want %q", got, want)
	}
	if !slices.Contains(waits, 10*time.Second) {
		t.Fatalf("no 10 second countdown: %v", waits)
	}
	if e.active("a") || e.screen.running("a") {
		t.Fatal("stop left the server active or running")
	}
	for _, s := range []string{"Issued the warning", "Server \"a\" saved.", "Server \"a\" stopped."} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("stop output lacks %q:\n%s", s, out.String())
		}
	}
	// Stopping a stopped server succeeds and keeps it inactive.
	out.Reset()
	if err := m.Stop(bg, "a", false, &out); err != nil || !strings.Contains(out.String(), "not running") {
		t.Fatalf("stop again: %v\n%s", err, out.String())
	}
}

// CT-CMD-022: now skips the warning and countdown, never the save or the
// wait for the process to end.
func TestStopNowStillSavesAndWaits(t *testing.T) {
	e := newEnv(t)
	e.server("a", behaveOK, "")
	e.run("a")
	e.clock.onWait = func(d time.Duration) {
		if d >= time.Second {
			t.Errorf("stop now waited %s", d)
		}
	}
	var out strings.Builder
	if err := e.manager().Stop(bg, "a", true, &out); err != nil {
		t.Fatal(err)
	}
	if got := e.screen.lines("a"); !slices.Equal(got, []string{"save-all", "stop"}) {
		t.Fatalf("console lines %q", got)
	}
	if e.screen.running("a") {
		t.Fatal("still running")
	}
}

// CT-CMD-023, CT-CMD-024: restart warns with the restart message and
// delay, stops, starts again and marks the server active; restart of a
// stopped server starts it.
func TestRestart(t *testing.T) {
	e := newEnv(t)
	e.server("a", behaveOK, "msm-restart-delay=3\n")
	e.run("a")
	var out strings.Builder
	if err := e.manager().Restart(bg, "a", false, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	want := []string{"say SERVER REBOOT IN 3 SECONDS!", "save-all", "stop"}
	if got := e.screen.lines("a"); !slices.Equal(got, want) || e.screen.launched("a") != 2 || !e.screen.running("a") || !e.active("a") {
		t.Fatalf("lines %q launches %d running %v active %v", got, e.screen.launched("a"), e.screen.running("a"), e.active("a"))
	}
	e.screen.crash("a")
	e.setActive("a", false)
	if err := e.manager().Restart(bg, "a", true, &out); err != nil || e.screen.launched("a") != 3 || !e.active("a") {
		t.Fatalf("restart of a stopped server: %v, %d launches", err, e.screen.launched("a"))
	}
}

// CT-CMD-025
func TestStatus(t *testing.T) {
	e := newEnv(t)
	e.server("a", behaveOK, "")
	var out strings.Builder
	m := e.manager()
	if err := m.Status(bg, "a", &out); err != nil || out.String() != "Server \"a\" is stopped.\n" {
		t.Fatalf("%v %q", err, out.String())
	}
	e.run("a")
	out.Reset()
	if err := m.Status(bg, "a", &out); err != nil || out.String() != "Server \"a\" is running.\n" {
		t.Fatalf("%v %q", err, out.String())
	}
	e.noScrn = true
	out.Reset()
	if err := e.manager().Status(bg, "a", &out); err != nil || !strings.Contains(out.String(), "stopped") {
		t.Fatalf("without screen: %v %q", err, out.String())
	}
}

// Ctrl+C during the countdown sends the abort message, leaves the server
// running and leaves its intent as it was.
func TestCountdownAbort(t *testing.T) {
	for _, restart := range []bool{false, true} {
		e := newEnv(t)
		e.server("a", behaveOK, "")
		e.run("a")
		e.setActive("a", true)
		ctx, cancel := context.WithCancel(bg)
		e.clock.onWait = func(d time.Duration) {
			if d == 10*time.Second {
				cancel()
			}
		}
		var out strings.Builder
		m := e.manager()
		var err error
		if restart {
			err = m.Restart(ctx, "a", false, &out)
		} else {
			err = m.Stop(ctx, "a", false, &out)
		}
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("restart=%v: %v\n%s", restart, err, out.String())
		}
		abort := "say Server shut down aborted."
		if restart {
			abort = "say Server reboot aborted."
		}
		lines := e.screen.lines("a")
		if len(lines) != 2 || lines[1] != abort || !e.screen.running("a") || !e.active("a") {
			t.Fatalf("restart=%v: lines %q running %v active %v", restart, lines, e.screen.running("a"), e.active("a"))
		}
		if !strings.Contains(out.String(), "was aborted") {
			t.Fatalf("output %q", out.String())
		}
	}
}

// A start line from an earlier run never confirms a new start (DEV-010):
// neither in a log that is appended to (server.log) nor in one the server
// replaces. Missing readiness fails at the deadline and leaves the server
// running; nothing is killed.
func TestReadinessNeedsAFreshStartLine(t *testing.T) {
	for _, tc := range []struct{ props, log, old string }{
		{"", "logs/latest.log", doneLine},
		{"msm-version=minecraft/1.2.5\n", "server.log", "2026-01-01 12:00:00 [INFO] Done (1.0s)! For help"},
	} {
		e := newEnv(t)
		dir := e.server("a", behaveSlow, tc.props)
		os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
		if err := os.WriteFile(filepath.Join(dir, tc.log), []byte(tc.old+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e.timeout = 20 * time.Second
		var out strings.Builder
		err := e.manager().Start(bg, "a", &out)
		if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "left running") {
			t.Fatalf("%s: %v\n%s", tc.log, err, out.String())
		}
		if !e.screen.running("a") || len(e.screen.lines("a")) != 0 {
			t.Fatalf("%s: the slow server was disturbed: %q", tc.log, e.screen.lines("a"))
		}
	}
}

// A server that ignores stop makes the command fail at the deadline; the
// manager never kills or quits the session.
func TestStopTimeoutNeverKills(t *testing.T) {
	e := newEnv(t)
	e.server("a", behaveHang, "")
	e.run("a")
	e.timeout = time.Minute
	err := e.manager().Stop(bg, "a", true, &strings.Builder{})
	if !errors.Is(err, ErrStillRunning) || !strings.Contains(err.Error(), "nothing was killed") || !e.screen.running("a") {
		t.Fatalf("%v, running %v", err, e.screen.running("a"))
	}
	for _, c := range e.screen.calls {
		if slices.Contains(c, "quit") || slices.Contains(c, "kill") {
			t.Fatalf("screen was asked to %q", c)
		}
	}
}

// Start refuses what cannot work before launching anything, and explains
// a server that exits during startup.
func TestStartFailures(t *testing.T) {
	cases := []struct {
		name     string
		behavior string
		setup    func(e *env, dir string)
		want     error
		launches int
	}{
		{"no java", behaveOK, func(e *env, _ string) { os.Remove(filepath.Join(e.path, "java")) }, ErrNoJava, 0},
		{"java not executable", behaveOK, func(e *env, _ string) { os.Chmod(filepath.Join(e.path, "java"), 0o644) }, ErrNoJava, 0},
		{"no jar", behaveOK, func(_ *env, dir string) { os.Remove(filepath.Join(dir, "server.jar")) }, ErrNoJAR, 0},
		{"invalid jar", behaveOK, func(_ *env, dir string) { os.WriteFile(filepath.Join(dir, "server.jar"), []byte("<html>"), 0o644) }, ErrInvalidJAR, 0},
		{"jar is a directory", behaveOK, func(_ *env, dir string) {
			os.Remove(filepath.Join(dir, "server.jar"))
			os.Mkdir(filepath.Join(dir, "server.jar"), 0o755)
		}, ErrInvalidJAR, 0},
		{"shell syntax", behaveOK, func(_ *env, dir string) {
			os.WriteFile(filepath.Join(dir, "server.properties"), []byte("msm-invocation=java -jar {JAR}; touch /tmp/x\n"), 0o644)
		}, legacyconf.ErrUnsafeInvocation, 0},
		{"no screen", behaveOK, func(e *env, _ string) { e.noScrn = true }, ErrNoScreen, 0},
		{"eula", behaveEULA, nil, ErrEULA, 1},
		{"port in use", behavePort, nil, ErrPortInUse, 1},
		{"crash", behaveCrash, nil, ErrExited, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			dir := e.server("a", tc.behavior, "")
			if tc.setup != nil {
				tc.setup(e, dir)
			}
			var out strings.Builder
			err := e.manager().Start(bg, "a", &out)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v\n%s", err, tc.want, out.String())
			}
			if e.screen.launched("a") != tc.launches || e.screen.running("a") {
				t.Fatalf("%d launches, running %v", e.screen.launched("a"), e.screen.running("a"))
			}
			// Intent is recorded even when the start fails, as in server_start.
			if !e.active("a") {
				t.Fatal("start did not mark the server active")
			}
		})
	}
}

// Two managers (two CLIs) starting one server at once launch it once: the
// server lock serializes them and the second sees it running.
func TestSimultaneousStarts(t *testing.T) {
	e := newEnv(t)
	e.server("a", behaveOK, "")
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = e.manager().Start(bg, "a", &strings.Builder{})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := e.screen.launched("a"); n != 1 {
		t.Fatalf("launched %d times", n)
	}
}

// A session that is not provably this server is never stopped or started
// over; the command fails instead.
func TestRefusesUnprovenSession(t *testing.T) {
	e := newEnv(t)
	e.server("a", behaveOK, "")
	e.run("a")
	e.screen.mu.Lock()
	e.screen.sessions["msm-a"].argv = []string{"bash"}
	e.screen.mu.Unlock()
	for name, op := range map[string]func() error{
		"stop":    func() error { return e.manager().Stop(bg, "a", true, &strings.Builder{}) },
		"start":   func() error { return e.manager().Start(bg, "a", &strings.Builder{}) },
		"restart": func() error { return e.manager().Restart(bg, "a", true, &strings.Builder{}) },
		"status":  func() error { return e.manager().Status(bg, "a", &strings.Builder{}) },
	} {
		if err := op(); !errors.Is(err, ErrNotProven) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(e.screen.lines("a")) != 0 || e.screen.launched("a") != 1 {
		t.Fatalf("touched the foreign session: %q", e.screen.lines("a"))
	}
}

func TestInvalidDelay(t *testing.T) {
	e := newEnv(t)
	e.server("a", behaveOK, "msm-stop-delay=soon\n")
	e.run("a")
	if err := e.manager().Stop(bg, "a", false, &strings.Builder{}); !errors.Is(err, ErrSettings) || !e.screen.running("a") {
		t.Fatalf("%v", err)
	}
}
