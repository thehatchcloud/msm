package maintenance

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/filelock"
)

func TestParseJob(t *testing.T) {
	for _, j := range Jobs {
		if got, err := ParseJob(string(j)); err != nil || got != j {
			t.Errorf("ParseJob(%q) = %q, %v", j, got, err)
		}
	}
	if _, err := ParseJob("cleanup"); err == nil || !strings.Contains(err.Error(), "backup, logroll, todisk, recover, prune") {
		t.Errorf("err = %v", err)
	}
}

// Every job stops before the legacy cadence would start it again.
func TestDefaultTimeoutsFitTheirCadence(t *testing.T) {
	if JobToDisk.DefaultTimeout() >= 30*time.Minute {
		t.Error("todisk runs every 30 minutes and must end first")
	}
	if JobRecover.DefaultTimeout() > time.Hour || JobLogroll.DefaultTimeout() > 24*time.Hour {
		t.Error("a timeout exceeds its cadence")
	}
	for _, j := range Jobs {
		if j.DefaultTimeout() <= 0 {
			t.Errorf("%s has no timeout", j)
		}
	}
}

func TestRunRunsTheJobAndReleasesItsLock(t *testing.T) {
	e := newEnv(t, "")
	var log bytes.Buffer
	cfg := RunConfig{Servers: e.sm, Log: &log}
	for i := 0; i < 2; i++ {
		ran := false
		if err := Run(t.Context(), cfg, JobPrune, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
			t.Fatalf("run %d: ran=%v err=%v", i, ran, err)
		}
	}
	if !strings.Contains(log.String(), "maintenance prune: euid=") || !strings.Contains(log.String(), "timeout=1h0m0s") {
		t.Errorf("start line:\n%s", log.String())
	}
}

func TestRunSkipsAnOverlappingRunOfTheSameJob(t *testing.T) {
	e := newEnv(t, "")
	var skipped bytes.Buffer
	cfg := RunConfig{Servers: e.sm, Skipped: &skipped}
	inner := make(chan error, 1)
	started, release := make(chan struct{}), make(chan struct{})
	go func() {
		inner <- Run(t.Context(), cfg, JobBackup, func(context.Context) error { close(started); <-release; return nil })
	}()
	<-started
	ran := false
	if err := Run(t.Context(), cfg, JobBackup, func(context.Context) error { ran = true; return nil }); err != nil {
		t.Fatalf("an overlapping run must be skipped successfully: %v", err)
	}
	if ran {
		t.Error("an overlapping run of the same job ran")
	}
	if !strings.Contains(skipped.String(), "maintenance backup: skipped, the previous run is still going") {
		t.Errorf("skip note:\n%s", skipped.String())
	}
	// A different job is not excluded.
	other := false
	if err := Run(t.Context(), cfg, JobLogroll, func(context.Context) error { other = true; return nil }); err != nil || !other {
		t.Errorf("another job: ran=%v err=%v", other, err)
	}
	close(release)
	if err := <-inner; err != nil {
		t.Fatal(err)
	}
	again := false
	if err := Run(t.Context(), cfg, JobBackup, func(context.Context) error { again = true; return nil }); err != nil || !again {
		t.Errorf("after the first run ended: ran=%v err=%v", again, err)
	}
}

func TestRunPropagatesTheJobsError(t *testing.T) {
	e := newEnv(t, "")
	boom := errors.New("boom")
	if err := Run(t.Context(), RunConfig{Servers: e.sm}, JobPrune, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunEndsAJobThatHonorsItsTimeout(t *testing.T) {
	e := newEnv(t, "")
	cfg := RunConfig{Servers: e.sm, Timeout: 50 * time.Millisecond}
	err := Run(t.Context(), cfg, JobBackup, func(ctx context.Context) error { <-ctx.Done(); return context.Cause(ctx) })
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestRunAbandonsAJobThatIgnoresItsTimeout(t *testing.T) {
	e := newEnv(t, "")
	cfg := RunConfig{Servers: e.sm, Timeout: 30 * time.Millisecond, Grace: 30 * time.Millisecond}
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	err := Run(t.Context(), cfg, JobBackup, func(context.Context) error { <-block; return nil })
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("err = %v, want ErrTimeout naming the stuck job", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the stuck job was waited for")
	}
}

func TestRunRejectsANegativeTimeout(t *testing.T) {
	e := newEnv(t, "")
	if err := Run(t.Context(), RunConfig{Servers: e.sm, Timeout: -1}, JobBackup, func(context.Context) error { return nil }); err == nil {
		t.Fatal("a negative timeout was accepted")
	}
}

func TestRunWithoutAStorageRoot(t *testing.T) {
	e := newEnv(t, "")
	if err := removeAll(e.root); err != nil {
		t.Fatal(err)
	}
	err := Run(t.Context(), RunConfig{Servers: e.sm}, JobPrune, func(context.Context) error { return nil })
	if err == nil || errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("err = %v, want a missing-root error", err)
	}
}
