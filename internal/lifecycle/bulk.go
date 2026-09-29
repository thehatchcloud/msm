package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// BulkError is the aggregate result of an operation on several servers.
// Its message lists every failed server in name order, so the result does
// not depend on which server finished first.
type BulkError struct {
	Op     string
	Total  int
	Failed []ServerError
}

// ServerError is one server's failure in a bulk operation.
type ServerError struct {
	Name string
	Err  error
}

func (e *BulkError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d of %d servers failed", e.Op, len(e.Failed), e.Total)
	for _, f := range e.Failed {
		fmt.Fprintf(&b, "\n  %s: %v", f.Name, f.Err)
	}
	return b.String()
}

func (e *BulkError) Unwrap() []error {
	errs := make([]error, len(e.Failed))
	for i, f := range e.Failed {
		errs[i] = f.Err
	}
	return errs
}

// Output is where bulk operations report: progress lines on Out, each
// prefixed with its server's name, and listing warnings on Err.
type Output struct {
	Out, Err io.Writer
}

// names lists the servers, reporting listing warnings.
func (m *Manager) names(ctx context.Context, out Output) ([]string, error) {
	entries, warnings, err := m.cfg.Servers.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		fmt.Fprintf(out.Err, "msm: warning: %s\n", w)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names, nil
}

// forEach runs fn on each named server, at most Jobs at a time, each under
// its own server lock, and returns one error per server in names order. A
// server not yet begun when ctx is canceled is not attempted.
func (m *Manager) forEach(ctx context.Context, names []string, out io.Writer, fn func(context.Context, *server) error) []error {
	return m.each(ctx, names, out, true, fn)
}

// each is forEach, optionally without the server locks, for reports that
// change nothing and should not wait behind another operation.
func (m *Manager) each(ctx context.Context, names []string, out io.Writer, lock bool, fn func(context.Context, *server) error) []error {
	errs := make([]error, len(names))
	sem := make(chan struct{}, m.cfg.Jobs)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				errs[i] = fmt.Errorf("%w: server %q was not attempted", ErrInterrupted, name)
				return
			}
			w := &lineWriter{mu: &mu, out: out, prefix: name + ": "}
			defer w.flush()
			var s *server
			if lock {
				l, srv, err := m.open(name, w)
				if err != nil {
					errs[i] = err
					return
				}
				defer l.Release()
				s = srv
			} else {
				settings, err := m.cfg.Servers.Settings(name)
				if err == nil {
					s, err = m.prepare(settings, w)
				}
				if err != nil {
					errs[i] = err
					return
				}
			}
			errs[i] = fn(ctx, s)
		}()
	}
	wg.Wait()
	return errs
}

func aggregate(op string, names []string, errs []error) error {
	e := &BulkError{Op: op, Total: len(names)}
	for i, err := range errs {
		if err != nil {
			e.Failed = append(e.Failed, ServerError{Name: names[i], Err: err})
		}
	}
	if len(e.Failed) == 0 {
		return nil
	}
	sort.Slice(e.Failed, func(i, j int) bool { return e.Failed[i].Name < e.Failed[j].Name })
	return e
}

// StartAll starts every active server that is stopped and leaves intent
// unchanged (msm start).
func (m *Manager) StartAll(ctx context.Context, out Output) error {
	names, err := m.names(ctx, out)
	if err != nil {
		return err
	}
	return m.startAll(ctx, names, out)
}

func (m *Manager) startAll(ctx context.Context, names []string, out Output) error {
	errs := m.forEach(ctx, names, out.Out, func(ctx context.Context, s *server) error {
		active := isActive(s.settings.Get("FLAG_ACTIVE_PATH"))
		st, err := s.state(ctx)
		if err != nil {
			return err
		}
		switch {
		case active && running(st):
			s.printf("[ACTIVE] Server %q already started.", s.name)
		case active:
			s.printf("[ACTIVE] Server %q starting:", s.name)
			return m.launch(ctx, s)
		case running(st):
			s.printf("[INACTIVE] Server %q already started. It should not be running! Use \"msm %s stop\" to stop this server.", s.name, s.name)
		default:
			s.printf("[INACTIVE] Server %q leaving stopped, as this server is inactive.", s.name)
		}
		return nil
	})
	return aggregate("start", names, errs)
}

// StopAll stops every running server, active or not, and leaves intent
// unchanged (msm stop [now]).
func (m *Manager) StopAll(ctx context.Context, now bool, out Output) error {
	names, err := m.names(ctx, out)
	if err != nil {
		return err
	}
	_, errs := m.stopAll(ctx, names, now, false, out)
	return aggregate("stop", names, errs)
}

// stopAll warns and stops the running servers. restart selects the restart
// messages and delay. It reports which servers were running.
func (m *Manager) stopAll(ctx context.Context, names []string, now, restart bool, out Output) ([]bool, []error) {
	wasRunning := make([]bool, len(names))
	index := make(map[string]int, len(names))
	for i, n := range names {
		index[n] = i
	}
	var mu sync.Mutex
	errs := m.forEach(ctx, names, out.Out, func(ctx context.Context, s *server) error {
		st, err := s.state(ctx)
		if err != nil {
			return err
		}
		if !running(st) {
			s.printf("Server %q was NOT running.", s.name)
			return nil
		}
		mu.Lock()
		wasRunning[index[s.name]] = true
		mu.Unlock()
		s.printf("Server %q was running, now stopping.", s.name)
		if !now {
			if st, err = m.countdown(ctx, s, st, restart); err != nil || !running(st) {
				return err
			}
		}
		return m.halt(ctx, s, st)
	})
	anyRunning := false
	for _, r := range wasRunning {
		anyRunning = anyRunning || r
	}
	if !anyRunning && ctx.Err() == nil {
		fmt.Fprintln(out.Out, "No servers were running.")
	}
	return wasRunning, errs
}

// RestartAll stops every running server, then starts the active ones,
// leaving intent unchanged (msm restart [now]). A server that failed to
// stop is not started; if the stop phase is interrupted, nothing starts.
func (m *Manager) RestartAll(ctx context.Context, now bool, out Output) error {
	names, err := m.names(ctx, out)
	if err != nil {
		return err
	}
	fmt.Fprintln(out.Out, "Stopping servers:")
	_, stopErrs := m.stopAll(ctx, names, now, true, out)
	if ctx.Err() != nil {
		return aggregate("restart", names, stopErrs)
	}
	var startNames []string
	for i, n := range names {
		if stopErrs[i] == nil {
			startNames = append(startNames, n)
		}
	}
	fmt.Fprintln(out.Out, "Starting servers:")
	startErr := m.startAll(ctx, startNames, out)
	errs := append([]error(nil), stopErrs...)
	var bulk *BulkError
	if errors.As(startErr, &bulk) {
		for _, f := range bulk.Failed {
			for i, n := range names {
				if n == f.Name {
					errs[i] = f.Err
				}
			}
		}
	}
	return aggregate("restart", names, errs)
}

// Verb is a per-server command that the "all" target applies to every
// server.
type Verb int

const (
	VerbStart Verb = iota
	VerbStop
	VerbRestart
	VerbStatus
)

func (v Verb) String() string { return [...]string{"start", "stop", "restart", "status"}[v] }

// Each applies a per-server command to every server (msm all <verb>).
// Unlike the global commands it changes intent exactly as the per-server
// command does: msm all stop marks every server inactive, where msm stop
// leaves intent alone.
func (m *Manager) Each(ctx context.Context, verb Verb, now bool, out Output) error {
	names, err := m.names(ctx, out)
	if err != nil {
		return err
	}
	errs := m.each(ctx, names, out.Out, verb != VerbStatus, func(ctx context.Context, s *server) error {
		switch verb {
		case VerbStart:
			return m.startServer(ctx, s)
		case VerbStop:
			return m.stopServer(ctx, s, now)
		case VerbRestart:
			return m.restartServer(ctx, s, now)
		default:
			return s.status(ctx)
		}
	})
	return aggregate(verb.String(), names, errs)
}

// lineWriter passes whole lines to a shared writer with a prefix, so
// concurrent servers' progress never interleaves within a line.
type lineWriter struct {
	mu     *sync.Mutex
	out    io.Writer
	prefix string
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		w.emit(w.buf[:i+1])
		w.buf = w.buf[i+1:]
	}
}

func (w *lineWriter) emit(line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	io.WriteString(w.out, w.prefix)
	w.out.Write(line)
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(append(w.buf, '\n'))
		w.buf = nil
	}
}
