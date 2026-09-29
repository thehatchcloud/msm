package lifecycle

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func output() (Output, *strings.Builder) {
	var b strings.Builder
	return Output{Out: &b, Err: &b}, &b
}

// CT-CMD-001: the global start starts only active, stopped servers and
// changes no intent.
func TestStartAll(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		e.server(n, behaveOK, "")
	}
	e.setActive("a", true)
	e.setActive("b", true)
	e.run("b")
	e.run("d") // inactive but running
	out, text := output()
	if err := e.manager().StartAll(bg, out); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if e.screen.launched("a") != 1 || e.screen.launched("b") != 1 || e.screen.launched("c") != 0 || e.screen.launched("d") != 1 {
		t.Fatalf("launches a%d b%d c%d d%d", e.screen.launched("a"), e.screen.launched("b"), e.screen.launched("c"), e.screen.launched("d"))
	}
	if !e.active("a") || !e.active("b") || e.active("c") || e.active("d") {
		t.Fatal("global start changed intent")
	}
	for _, want := range []string{
		`a: [ACTIVE] Server "a" starting:`, `b: [ACTIVE] Server "b" already started.`,
		`c: [INACTIVE] Server "c" leaving stopped`, `d: [INACTIVE] Server "d" already started. It should not be running!`,
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
}

// CT-CMD-002, CT-CMD-003, and the difference from msm all stop: the global
// stop stops every running server, even an inactive one, and preserves
// intent; msm all stop runs the per-server stop, which marks every server
// inactive.
func TestGlobalStopIsNotAllStop(t *testing.T) {
	setup := func() *env {
		e := newEnv(t)
		for _, n := range []string{"a", "b", "c"} {
			e.server(n, behaveOK, "")
		}
		e.setActive("a", true)
		e.setActive("c", true)
		e.run("a")
		e.run("b")
		return e
	}

	e := setup()
	out, text := output()
	if err := e.manager().StopAll(bg, false, out); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if e.screen.running("a") || e.screen.running("b") {
		t.Fatal("global stop left a server running")
	}
	if !e.active("a") || e.active("b") || !e.active("c") {
		t.Fatal("global stop changed intent")
	}
	for _, n := range []string{"a", "b"} {
		if got := e.screen.lines(n); !slices.Equal(got, []string{"say SERVER SHUTTING DOWN IN 10 SECONDS!", "save-all", "stop"}) {
			t.Fatalf("%s: %q", n, got)
		}
	}
	if !strings.Contains(text.String(), `c: Server "c" was NOT running.`) {
		t.Fatalf("output:\n%s", text)
	}

	e = setup()
	out, text = output()
	if err := e.manager().Each(bg, VerbStop, true, out); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if e.screen.running("a") || e.screen.running("b") {
		t.Fatal("all stop left a server running")
	}
	if e.active("a") || e.active("b") || e.active("c") {
		t.Fatal("all stop did not mark every server inactive")
	}
	for _, n := range []string{"a", "b"} {
		if got := e.screen.lines(n); !slices.Equal(got, []string{"save-all", "stop"}) {
			t.Fatalf("%s: stop now sent %q", n, got)
		}
	}

	e = newEnv(t)
	e.server("a", behaveOK, "")
	out, text = output()
	if err := e.manager().StopAll(bg, true, out); err != nil || !strings.Contains(text.String(), "No servers were running.") {
		t.Fatalf("%v\n%s", err, text)
	}
}

// CT-CMD-004, CT-CMD-005: the global restart stops every running server,
// then starts only the active set, and changes no intent.
func TestRestartAll(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"a", "b", "c"} {
		e.server(n, behaveOK, "")
	}
	e.setActive("a", true)
	e.setActive("c", true)
	e.run("a")
	e.run("b")
	out, text := output()
	if err := e.manager().RestartAll(bg, false, out); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if !e.screen.running("a") || e.screen.running("b") || !e.screen.running("c") {
		t.Fatalf("running a%v b%v c%v", e.screen.running("a"), e.screen.running("b"), e.screen.running("c"))
	}
	if e.screen.launched("a") != 2 || e.screen.launched("b") != 1 || e.screen.launched("c") != 1 {
		t.Fatal("wrong launches")
	}
	if !e.active("a") || e.active("b") || !e.active("c") {
		t.Fatal("global restart changed intent")
	}
	if got := e.screen.lines("a"); !slices.Equal(got, []string{"say SERVER REBOOT IN 10 SECONDS!", "save-all", "stop"}) {
		t.Fatalf("a: %q", got)
	}
	s := text.String()
	if i, j := strings.Index(s, "Stopping servers:"), strings.Index(s, "Starting servers:"); i < 0 || j < i {
		t.Fatalf("phases out of order:\n%s", s)
	}
}

// Partial bulk failure: every server is attempted, the successful ones
// finish, and the aggregate error lists the failures in name order with
// the same text whatever the concurrency.
func TestBulkPartialFailure(t *testing.T) {
	var messages []string
	for _, jobs := range []int{1, 2, 8} {
		e := newEnv(t)
		e.jobs = jobs
		e.server("d", behaveOK, "")
		e.server("c", behaveEULA, "")
		e.server("b", behaveCrash, "")
		e.server("a", behavePort, "")
		for _, n := range []string{"a", "b", "c", "d"} {
			e.setActive(n, true)
		}
		out, text := output()
		err := e.manager().StartAll(bg, out)
		var bulk *BulkError
		if !errors.As(err, &bulk) || bulk.Total != 4 || len(bulk.Failed) != 3 {
			t.Fatalf("jobs %d: %v\n%s", jobs, err, text)
		}
		if !errors.Is(err, ErrPortInUse) || !errors.Is(err, ErrExited) || !errors.Is(err, ErrEULA) {
			t.Fatalf("jobs %d: causes lost: %v", jobs, err)
		}
		if names := []string{bulk.Failed[0].Name, bulk.Failed[1].Name, bulk.Failed[2].Name}; !slices.Equal(names, []string{"a", "b", "c"}) {
			t.Fatalf("jobs %d: failures %v", jobs, names)
		}
		if !e.screen.running("d") {
			t.Fatalf("jobs %d: the healthy server did not start", jobs)
		}
		// Temporary paths differ per run; compare the rest.
		messages = append(messages, strings.ReplaceAll(err.Error(), e.root, "ROOT"))
	}
	if messages[0] != messages[1] || messages[1] != messages[2] {
		t.Fatalf("aggregate differs with concurrency:\n%s\n---\n%s", messages[0], messages[2])
	}
}

// Bulk work is bounded: never more than Jobs servers at once.
func TestBulkConcurrencyIsBounded(t *testing.T) {
	e := newEnv(t)
	e.jobs = 2
	names := []string{"a", "b", "c", "d", "e"}
	for _, n := range names {
		e.server(n, behaveOK, "")
		e.run(n)
	}
	m := e.manager()
	var mu = make(chan struct{}, 1)
	current, peak := 0, 0
	errs := m.forEach(bg, names, &strings.Builder{}, func(ctx context.Context, s *server) error {
		mu <- struct{}{}
		current++
		peak = max(peak, current)
		<-mu
		time.Sleep(5 * time.Millisecond)
		mu <- struct{}{}
		current--
		<-mu
		return nil
	})
	if aggregate("x", names, errs) != nil || peak < 1 || peak > 2 {
		t.Fatalf("peak %d, errs %v", peak, errs)
	}
}

// Ctrl+C during a global stop countdown aborts every countdown, stops
// nothing, and makes the whole command fail.
func TestStopAllAbort(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"a", "b"} {
		e.server(n, behaveOK, "")
		e.run(n)
	}
	ctx, cancel := context.WithCancel(bg)
	e.clock.onWait = func(d time.Duration) {
		if d == 10*time.Second {
			cancel()
		}
	}
	out, text := output()
	err := e.manager().RestartAll(ctx, false, out)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("%v\n%s", err, text)
	}
	for _, n := range []string{"a", "b"} {
		lines := e.screen.lines(n)
		if !e.screen.running(n) || slices.Contains(lines, "stop") || e.screen.launched(n) != 1 {
			t.Fatalf("%s: running %v lines %q", n, e.screen.running(n), lines)
		}
	}
	if strings.Contains(text.String(), "Starting servers:") {
		t.Fatal("restart went on to start servers after an abort")
	}
}

// A server that crashed is stopped, and the global start brings back an
// active crashed server.
func TestCrashedServerIsRestarted(t *testing.T) {
	e := newEnv(t)
	e.server("a", behaveOK, "")
	e.setActive("a", true)
	e.run("a")
	e.screen.crash("a")
	out, text := output()
	if err := e.manager().StartAll(bg, out); err != nil || !e.screen.running("a") || e.screen.launched("a") != 2 {
		t.Fatalf("%v\n%s", err, text)
	}
}
