package testutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/process"
)

func TestRunner(t *testing.T) {
	wantErr := errors.New("fake failure")
	runner := NewRunner(process.Result{Stdout: "result", Stderr: "detail"}, wantErr)
	cmd := process.Command{Path: "not-an-executable", Args: []string{"literal; argument"}, Env: []string{"X=value"}}
	result, err := runner.Run(context.Background(), cmd)
	if !errors.Is(err, wantErr) || result.Stdout != "result" || result.Stderr != "detail" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	cmd.Args[0] = "changed"
	calls := runner.Calls()
	if calls[0].Args[0] != "literal; argument" {
		t.Fatal("caller mutated recorded arguments")
	}
	calls[0].Env[0] = "changed"
	if runner.Calls()[0].Env[0] != "X=value" {
		t.Fatal("returned calls alias internal state")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runner.Run(ctx, cmd); !errors.Is(err, context.Canceled) || len(runner.Calls()) != 1 {
		t.Fatal("canceled command must not be recorded")
	}
}

func TestRunnerConcurrent(t *testing.T) {
	runner := NewRunner(process.Result{}, nil)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, _ = runner.Run(context.Background(), process.Command{Path: "fixture"})
			_ = runner.Calls()
		})
	}
	wg.Wait()
	if len(runner.Calls()) != 20 {
		t.Fatal("lost calls")
	}
}

func TestClock(t *testing.T) {
	start := time.Unix(0, 0)
	c := NewClock(start)
	done := make(chan error, 1)
	go func() { done <- c.Wait(context.Background(), time.Minute) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-c.WaitStarted():
	case <-ctx.Done():
		t.Fatal("waiter did not register")
	}
	c.Advance(30 * time.Second)
	select {
	case <-done:
		t.Fatal("wait completed before its deadline")
	default:
	}
	c.Advance(30 * time.Second)
	select {
	case err := <-done:
		if err != nil || !c.Now().Equal(start.Add(time.Minute)) {
			t.Fatalf("wait: %v; now=%v", err, c.Now())
		}
	case <-ctx.Done():
		t.Fatal("fake clock waiter did not finish")
	}
}

func TestClockCancellationAndZeroWait(t *testing.T) {
	c := NewClock(time.Unix(0, 0))
	if err := c.Wait(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal("expected cancellation")
	}
}

func TestClockCancelWhileWaiting(t *testing.T) {
	c := NewClock(time.Unix(0, 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Wait(ctx, time.Hour) }()
	guard := time.NewTimer(5 * time.Second)
	defer guard.Stop()
	select {
	case <-c.WaitStarted():
	case <-guard.C:
		t.Fatal("waiter did not register")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("expected cancellation")
		}
	case <-guard.C:
		t.Fatal("waiter did not cancel")
	}
}

func TestFixtureIsolation(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "msm.conf"), []byte("ROOT=__FIXTURE_ROOT__"), 0600); err != nil {
		t.Fatal(err)
	}
	first, second := CopyFixture(t, source), CopyFixture(t, source)
	data, err := os.ReadFile(filepath.Join(first, "msm.conf"))
	if err != nil || !strings.Contains(string(data), first) {
		t.Fatalf("fixture: %q %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(first, "sentinel"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(second, "sentinel")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixtures are not isolated")
	}
}
