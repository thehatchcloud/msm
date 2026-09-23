// Package screen is the narrow GNU screen adapter that owns a server's
// console session: discovering it, launching a process detached inside it,
// typing a console line into it, and attaching a terminal to it.
//
// Every screen invocation is an explicit argument vector with an explicit
// working directory, environment and owner; nothing goes through a shell.
// The package distinguishes three facts the legacy manager conflated:
// whether a screen session exists, whether the process inside it is the
// expected server invocation (liveness), and whether that server is ready,
// which only a caller-supplied probe can decide. It never kills, wipes or
// adopts a process it cannot prove is the one it launched.
package screen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/process"
)

var (
	ErrInvalidName    = errors.New("screen: invalid session name")
	ErrInaccessible   = errors.New("screen: sessions are inaccessible")
	ErrDuplicate      = errors.New("screen: more than one live session has this name")
	ErrAlreadyRunning = errors.New("screen: a live session already has this name")
	ErrNotRunning     = errors.New("screen: server is not running")
	ErrStartTimeout   = errors.New("screen: server did not start before the deadline")
	ErrStopTimeout    = errors.New("screen: server did not stop before the deadline")
	ErrReadyTimeout   = errors.New("screen: server was not ready before the deadline")
	ErrExited         = errors.New("screen: server exited")
	ErrNotTerminal    = errors.New("screen: attach requires an interactive terminal")
	ErrAttachOwner    = errors.New("screen: attach must run as the session owner")
)

// Liveness is what the process table says about a named session.
type Liveness int

const (
	// Stopped: no live session has the name. Dead sockets may remain.
	Stopped Liveness = iota
	// Running: exactly one live session, owned by the configured user,
	// whose window process is the expected invocation.
	Running
	// Starting: the session exists but its window process has not been
	// observed yet, or has just exited and screen has not closed.
	Starting
	// Foreign: a live session with this name belongs to another user or
	// runs something else. It is reported, never adopted or terminated.
	Foreign
)

func (l Liveness) String() string {
	return [...]string{"stopped", "running", "starting", "foreign"}[l]
}

// Status is one observation of a named session.
type Status struct {
	Name     string
	Liveness Liveness
	// Session is the single live session with the name, if there is one.
	Session *Session
	// Process is the session's window process: the server itself when
	// Running, or the unexpected process when Foreign.
	Process *ProcInfo
	// Stale lists dead or unreachable sockets with the name. They are
	// ignored for liveness and left in place for an administrator to
	// inspect; screen -wipe removes them.
	Stale []Session
	// Detail explains a Foreign or Starting observation.
	Detail string
}

// Config describes how to run screen for one owner. Every field except
// PollInterval is required.
type Config struct {
	// Screen is the absolute path of the screen executable.
	Screen string
	// Owner is the OS user every session runs as. A root caller runs
	// screen with Owner's credentials; any other caller must be Owner.
	Owner identity.Identity
	// Env is the complete environment for every screen invocation,
	// including SCREENDIR when sessions live somewhere non-default. It is
	// not merged with the caller's environment.
	Env       []string
	Runner    process.Runner
	Terminal  process.Terminal
	Processes ProcessTable
	Clock     clock.Clock
	// PollInterval spaces status checks while waiting; default 100ms.
	PollInterval time.Duration
}

// Backend is safe for concurrent use, but it does not serialize
// operations on one server: callers hold that server's filelock around
// any launch or stop sequence.
type Backend struct {
	cfg        Config
	credential *process.Credential
}

func New(cfg Config) (*Backend, error) {
	switch {
	case !filepath.IsAbs(cfg.Screen):
		return nil, fmt.Errorf("screen: executable path %q must be absolute", cfg.Screen)
	case cfg.Env == nil:
		return nil, errors.New("screen: an explicit environment is required")
	case cfg.Runner == nil || cfg.Terminal == nil || cfg.Processes == nil || cfg.Clock == nil:
		return nil, errors.New("screen: runner, terminal, process table and clock are required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	b := &Backend{cfg: cfg}
	switch euid := os.Geteuid(); {
	case euid == cfg.Owner.UID:
	case euid == 0:
		b.credential = &process.Credential{UID: uint32(cfg.Owner.UID), GID: uint32(cfg.Owner.GID)}
	default:
		return nil, fmt.Errorf("%w: sessions belong to %s (uid %d), running as uid %d",
			identity.ErrPrivilegeRequired, cfg.Owner.Username, cfg.Owner.UID, euid)
	}
	return b, nil
}

// ValidateName accepts the session names this port manages: 1–64 ASCII
// letters, digits, '_' and '-'. The default "msm-{SERVER_NAME}" always
// qualifies. '.' is excluded because screen joins "<pid>.<name>", and
// shorter names keep socket paths inside the platform sun_path limit.
func ValidateName(name string) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("%w %q: must be 1-64 characters", ErrInvalidName, name)
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return fmt.Errorf("%w %q: only letters, digits, '_' and '-' are allowed", ErrInvalidName, name)
		}
	}
	return nil
}

func (b *Backend) command(dir string, env []string, args ...string) process.Command {
	return process.Command{
		Path: b.cfg.Screen, Args: args, Dir: dir,
		Env:        append(append([]string{}, b.cfg.Env...), env...),
		Credential: b.credential,
	}
}

// Sessions lists every socket screen reports for the owner.
func (b *Backend) Sessions(ctx context.Context) ([]Session, error) {
	result, err := b.cfg.Runner.Run(ctx, b.command("/", nil, "-ls"))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil && result.Stdout == "" && result.Stderr == "" {
		return nil, fmt.Errorf("screen: run %s -ls: %w", b.cfg.Screen, err)
	}
	sessions := parseList(result.Stdout)
	if sessions == nil && !strings.Contains(result.Stdout, "No Sockets found") {
		// Screen explains an unusable socket directory (wrong mode or
		// owner, missing /run/screen) on stdout and exits nonzero.
		detail := strings.TrimSpace(result.Stdout + "\n" + result.Stderr)
		return nil, fmt.Errorf("%w: %s", ErrInaccessible, detail)
	}
	return sessions, nil
}

// Status observes the named session and classifies its window process
// against want, the expected invocation argv.
func (b *Backend) Status(ctx context.Context, name string, want []string) (Status, error) {
	if err := ValidateName(name); err != nil {
		return Status{}, err
	}
	sessions, err := b.Sessions(ctx)
	if err != nil {
		return Status{}, err
	}
	st := Status{Name: name}
	var live []Session
	for _, s := range sessions {
		switch {
		case s.Name != name:
		case s.Live():
			live = append(live, s)
		default:
			st.Stale = append(st.Stale, s)
		}
	}
	switch len(live) {
	case 0:
		return st, nil
	case 1:
		st.Session = &live[0]
	default:
		ids := make([]string, len(live))
		for i, s := range live {
			ids[i] = s.ID
		}
		return st, fmt.Errorf("%w %q: %s; stop the extra sessions by hand", ErrDuplicate, name, strings.Join(ids, ", "))
	}
	return st, b.classify(&st, want)
}

func (b *Backend) classify(st *Status, want []string) error {
	server, err := b.cfg.Processes.Get(st.Session.PID)
	if errors.Is(err, ErrNoProcess) {
		// Screen answered on the socket but its process is already gone:
		// it is exiting. Report nothing live rather than guess.
		st.Stale, st.Session = append(st.Stale, *st.Session), nil
		return nil
	}
	if err != nil {
		return err
	}
	if server.UID != b.cfg.Owner.UID {
		st.Liveness, st.Detail = Foreign, fmt.Sprintf("screen process %d belongs to uid %d", server.PID, server.UID)
		return nil
	}
	children, err := b.cfg.Processes.Children(server.PID)
	if err != nil {
		return err
	}
	var matches []ProcInfo
	for _, c := range children {
		if c.UID == b.cfg.Owner.UID && sameInvocation(c.Argv, want) {
			matches = append(matches, c)
		}
	}
	switch {
	case len(matches) == 1:
		// An administrator may have opened more windows from the console;
		// the server is still the one window running the invocation.
		st.Liveness, st.Process = Running, &matches[0]
		if extra := len(children) - 1; extra > 0 {
			st.Detail = fmt.Sprintf("session also has %d other window(s)", extra)
		}
	case len(matches) > 1:
		st.Liveness, st.Detail = Foreign, fmt.Sprintf("%d windows run the invocation", len(matches))
	case len(children) == 0:
		st.Liveness, st.Detail = Starting, "session has no window process"
	default:
		st.Liveness, st.Process = Foreign, &children[0]
		st.Detail = fmt.Sprintf("window process %d (uid %d) runs %q", children[0].PID, children[0].UID, children[0].Argv)
	}
	return nil
}

// LaunchSpec is a server invocation to start in a detached session.
type LaunchSpec struct {
	Name string
	// Dir is the absolute working directory of the server.
	Dir string
	// Argv is the complete invocation; Argv[0] is resolved by screen
	// using the PATH in Config.Env when it is not absolute.
	Argv []string
	// Env adds to Config.Env for this invocation only.
	Env []string
	// Timeout bounds how long the window process may take to appear.
	Timeout time.Duration
}

// Launch starts spec in a new detached session and waits until its window
// process is observed running the invocation. It refuses when any live session already has the
// name, whatever it runs. The server may not be ready yet; see WaitReady.
func (b *Backend) Launch(ctx context.Context, spec LaunchSpec) (Status, error) {
	if err := ValidateName(spec.Name); err != nil {
		return Status{}, err
	}
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return Status{}, errors.New("screen: launch requires an invocation")
	}
	if !filepath.IsAbs(spec.Dir) {
		return Status{}, fmt.Errorf("screen: working directory %q must be absolute", spec.Dir)
	}
	if spec.Timeout <= 0 {
		return Status{}, errors.New("screen: launch requires a timeout")
	}
	st, err := b.Status(ctx, spec.Name, spec.Argv)
	if err != nil {
		return st, err
	}
	if st.Session != nil {
		return st, fmt.Errorf("%w: %s (%s)", ErrAlreadyRunning, st.Session.ID, st.Liveness)
	}
	args := append([]string{"-dmS", spec.Name}, spec.Argv...)
	if result, err := b.cfg.Runner.Run(ctx, b.command(spec.Dir, spec.Env, args...)); err != nil {
		return st, fmt.Errorf("screen: launch %q: %w: %s", spec.Name, err, strings.TrimSpace(result.Stdout+result.Stderr))
	}
	seen := false
	return b.poll(ctx, spec.Timeout, func() (Status, bool, error) {
		st, err := b.Status(ctx, spec.Name, spec.Argv)
		// A Foreign observation is not fatal here: until screen's forked
		// window process execs the invocation, it still looks like screen.
		// If it never becomes the invocation, the deadline reports why.
		switch {
		case err != nil:
		case st.Session != nil:
			seen = true
		case seen:
			err = fmt.Errorf("%w: session %q ended during startup; check that %q runs in %s",
				ErrExited, spec.Name, spec.Argv, spec.Dir)
		}
		return st, st.Liveness == Running, err
	}, func(st Status) error {
		if st.Session == nil {
			return fmt.Errorf("%w: session %q is gone; the invocation %q probably failed to run in %s",
				ErrStartTimeout, spec.Name, spec.Argv, spec.Dir)
		}
		return fmt.Errorf("%w: %s: %s", ErrStartTimeout, st.Session.ID, st.Detail)
	})
}

// Send types one console line into a running session and presses Enter.
// st must be a Running observation from Status or Launch; the line is
// addressed to that exact "<pid>.<name>" session, never a name prefix. A
// long line takes several screen calls, so concurrent senders to one
// server must be serialized by the caller's server lock.
func (b *Backend) Send(ctx context.Context, st Status, line string) error {
	if st.Liveness != Running || st.Session == nil {
		return fmt.Errorf("%w: %q is %s", ErrNotRunning, st.Name, st.Liveness)
	}
	if err := ValidateInput(line); err != nil {
		return err
	}
	for _, chunk := range stuffChunks(line) {
		result, err := b.cfg.Runner.Run(ctx, b.command("/", nil,
			"-S", st.Session.ID, "-p", "0", "-X", "stuff", chunk))
		if err != nil {
			out := strings.TrimSpace(result.Stdout + result.Stderr)
			if strings.Contains(out, "No screen session found") {
				return fmt.Errorf("%w: session %s ended", ErrNotRunning, st.Session.ID)
			}
			return fmt.Errorf("screen: send to %s: %w: %s", st.Session.ID, err, out)
		}
	}
	return nil
}

// Attach connects the caller's terminal to a running session until the
// user detaches (Ctrl-A d) or the session ends. Detaching leaves the
// server running. Only the owner can attach: screen refuses a terminal
// that belongs to another user, so a root caller must switch user first.
func (b *Backend) Attach(ctx context.Context, st Status, stdio process.Stdio, term string) error {
	if st.Liveness != Running || st.Session == nil {
		return fmt.Errorf("%w: %q is %s", ErrNotRunning, st.Name, st.Liveness)
	}
	if b.credential != nil {
		return fmt.Errorf("%w: run the console as %s", ErrAttachOwner, b.cfg.Owner.Username)
	}
	for _, f := range []*os.File{stdio.In, stdio.Out} {
		if f == nil || !isTerminal(f) {
			return ErrNotTerminal
		}
	}
	if term == "" {
		term = "vt100"
	}
	cmd := b.command("/", []string{"TERM=" + term}, "-r", st.Session.ID)
	if err := b.cfg.Terminal.RunAttached(ctx, cmd, stdio); err != nil {
		return fmt.Errorf("screen: attach to %s: %w", st.Session.ID, err)
	}
	return nil
}

func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

// WaitStopped waits until no live session has the name, after the caller
// has asked the server to stop. It never signals or kills anything.
func (b *Backend) WaitStopped(ctx context.Context, name string, want []string, timeout time.Duration) (Status, error) {
	return b.poll(ctx, timeout, func() (Status, bool, error) {
		st, err := b.Status(ctx, name, want)
		return st, err == nil && st.Session == nil, err
	}, func(st Status) error {
		return fmt.Errorf("%w: %s is still %s", ErrStopTimeout, st.Session.ID, st.Liveness)
	})
}

// Probe reports whether a running server is ready, for example from a log
// event correlated after launch. It is only called while the server's
// process is Running.
type Probe func(context.Context) (bool, error)

// WaitReady waits for probe to succeed, failing early with ErrExited if
// the expected process stops running first, so a dead server is never
// reported as merely slow.
func (b *Backend) WaitReady(ctx context.Context, name string, want []string, timeout time.Duration, probe Probe) (Status, error) {
	return b.poll(ctx, timeout, func() (Status, bool, error) {
		st, err := b.Status(ctx, name, want)
		if err != nil {
			return st, false, err
		}
		if st.Liveness != Running {
			return st, false, fmt.Errorf("%w before it was ready: %s %s", ErrExited, st.Liveness, st.Detail)
		}
		ready, err := probe(ctx)
		return st, ready, err
	}, func(Status) error {
		return fmt.Errorf("%w: %q is running but its readiness probe never succeeded", ErrReadyTimeout, name)
	})
}

// poll calls check until it reports done or fails, or the deadline passes.
func (b *Backend) poll(ctx context.Context, timeout time.Duration, check func() (Status, bool, error), expired func(Status) error) (Status, error) {
	deadline := b.cfg.Clock.Now().Add(timeout)
	for {
		st, done, err := check()
		if err != nil || done {
			return st, err
		}
		if !b.cfg.Clock.Now().Before(deadline) {
			return st, expired(st)
		}
		if err := b.cfg.Clock.Wait(ctx, b.cfg.PollInterval); err != nil {
			return st, err
		}
	}
}
