// Package lifecycle starts, stops, restarts and reports on servers, one at
// a time or in bulk, following the legacy manager's intent rules
// (docs/compatibility/README.md, "Active state and lifecycle"):
//
//   - msm <server> start and restart mark the server active; msm <server>
//     stop marks it inactive. The global msm start, stop and restart never
//     change intent: start and restart start only the active servers, stop
//     stops every running one.
//   - "now" skips the warning and countdown only. Every stop still sends
//     save-all, then stop, and waits for the process to end.
//   - Readiness is a fresh start line in the server's log, never an old one
//     (DEV-010), and every wait has a deadline. At a deadline the command
//     fails and says what is still running; nothing is ever killed.
//   - A countdown canceled (Ctrl+C) sends the abort message, leaves the
//     server running and leaves its intent unchanged.
//
// Each server operation holds that server's lock, so two managers never
// act on one server at once; bulk operations work on several servers
// concurrently, up to a bound, and report a deterministic aggregate result.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/profiles"
	"github.com/thehatchcloud/msm/internal/screen"
	"github.com/thehatchcloud/msm/internal/servers"
)

var (
	ErrNoScreen     = errors.New("lifecycle: GNU screen is not installed")
	ErrNoJava       = errors.New("lifecycle: Java is not available")
	ErrNoJAR        = errors.New("lifecycle: server JAR is missing")
	ErrInvalidJAR   = errors.New("lifecycle: server JAR is invalid")
	ErrEULA         = errors.New("lifecycle: the Minecraft EULA has not been accepted")
	ErrPortInUse    = errors.New("lifecycle: the server's port is in use")
	ErrExited       = errors.New("lifecycle: server exited during startup")
	ErrNotReady     = errors.New("lifecycle: server did not report that it started")
	ErrStillRunning = errors.New("lifecycle: server did not stop")
	ErrNotProven    = errors.New("lifecycle: server state cannot be proven")
	ErrAborted      = errors.New("lifecycle: aborted")
	ErrInterrupted  = errors.New("lifecycle: interrupted")
	ErrSettings     = errors.New("lifecycle: invalid server setting")
)

const (
	// DefaultTimeout bounds waiting for a server to become ready or to
	// stop, unless Config.Timeout says otherwise.
	DefaultTimeout = 5 * time.Minute
	// DefaultJobs bounds how many servers a bulk operation handles at once.
	DefaultJobs = 4

	launchTimeout = 15 * time.Second
	// messageTimeout bounds sending the abort message after Ctrl+C.
	messageTimeout = 10 * time.Second
	logPoll        = 250 * time.Millisecond
)

// Sessions is the part of the screen backend lifecycle uses.
type Sessions interface {
	Status(ctx context.Context, name string, want []string) (screen.Status, error)
	Launch(ctx context.Context, spec screen.LaunchSpec) (screen.Status, error)
	Send(ctx context.Context, st screen.Status, line string) error
	WaitStopped(ctx context.Context, name string, want []string, timeout time.Duration) (screen.Status, error)
	WaitReady(ctx context.Context, name string, want []string, timeout time.Duration, probe screen.Probe) (screen.Status, error)
}

var _ Sessions = (*screen.Backend)(nil)

// Config connects lifecycle to the host.
type Config struct {
	Servers *servers.Manager
	// Sessions returns the screen backend for a server owner, or
	// ErrNoScreen when screen is not installed.
	Sessions func(owner identity.Identity) (Sessions, error)
	// Path is the PATH screen uses to find the invocation's program.
	Path  string
	Clock clock.Clock
	// Timeout, when set, replaces the readiness and stop deadlines.
	Timeout time.Duration
	// Jobs bounds bulk concurrency; zero means DefaultJobs.
	Jobs int
	// EUID is the effective user ID; nil means ask the OS.
	EUID *int
}

// Manager runs lifecycle operations.
type Manager struct {
	cfg  Config
	euid int
}

func New(cfg Config) (*Manager, error) {
	if cfg.Servers == nil || cfg.Sessions == nil || cfg.Clock == nil {
		return nil, errors.New("lifecycle: servers, sessions and clock are required")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("lifecycle: timeout must not be negative")
	}
	if cfg.Jobs <= 0 {
		cfg.Jobs = DefaultJobs
	}
	m := &Manager{cfg: cfg, euid: os.Geteuid()}
	if cfg.EUID != nil {
		m.euid = *cfg.EUID
	}
	return m, nil
}

// server is one locked server and what is needed to act on it.
type server struct {
	name     string
	dir      string
	settings *legacyconf.ServerSettings
	owner    identity.Identity
	profile  profiles.Profile
	argv     []string
	argvErr  error
	sessions Sessions // nil when screen is not installed
	screen   string
	out      io.Writer
}

// open locks a server and resolves everything an operation needs. The
// caller releases the lock.
func (m *Manager) open(name string, out io.Writer) (*filelock.Lock, *server, error) {
	lock, s, err := m.cfg.Servers.LockServer(name)
	if err != nil {
		return nil, nil, err
	}
	srv, err := m.prepare(s, out)
	if err != nil {
		lock.Release()
		return nil, nil, err
	}
	return lock, srv, nil
}

func (m *Manager) prepare(s *legacyconf.ServerSettings, out io.Writer) (*server, error) {
	owner, err := m.cfg.Servers.Owner(s)
	if err != nil {
		return nil, err
	}
	srv := &server{name: s.Name, dir: s.Dir, settings: s, owner: owner,
		screen: s.Get("SCREEN_NAME"), out: out}
	var note string
	srv.profile, note = profiles.Select(s.Get("VERSION"))
	_ = note // the fallback is the legacy default; P08 reports it
	srv.argv, srv.argvErr = s.Invocation()
	srv.sessions, err = m.cfg.Sessions(owner)
	if err != nil && !errors.Is(err, ErrNoScreen) {
		return nil, fmt.Errorf("server %q: %w", s.Name, err)
	}
	return srv, nil
}

func (s *server) printf(format string, args ...any) {
	fmt.Fprintf(s.out, format+"\n", args...)
}

// state observes the server's session. A session that is not provably
// this server's process, or sessions that cannot be inspected, fail with
// ErrNotProven.
func (s *server) state(ctx context.Context) (screen.Status, error) {
	if s.sessions == nil {
		return screen.Status{Name: s.screen}, nil
	}
	st, err := s.sessions.Status(ctx, s.screen, s.argv)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return st, ctxErr
	}
	if err != nil {
		return st, fmt.Errorf("%w: server %q: %v", ErrNotProven, s.name, err)
	}
	switch {
	case st.Session == nil || st.Liveness == screen.Running && s.argvErr == nil:
		return st, nil
	case s.argvErr != nil:
		return st, fmt.Errorf("%w: server %q: screen session %s cannot be matched to the server: %v", ErrNotProven, s.name, st.Session.ID, s.argvErr)
	default:
		return st, fmt.Errorf("%w: server %q: screen session %s is %s (%s)", ErrNotProven, s.name, st.Session.ID, st.Liveness, st.Detail)
	}
}

func running(st screen.Status) bool { return st.Session != nil && st.Liveness == screen.Running }

func (m *Manager) setActive(s *server, active bool) error {
	return setActive(m.cfg.Servers.Root(), s.name, s.settings.Get("FLAG_ACTIVE_PATH"), s.owner, m.euid, active)
}

// delay parses STOP_DELAY or RESTART_DELAY, whole seconds.
func (s *server) delay(setting string) (time.Duration, error) {
	v := s.settings.Get(setting)
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 86400 {
		return 0, fmt.Errorf("%w: server %q %s %q (from %s) must be a whole number of seconds from 0 to 86400",
			ErrSettings, s.name, setting, v, s.settings.Source(setting))
	}
	return time.Duration(n) * time.Second, nil
}

func (m *Manager) readyTimeout(s *server) time.Duration {
	if m.cfg.Timeout > 0 {
		return m.cfg.Timeout
	}
	return max(DefaultTimeout, s.profile.Start.Timeout)
}

func (m *Manager) stopTimeout() time.Duration {
	if m.cfg.Timeout > 0 {
		return m.cfg.Timeout
	}
	return DefaultTimeout
}

// Start marks a server active and starts it (msm <server> start).
func (m *Manager) Start(ctx context.Context, name string, out io.Writer) error {
	lock, s, err := m.open(name, out)
	if err != nil {
		return err
	}
	defer lock.Release()
	return m.startServer(ctx, s)
}

func (m *Manager) startServer(ctx context.Context, s *server) error {
	if err := m.setActive(s, true); err != nil {
		return err
	}
	return m.launch(ctx, s)
}

// Stop stops a server and marks it inactive (msm <server> stop [now]).
func (m *Manager) Stop(ctx context.Context, name string, now bool, out io.Writer) error {
	lock, s, err := m.open(name, out)
	if err != nil {
		return err
	}
	defer lock.Release()
	return m.stopServer(ctx, s, now)
}

func (m *Manager) stopServer(ctx context.Context, s *server, now bool) error {
	st, err := s.state(ctx)
	if err != nil {
		return err
	}
	if !running(st) {
		if err := m.setActive(s, false); err != nil {
			return err
		}
		s.printf("Server %q is not running.", s.name)
		return nil
	}
	if !now {
		if st, err = m.countdown(ctx, s, st, false); err != nil || !running(st) {
			if err == nil {
				err = m.setActive(s, false)
			}
			return err
		}
	}
	if err := m.setActive(s, false); err != nil {
		return err
	}
	return m.halt(ctx, s, st)
}

// Restart marks a server active, stops it if it is running and starts it
// (msm <server> restart [now]).
func (m *Manager) Restart(ctx context.Context, name string, now bool, out io.Writer) error {
	lock, s, err := m.open(name, out)
	if err != nil {
		return err
	}
	defer lock.Release()
	return m.restartServer(ctx, s, now)
}

func (m *Manager) restartServer(ctx context.Context, s *server, now bool) error {
	st, err := s.state(ctx)
	if err != nil {
		return err
	}
	if running(st) && !now {
		if st, err = m.countdown(ctx, s, st, true); err != nil {
			return err
		}
	}
	if err := m.setActive(s, true); err != nil {
		return err
	}
	if running(st) {
		if err := m.halt(ctx, s, st); err != nil {
			return err
		}
	}
	return m.launch(ctx, s)
}

// Status reports whether a server is running (msm <server> status).
func (m *Manager) Status(ctx context.Context, name string, out io.Writer) error {
	s, err := m.cfg.Servers.Settings(name)
	if err != nil {
		return err
	}
	srv, err := m.prepare(s, out)
	if err != nil {
		return err
	}
	return srv.status(ctx)
}

func (s *server) status(ctx context.Context) error {
	st, err := s.state(ctx)
	if err != nil {
		return err
	}
	if running(st) {
		s.printf("Server %q is running.", s.name)
	} else {
		s.printf("Server %q is stopped.", s.name)
	}
	return nil
}

// launch starts a stopped server and waits until a fresh start line in its
// log says it is ready.
func (m *Manager) launch(ctx context.Context, s *server) error {
	st, err := s.state(ctx)
	if err != nil {
		return err
	}
	if running(st) {
		s.printf("Server %q is already running.", s.name)
		return nil
	}
	if s.sessions == nil {
		return fmt.Errorf("cannot start server %q: %w", s.name, ErrNoScreen)
	}
	if s.argvErr != nil {
		return fmt.Errorf("cannot start server %q: %w", s.name, s.argvErr)
	}
	if _, err := findProgram(s.argv[0], s.dir, m.cfg.Path); err != nil {
		return fmt.Errorf("cannot start server %q: %w", s.name, err)
	}
	if err := checkJAR(s.name, s.settings.Get("JAR_PATH")); err != nil {
		return err
	}
	ready, err := s.profile.Match(s.profile.Start.Pattern)
	if err != nil {
		return err
	}
	log, err := watchLog(s.settings.Get("LOG_PATH"))
	if err != nil {
		return err
	}
	defer log.close()

	s.printf("Starting server %q...", s.name)
	_, err = s.sessions.Launch(ctx, screen.LaunchSpec{Name: s.screen, Dir: s.dir, Argv: s.argv, Timeout: launchTimeout})
	if err == nil {
		timeout := m.readyTimeout(s)
		_, err = s.sessions.WaitReady(ctx, s.screen, s.argv, timeout, func(context.Context) (bool, error) {
			return log.poll(ready)
		})
		if errors.Is(err, screen.ErrReadyTimeout) {
			return fmt.Errorf("%w: server %q is running but did not log its start line within %s; it was left running (see %s)",
				ErrNotReady, s.name, timeout, s.settings.Get("LOG_PATH"))
		}
	}
	switch {
	case err == nil:
		s.printf("Server %q started.", s.name)
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("%w while server %q was starting; it may still be starting", ErrInterrupted, s.name)
	case errors.Is(err, screen.ErrExited):
		return s.diagnose(log, err)
	case errors.Is(err, screen.ErrStartTimeout):
		// A session that is gone means the invocation ended at once.
		if st, stErr := s.state(ctx); stErr == nil && st.Session == nil {
			return s.diagnose(log, err)
		}
		return fmt.Errorf("start server %q: %w", s.name, err)
	default:
		return fmt.Errorf("start server %q: %w", s.name, err)
	}
}

// diagnose explains a server that stopped during startup.
func (s *server) diagnose(log *logWatcher, cause error) error {
	_, _ = log.poll(nil)
	eula := eulaRejected(s.dir)
	port := ""
	for _, h := range log.hints {
		switch {
		case eulaHint.MatchString(h):
			eula = true
		case portHint.MatchString(h):
			port = h
		}
	}
	switch {
	case eula:
		return fmt.Errorf("%w: server %q stopped; read the EULA and set eula=true in %s", ErrEULA, s.name, filepath.Join(s.dir, "eula.txt"))
	case port != "":
		return fmt.Errorf("%w: server %q stopped because it could not bind its port; change server-port in its properties or stop what uses it: %s", ErrPortInUse, s.name, port)
	default:
		return fmt.Errorf("%w: server %q (%v); see %s", ErrExited, s.name, cause, s.settings.Get("LOG_PATH"))
	}
}

// countdown warns the players and waits STOP_DELAY (or RESTART_DELAY for a
// restart). If ctx is canceled first, it sends the abort message and fails
// with ErrAborted; the server keeps running.
func (m *Manager) countdown(ctx context.Context, s *server, st screen.Status, restart bool) (screen.Status, error) {
	delaySetting, message, abort, what := "STOP_DELAY", "MESSAGE_STOP", "MESSAGE_STOP_ABORT", "shutdown"
	if restart {
		delaySetting, message, abort, what = "RESTART_DELAY", "MESSAGE_RESTART", "MESSAGE_RESTART_ABORT", "restart"
	}
	delay, err := s.delay(delaySetting)
	if err != nil {
		return st, err
	}
	if err := s.sessions.Send(ctx, st, "say "+s.settings.Get(message)); err != nil {
		return st, fmt.Errorf("warn players on server %q: %w", s.name, err)
	}
	s.printf("Issued the warning %q to players on server %q.", s.settings.Get(message), s.name)
	return m.wait(ctx, s, st, delay, what, abort)
}

// wait is the countdown itself, shared with the global stop.
func (m *Manager) wait(ctx context.Context, s *server, st screen.Status, delay time.Duration, what, abort string) (screen.Status, error) {
	if delay > 0 {
		unit := "seconds"
		if delay == time.Second {
			unit = "second"
		}
		s.printf("Server %q %s in %d %s (Ctrl+C aborts).", s.name, map[string]string{"shutdown": "shuts down", "restart": "restarts"}[what], int(delay/time.Second), unit)
		if err := m.cfg.Clock.Wait(ctx, delay); err != nil {
			sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), messageTimeout)
			defer cancel()
			if sendErr := s.sessions.Send(sendCtx, st, "say "+s.settings.Get(abort)); sendErr != nil {
				return st, fmt.Errorf("%w: server %q %s (the abort message could not be sent: %v)", ErrAborted, s.name, what, sendErr)
			}
			s.printf("Server %q %s was aborted.", s.name, what)
			return st, fmt.Errorf("%w: server %q %s", ErrAborted, s.name, what)
		}
	}
	// Look again: the server may have stopped during the countdown.
	st, err := s.state(ctx)
	if err == nil && !running(st) {
		s.printf("Server %q stopped during the countdown.", s.name)
	}
	return st, err
}

// halt saves and stops a running server and waits for its process to end.
func (m *Manager) halt(ctx context.Context, s *server, st screen.Status) error {
	if err := m.saveAll(ctx, s, st); err != nil {
		return err
	}
	if err := s.sessions.Send(ctx, st, "stop"); err != nil {
		return fmt.Errorf("stop server %q: %w", s.name, err)
	}
	s.printf("Stopping server %q...", s.name)
	timeout := m.stopTimeout()
	_, err := s.sessions.WaitStopped(ctx, s.screen, s.argv, timeout)
	switch {
	case err == nil:
		s.printf("Server %q stopped.", s.name)
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("%w while waiting for server %q to stop; the stop command was sent", ErrInterrupted, s.name)
	case errors.Is(err, screen.ErrStopTimeout):
		return fmt.Errorf("%w: server %q is still running %s after the stop command; nothing was killed", ErrStillRunning, s.name, timeout)
	default:
		return fmt.Errorf("stop server %q: %w", s.name, err)
	}
}

// saveAll sends save-all and waits briefly for the profile's confirmation,
// as server_save_all does. An unconfirmed save is reported, not fatal:
// Minecraft's stop command saves the worlds too.
func (m *Manager) saveAll(ctx context.Context, s *server, st screen.Status) error {
	cmd := s.profile.SaveAll
	confirm, err := s.profile.Match(cmd.Confirm)
	if err != nil {
		return err
	}
	log, err := watchLog(s.settings.Get("LOG_PATH"))
	if err != nil {
		return err
	}
	defer log.close()
	if err := s.sessions.Send(ctx, st, cmd.Line); err != nil {
		return fmt.Errorf("save server %q: %w", s.name, err)
	}
	deadline := m.cfg.Clock.Now().Add(cmd.Timeout)
	for {
		ok, err := log.poll(confirm)
		if err != nil {
			return fmt.Errorf("save server %q: %w", s.name, err)
		}
		if ok {
			s.printf("Server %q saved.", s.name)
			return nil
		}
		if !m.cfg.Clock.Now().Before(deadline) {
			s.printf("Server %q did not confirm save-all within %s; stopping anyway (stop saves the worlds too).", s.name, cmd.Timeout)
			return nil
		}
		if err := m.cfg.Clock.Wait(ctx, logPoll); err != nil {
			return fmt.Errorf("%w before server %q was stopped; it is still running", ErrInterrupted, s.name)
		}
	}
}
