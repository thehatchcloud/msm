// Package console sends commands to a running server's console and reads
// the server's answer from its log: the player and game commands
// (whitelist, ban, op, gamemode, kick, say, time, give, xp, save, ...),
// the raw cmd and cmdlog commands, the connected-player list and the
// interactive console.
//
// What each command types and what answers it come from the server's
// version profile (internal/profiles), as inert data. An answer is a log
// line written after the command was sent (internal/logwatch), so a stale
// line can never confirm a command (DEV-010). The legacy manager printed a
// success message whatever the server said; here a refusal in the log is
// an error, and a command whose answer never arrives is an error too. A
// command the profile has no answer for is reported as sent, never as done.
//
// A server's lock is held while commands are sent and answered, so two
// managers never interleave commands or race a stop or start. It is not
// held while following a log or attached to the console.
package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/logwatch"
	"github.com/thehatchcloud/msm/internal/process"
	"github.com/thehatchcloud/msm/internal/profiles"
	"github.com/thehatchcloud/msm/internal/screen"
	"github.com/thehatchcloud/msm/internal/serverprops"
	"github.com/thehatchcloud/msm/internal/servers"
)

var (
	ErrNoScreen    = errors.New("console: GNU screen is not installed")
	ErrNotRunning  = errors.New("console: server is not running")
	ErrNotProven   = errors.New("console: server state cannot be proven")
	ErrRefused     = errors.New("console: the server refused the command")
	ErrUnconfirmed = errors.New("console: the server did not answer the command")
	ErrAmbiguous   = errors.New("console: this command needs one server")
	ErrBadList     = errors.New("console: cannot read the list")
)

const (
	// DefaultAnswerTimeout is how long to wait for the log line that
	// confirms a command, unless the profile or Config.Timeout says
	// otherwise. The shipped profiles' one second is too short for a
	// loaded server.
	DefaultAnswerTimeout = 5 * time.Second

	logPoll = 100 * time.Millisecond
	// tailLines is how much recent log cmdlog shows before following.
	tailLines = 5
	// maxList bounds a whitelist, ban or ops file.
	maxList = 8 << 20
)

// Sessions is the part of the screen backend the console uses.
type Sessions interface {
	Status(ctx context.Context, name string, want []string) (screen.Status, error)
	Send(ctx context.Context, st screen.Status, line string) error
	Attach(ctx context.Context, st screen.Status, stdio process.Stdio, term string) error
}

var _ Sessions = (*screen.Backend)(nil)

// Config connects the console to the host.
type Config struct {
	Servers *servers.Manager
	// Sessions returns the screen backend for a server owner, or
	// ErrNoScreen when screen is not installed.
	Sessions func(owner identity.Identity) (Sessions, error)
	Clock    clock.Clock
	// Timeout, when set, replaces how long each command waits for its
	// answer.
	Timeout time.Duration
}

// Output is where the console reports: results on Out, notes on Err.
type Output struct {
	Out, Err io.Writer
}

// Manager runs console commands.
type Manager struct{ cfg Config }

func New(cfg Config) (*Manager, error) {
	if cfg.Servers == nil || cfg.Sessions == nil || cfg.Clock == nil {
		return nil, errors.New("console: servers, sessions and clock are required")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("console: timeout must not be negative")
	}
	return &Manager{cfg: cfg}, nil
}

// server is one server and what is needed to talk to it.
type server struct {
	name     string
	dir      string
	settings *legacyconf.ServerSettings
	profile  profiles.Profile
	argv     []string
	argvErr  error
	sessions Sessions // nil when screen is not installed
	screen   string
	out      Output
}

func (s *server) printf(format string, args ...any) {
	fmt.Fprintf(s.out.Out, format+"\n", args...)
}

func (m *Manager) prepare(set *legacyconf.ServerSettings, out Output) (*server, error) {
	owner, err := m.cfg.Servers.Owner(set)
	if err != nil {
		return nil, err
	}
	s := &server{name: set.Name, dir: set.Dir, settings: set, screen: set.Get("SCREEN_NAME"), out: out}
	var note string
	s.profile, note = profiles.Select(set.Get("VERSION"))
	// An unset VERSION is the normal case and the fallback is the legacy
	// default; only a version that names no profile is worth a warning.
	if v := set.Get("VERSION"); note != "" && v != "" && v != "unknown" {
		fmt.Fprintf(out.Err, "msm: warning: server %q: %s\n", s.name, note)
	}
	s.argv, s.argvErr = set.Invocation()
	s.sessions, err = m.cfg.Sessions(owner)
	if err != nil && !errors.Is(err, ErrNoScreen) {
		return nil, fmt.Errorf("server %q: %w", set.Name, err)
	}
	return s, nil
}

// open locks a server and resolves what a command needs. The caller
// releases the lock.
func (m *Manager) open(name string, out Output) (*filelock.Lock, *server, error) {
	lock, set, err := m.cfg.Servers.LockServer(name)
	if err != nil {
		return nil, nil, err
	}
	s, err := m.prepare(set, out)
	if err != nil {
		lock.Release()
		return nil, nil, err
	}
	return lock, s, nil
}

// observe returns the server's session, which is nil if it is not running.
func (s *server) observe(ctx context.Context) (screen.Status, error) {
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

// running is observe for a command that needs a running server.
func (s *server) running(ctx context.Context) (screen.Status, error) {
	if s.sessions == nil {
		return screen.Status{}, fmt.Errorf("%w: server %q cannot be inspected", ErrNoScreen, s.name)
	}
	st, err := s.observe(ctx)
	if err != nil {
		return st, err
	}
	if st.Session == nil || st.Liveness != screen.Running {
		return st, fmt.Errorf("%w: server %q is not running", ErrNotRunning, s.name)
	}
	return st, nil
}

// Step is one console command to send and what to say about it.
type Step struct {
	// Command is a profile command name (profiles.WhitelistAdd, ...).
	Command string
	Args    map[string]string
	// Doing, when set, is printed before the command is sent; Done once
	// the server confirmed it.
	Doing, Done string
	// Sync flushes the file system after the command is confirmed.
	Sync bool
}

// Do sends steps to a running server, one after another under its lock,
// and reports each. Every step is prepared before the first is sent, so a
// command this server version cannot do sends nothing. A refused step does
// not stop the later ones; an unanswered one does, since a late answer
// could be taken for the next step's.
func (m *Manager) Do(ctx context.Context, name string, steps []Step, out Output) error {
	lock, s, err := m.open(name, out)
	if err != nil {
		return err
	}
	defer lock.Release()
	return m.do(ctx, s, steps)
}

func (m *Manager) do(ctx context.Context, s *server, steps []Step) error {
	prepared := make([]profiles.Prepared, len(steps))
	for i, step := range steps {
		p, err := s.profile.Prepare(step.Command, step.Args)
		if err != nil {
			return fmt.Errorf("server %q (%s): %w", s.name, s.profile.Name, err)
		}
		if err := screen.ValidateInput(p.Line); err != nil {
			return fmt.Errorf("server %q: %q: %w", s.name, step.Command, err)
		}
		prepared[i] = p
	}
	st, err := s.running(ctx)
	if err != nil {
		return err
	}
	body, err := s.profile.Body()
	if err != nil {
		return err
	}
	log, err := logwatch.Watch(s.settings.Get("LOG_PATH"))
	if err != nil {
		return fmt.Errorf("server %q: %w", s.name, err)
	}
	defer log.Close()
	var errs []error
	for i, step := range steps {
		err := m.send(ctx, s, st, log, body, prepared[i], step)
		if err == nil {
			continue
		}
		errs = append(errs, err)
		if errors.Is(err, ErrUnconfirmed) || ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

// answerTimeout is how long to wait for a command's answer. A command that
// expects a confirmation waits at least DefaultAnswerTimeout; one for which
// only a refusal is known waits no longer than its profile says.
func (m *Manager) answerTimeout(profile time.Duration, confirmed bool) time.Duration {
	switch {
	case m.cfg.Timeout > 0:
		return m.cfg.Timeout
	case !confirmed:
		return profile
	default:
		return max(profile, DefaultAnswerTimeout)
	}
}

func (m *Manager) send(ctx context.Context, s *server, st screen.Status, log *logwatch.Watcher, body *regexp.Regexp, p profiles.Prepared, step Step) error {
	if step.Doing != "" {
		fmt.Fprint(s.out.Out, step.Doing+" ")
	}
	if err := s.sessions.Send(ctx, st, p.Line); err != nil {
		if step.Doing != "" {
			fmt.Fprintln(s.out.Out)
		}
		return fmt.Errorf("server %q: send %q: %w", s.name, p.Line, err)
	}
	if p.Success == nil && p.Failure == nil {
		if step.Doing != "" {
			fmt.Fprintln(s.out.Out)
		}
		s.printf("Sent %q to server %q; %s does not log an answer to it.", p.Line, s.name, s.profile.Name)
		return nil
	}
	timeout := m.answerTimeout(p.Timeout, p.Success != nil)
	deadline := m.cfg.Clock.Now().Add(timeout)
	for {
		lines, err := log.Lines()
		if err != nil {
			return fmt.Errorf("server %q: %w", s.name, err)
		}
		for _, l := range lines {
			switch {
			case p.Failure != nil && p.Failure.MatchString(l):
				if step.Doing != "" {
					fmt.Fprintln(s.out.Out)
				}
				return fmt.Errorf("%w: server %q: %q: %s", ErrRefused, s.name, p.Line, message(body, l))
			case p.Success != nil && p.Success.MatchString(l):
				m.done(s, step, "")
				return nil
			}
		}
		if !m.cfg.Clock.Now().Before(deadline) {
			break
		}
		if err := m.cfg.Clock.Wait(ctx, logPoll); err != nil {
			return fmt.Errorf("%w: server %q: %q was sent; its answer was not awaited", context.Cause(ctx), s.name, p.Line)
		}
	}
	if p.Success == nil {
		// Only refusals are known for this version, and none came.
		m.done(s, step, " (not confirmed: this server version does not log an answer to it, and no refusal was logged)")
		return nil
	}
	if step.Doing != "" {
		fmt.Fprintln(s.out.Out)
	}
	return fmt.Errorf("%w: server %q: no answer to %q in %s; it may still have taken effect (see %s)",
		ErrUnconfirmed, s.name, p.Line, timeout, s.settings.Get("LOG_PATH"))
}

func (m *Manager) done(s *server, step Step, note string) {
	if step.Sync {
		syscall.Sync()
	}
	if step.Doing != "" {
		fmt.Fprintln(s.out.Out, step.Done+note)
		return
	}
	s.printf("%s%s", step.Done, note)
}

// message is a log line without its time and level, with control
// characters removed so a hostile line cannot drive the terminal.
func message(body *regexp.Regexp, line string) string {
	if m := body.FindStringSubmatch(line); m != nil {
		line = m[len(m)-1]
	}
	return clean(line)
}

// clean drops control characters other than tab.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' {
			return -1
		}
		return r
	}, s)
}

// Cmd sends a raw console command (msm <server> cmd <command>). The words
// are joined with spaces; the server's answer is not awaited.
func (m *Manager) Cmd(ctx context.Context, name string, words []string, out Output) error {
	lock, s, err := m.open(name, out)
	if err != nil {
		return err
	}
	defer lock.Release()
	st, err := s.running(ctx)
	if err != nil {
		return err
	}
	if err := s.sessions.Send(ctx, st, strings.Join(words, " ")); err != nil {
		return fmt.Errorf("server %q: %w", s.name, err)
	}
	s.printf("Command sent.")
	return nil
}

// Cmdlog is Cmd, then follows the server's log until ctx is canceled
// (Ctrl+C), which ends the follower and leaves Minecraft running.
func (m *Manager) Cmdlog(ctx context.Context, name string, words []string, out Output) error {
	lock, s, err := m.open(name, out)
	if err != nil {
		return err
	}
	released := false
	release := func() {
		if !released {
			released = true
			lock.Release()
		}
	}
	defer release()
	st, err := s.running(ctx)
	if err != nil {
		return err
	}
	path := s.settings.Get("LOG_PATH")
	recent, err := tail(path, tailLines)
	if err != nil {
		return fmt.Errorf("server %q: %w", s.name, err)
	}
	log, err := logwatch.Watch(path)
	if err != nil {
		return fmt.Errorf("server %q: %w", s.name, err)
	}
	defer log.Close()
	s.printf("Now watching logs (press Ctrl+C to exit; the server keeps running):")
	s.printf("...")
	if err := s.sessions.Send(ctx, st, strings.Join(words, " ")); err != nil {
		return fmt.Errorf("server %q: %w", s.name, err)
	}
	release() // other commands may run while this one watches
	for _, l := range recent {
		s.printf("%s", clean(l))
	}
	for {
		lines, err := log.Lines()
		if err != nil {
			return fmt.Errorf("server %q: %w", s.name, err)
		}
		for _, l := range lines {
			s.printf("%s", clean(l))
		}
		if err := m.cfg.Clock.Wait(ctx, logPoll); err != nil {
			return nil
		}
	}
}

// tail returns up to n of the last complete lines of the log, or none if
// there is no log.
func tail(path string, n int) ([]string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("log %s is not a regular file", path)
	}
	const window = 64 << 10
	size := min(info.Size(), window)
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, info.Size()-size); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read log %s: %w", path, err)
	}
	text := string(buf)
	if info.Size() > window {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	lines := strings.Split(strings.TrimRight(text, "\r\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	return lines, nil
}

// Console attaches the caller's terminal to a running server's console
// (msm <server> console). The server's lock is not held while attached.
func (m *Manager) Console(ctx context.Context, name string, stdio process.Stdio, term string, out Output) error {
	set, err := m.cfg.Servers.Settings(name)
	if err != nil {
		return err
	}
	s, err := m.prepare(set, out)
	if err != nil {
		return err
	}
	st, err := s.running(ctx)
	if err != nil {
		return err
	}
	s.printf("Attaching to the console of server %q.", s.name)
	s.printf("Press Ctrl+A then D to detach and leave the server running. Ctrl+C is typed into the")
	s.printf("server's console: it can stop the server, so do not use it to leave.")
	return s.sessions.Attach(ctx, st, stdio, term)
}

// SetWhitelist turns the whitelist on or off (msm <server> whitelist on|off).
// A running server is told with its console command. A stopped server's
// white-list property is changed instead, and takes effect when it starts.
func (m *Manager) SetWhitelist(ctx context.Context, name string, on bool, out Output) error {
	lock, s, err := m.open(name, out)
	if err != nil {
		return err
	}
	defer lock.Release()
	command, done, value := profiles.WhitelistOff, "Whitelist disabled.", "false"
	if on {
		command, done, value = profiles.WhitelistOn, "Whitelist enabled.", "true"
	}
	if _, err := s.running(ctx); err == nil {
		return m.do(ctx, s, []Step{{Command: command, Done: done}})
	} else if !errors.Is(err, ErrNotRunning) {
		return err
	}
	path := filepath.Join(s.dir, m.cfg.Servers.PropertiesFile())
	doc, err := serverprops.Load(path)
	if err != nil {
		return fmt.Errorf("server %q: %w", s.name, err)
	}
	if err := doc.Set("white-list", value); err != nil {
		return fmt.Errorf("server %q: %w", s.name, err)
	}
	if err := doc.Save(path, 0o644); err != nil {
		return fmt.Errorf("server %q: %w", s.name, err)
	}
	s.printf("%s Server %q is stopped, so white-list=%s was written to %s and applies when it starts.",
		strings.TrimSuffix(done, "."), s.name, value, filepath.Base(path))
	return nil
}

// ForAll runs fn for every server in name order and returns every failure
// together. A server that is not running is skipped with a note: "all"
// means the servers that can be reached, not an error for each stopped one.
func (m *Manager) ForAll(ctx context.Context, out Output, fn func(name string) error) error {
	entries, warnings, err := m.cfg.Servers.List(ctx)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(out.Err, "msm: warning: %s\n", w)
	}
	var errs []error
	for _, e := range entries {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		switch err := fn(e.Name); {
		case err == nil:
		case errors.Is(err, ErrNotRunning):
			fmt.Fprintf(out.Out, "Server %q is not running; skipped.\n", e.Name)
		default:
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
