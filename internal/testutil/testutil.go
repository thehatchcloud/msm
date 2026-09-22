// Package testutil contains deterministic test helpers. It is not imported by
// the application binary and never launches Java, screen, or the Bash manager.
package testutil

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/process"
)

// Runner records argument vectors and returns preconfigured results.
type Runner struct {
	mu     sync.Mutex
	calls  []process.Command
	result process.Result
	err    error
}

func NewRunner(result process.Result, err error) *Runner {
	return &Runner{result: result, err: err}
}

func (r *Runner) Run(ctx context.Context, cmd process.Command) (process.Result, error) {
	if err := ctx.Err(); err != nil {
		return process.Result{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cmd.Args = append([]string(nil), cmd.Args...)
	cmd.Env = append([]string(nil), cmd.Env...)
	r.calls = append(r.calls, cmd)
	return r.result, r.err
}

func (r *Runner) Calls() []process.Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]process.Command(nil), r.calls...)
	for i := range out {
		out[i].Args = append([]string(nil), out[i].Args...)
		out[i].Env = append([]string(nil), out[i].Env...)
	}
	return out
}

// Clock advances only when the test calls Advance; Wait never sleeps in real
// time. Broadcasting changes lets every waiter re-check its deadline.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	changed chan struct{}
	started chan struct{}
}

func NewClock(now time.Time) *Clock {
	return &Clock{now: now, changed: make(chan struct{}), started: make(chan struct{}, 1)}
}

// WaitStarted signals when a wait has registered, allowing tests to advance
// only after the goroutine is ready. Notifications are coalesced.
func (c *Clock) WaitStarted() <-chan struct{} { return c.started }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Advance(d time.Duration) {
	if d < 0 {
		panic("testutil.Clock cannot go backwards")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *Clock) Wait(ctx context.Context, delay time.Duration) error {
	c.mu.Lock()
	deadline := c.now.Add(delay)
	select {
	case c.started <- struct{}{}:
	default:
	}
	for {
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return err
		}
		if !c.now.Before(deadline) {
			c.mu.Unlock()
			return nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		c.mu.Lock()
	}
}

var (
	_ process.Runner = (*Runner)(nil)
	_ clock.Clock    = (*Clock)(nil)
)

// CopyFixture copies inert regular files into a fresh t.TempDir. It deliberately
// rejects symlinks/special files; runtime symlink cases must be created by tests.
func CopyFixture(t testing.TB, source string) string {
	t.Helper()
	root := t.TempDir()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("fixture must contain only directories and regular files")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if filepath.Base(path) == "msm.conf" {
			data = []byte(strings.ReplaceAll(string(data), "__FIXTURE_ROOT__", root))
		}
		return os.WriteFile(dest, data, 0600)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return root
}
