package screen

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/process"
)

const ls409 = "There are screens on:\n" +
	"\t694.msm-a\t(09/23/26 01:33:18)\t(Detached)\n" +
	"\t697.msm-b\t(09/23/26 01:33:18)\t(Attached)\n" +
	"\t691.msm-a\t(09/23/26 01:33:18)\t(Dead ???)\n" +
	"\t700.msm-c\t(Remote or dead)\n" +
	"\t701.msm-d\t(Multi, detached)\n" +
	"\tgarbage line\n" +
	"Remove dead screens with 'screen -wipe'.\n5 Sockets in /tmp/sd.\n"

func TestParseList(t *testing.T) {
	got := parseList(ls409)
	want := []Session{
		{"694.msm-a", 694, "msm-a", Detached},
		{"697.msm-b", 697, "msm-b", Attached},
		{"691.msm-a", 691, "msm-a", Dead},
		{"700.msm-c", 700, "msm-c", Unreachable},
		{"701.msm-d", 701, "msm-d", Detached},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
	// Screen 5 omits the start time.
	if got := parseList("There is a screen on:\n\t12.x\t(Detached)\n1 Socket in /run/screen/S-u.\n"); len(got) != 1 || got[0].ID != "12.x" {
		t.Fatalf("got %+v", got)
	}
	if got := parseList("No Sockets found in /tmp/sd.\n\n"); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestValidateInputAndEscape(t *testing.T) {
	for _, bad := range []string{"", "a\rb", "a\nb", "\x1b[2J", "tab\there", "\x7f", "c1\u0085", string([]byte{0xff}), strings.Repeat("x", MaxInputBytes+1)} {
		if err := ValidateInput(bad); !errors.Is(err, ErrUnsafeInput) {
			t.Errorf("ValidateInput(%q) = %v", bad, err)
		}
	}
	good := `say "hi" 'there' $HOME ${X} ^C \015 é漢`
	if err := ValidateInput(good); err != nil {
		t.Fatal(err)
	}
	if got, want := stuffChunks(good), []string{`say "hi" 'there' \$HOME \${X} \^C \\015 é漢` + "\r"}; !slices.Equal(got, want) {
		t.Fatalf("chunks = %q, want %q", got, want)
	}

	// A maximum-length line of escapable and multibyte characters splits
	// into bounded chunks without breaking an escape or a UTF-8 sequence.
	long := strings.Repeat("$é", MaxInputBytes/3)
	chunks := stuffChunks(long)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	var joined strings.Builder
	for i, c := range chunks {
		if len(c) > stuffChunkBytes || !utf8.ValidString(c) || strings.HasSuffix(c, `\`) {
			t.Fatalf("chunk %d = %q", i, c)
		}
		joined.WriteString(c)
	}
	if want := strings.ReplaceAll(long, "$", `\$`) + "\r"; joined.String() != want {
		t.Fatal("chunks do not reassemble into the escaped line")
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"msm-survival", "A_1", strings.Repeat("a", 64)} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "has.dot", "sp ace", "a/b", "é", strings.Repeat("a", 65)} {
		if err := ValidateName(bad); !errors.Is(err, ErrInvalidName) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestParseVersion(t *testing.T) {
	for out, want := range map[string]Version{
		"Screen version 4.09.01 (GNU) 20-Aug-23\n":                 {4, 9, 1},
		"Screen version 4.00.03 (FAU) 23-Oct-06\n":                 {4, 0, 3},
		"Screen version 5.0.1 (build on 2025-05-12 10:00:00) \n":   {5, 0, 1},
		"Screen version 4.1 (unpatched)\n":                         {4, 1, 0},
		"garbage then Screen version 4.08.00 (GNU) 05-Feb-20 tail": {4, 8, 0},
	} {
		if got, err := parseVersion(out); err != nil || got != want {
			t.Errorf("%q: got %v, %v", out, got, err)
		}
	}
	if _, err := parseVersion("tmux 3.4"); err == nil {
		t.Fatal("expected error")
	}
}

func TestVersionRejectsBundledMacOSScreen(t *testing.T) {
	for out, supported := range map[string]bool{
		"Screen version 4.00.03 (FAU) 23-Oct-06\n":            false,
		"Screen version 4.09.01 (GNU) 20-Aug-23\n":            true,
		"Screen version 5.0.2 (build on 2026-07-11 12:23:56)": true,
	} {
		_, err := testBackend(t, lsOnly(out), fakeProcs{}).Version(context.Background())
		if supported != (err == nil) || (!supported && !errors.Is(err, ErrUnsupportedVersion)) {
			t.Errorf("%q: %v", out, err)
		}
	}
}

func TestParseProcArgs(t *testing.T) {
	raw := binary.NativeEndian.AppendUint32(nil, 3)
	raw = append(raw, "/usr/bin/java\x00\x00\x00\x00java\x00-jar\x00a b.jar\x00PATH=/bin\x00"...)
	if got, err := parseProcArgs(raw); err != nil || !slices.Equal(got, []string{"java", "-jar", "a b.jar"}) {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := parseProcArgs(raw[:20]); err == nil {
		t.Fatal("expected truncation error")
	}
}

// fakeRunner answers each screen invocation from a function of its args.
type fakeRunner struct {
	mu     sync.Mutex
	calls  []process.Command
	answer func(args []string) (process.Result, error)
}

func (f *fakeRunner) Run(ctx context.Context, cmd process.Command) (process.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()
	return f.answer(cmd.Args)
}

func (f *fakeRunner) RunAttached(ctx context.Context, cmd process.Command, _ process.Stdio) error {
	_, err := f.Run(ctx, cmd)
	return err
}

func (f *fakeRunner) argsOf(verb string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if slices.Contains(c.Args, verb) {
			out = append(out, c.Args)
		}
	}
	return out
}

type fakeProcs map[int]ProcInfo

func (p fakeProcs) Get(pid int) (ProcInfo, error) {
	if info, ok := p[pid]; ok {
		return info, nil
	}
	return ProcInfo{}, fmt.Errorf("%w: %d", ErrNoProcess, pid)
}

func (p fakeProcs) Children(ppid int) ([]ProcInfo, error) {
	var out []ProcInfo
	for _, info := range p {
		if info.PPID == ppid {
			out = append(out, info)
		}
	}
	slices.SortFunc(out, func(a, b ProcInfo) int { return a.PID - b.PID })
	return out, nil
}

// stepClock advances by each requested wait immediately.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *stepClock) Wait(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

var invocation = []string{"java", "-Xmx1024M", "-jar", "/srv/a/server.jar", "nogui"}

func testBackend(t *testing.T, runner *fakeRunner, procs ProcessTable) *Backend {
	t.Helper()
	b, err := New(Config{
		Screen: "/usr/bin/screen", Owner: identity.Identity{Username: "me", UID: os.Geteuid(), GID: os.Getegid()},
		Env: []string{"PATH=/usr/bin:/bin", "SCREENDIR=/tmp/sd"}, Runner: runner, Terminal: runner,
		Processes: procs, Clock: &stepClock{now: time.Unix(0, 0)}, PollInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func lsOnly(out string) *fakeRunner {
	return &fakeRunner{answer: func([]string) (process.Result, error) {
		return process.Result{Stdout: out}, nil
	}}
}

func TestNewValidatesConfig(t *testing.T) {
	r := lsOnly("")
	base := Config{Screen: "/usr/bin/screen", Env: []string{}, Runner: r, Terminal: r, Processes: fakeProcs{}, Clock: &stepClock{},
		Owner: identity.Identity{UID: os.Geteuid()}}
	for name, mutate := range map[string]func(*Config){
		"relative screen": func(c *Config) { c.Screen = "screen" },
		"nil env":         func(c *Config) { c.Env = nil },
		"no runner":       func(c *Config) { c.Runner = nil },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	other := base
	other.Owner = identity.Identity{Username: "other", UID: os.Geteuid() + 1, GID: 4242}
	b, err := New(other)
	if os.Geteuid() == 0 {
		if err != nil || b.credential == nil || b.credential.UID != uint32(other.Owner.UID) || b.credential.GID != 4242 {
			t.Fatalf("root must run screen as the owner: %+v, %v", b, err)
		}
	} else if !errors.Is(err, identity.ErrPrivilegeRequired) {
		t.Fatalf("unprivileged caller for another owner: %v", err)
	}
}

func TestStatus(t *testing.T) {
	me := os.Geteuid()
	running := fakeProcs{694: {PID: 694, PPID: 1, UID: me, Argv: []string{"SCREEN"}},
		695: {PID: 695, PPID: 694, UID: me, Argv: append([]string{"/opt/jdk/bin/java"}, invocation[1:]...)}}
	cases := []struct {
		name     string
		ls       string
		procs    fakeProcs
		want     Liveness
		stale    int
		errIs    error
		detailed bool
	}{
		{"running with stale socket", ls409, running, Running, 1, nil, false},
		{"absent", "No Sockets found in /tmp/sd.\n", running, Stopped, 0, nil, false},
		{"no window yet", ls409, fakeProcs{694: running[694]}, Starting, 1, nil, true},
		{"screen process vanished", ls409, fakeProcs{}, Stopped, 2, nil, false},
		{"foreign argv", ls409, fakeProcs{694: running[694], 695: {PID: 695, PPID: 694, UID: me, Argv: []string{"java", "-jar", "other.jar"}}}, Foreign, 1, nil, true},
		{"foreign owner", ls409, fakeProcs{694: {PID: 694, UID: me + 1}}, Foreign, 1, nil, true},
		{"extra console window", ls409, fakeProcs{694: running[694], 695: running[695], 696: {PID: 696, PPID: 694, UID: me, Argv: []string{"bash"}}}, Running, 1, nil, true},
		{"invocation twice", ls409, fakeProcs{694: running[694], 695: running[695], 696: {PID: 696, PPID: 694, UID: me, Argv: invocation}}, Foreign, 1, nil, true},
		{"duplicate", ls409 + "\t699.msm-a\t(Detached)\n", running, Stopped, 1, ErrDuplicate, false},
		{"inaccessible", "Directory /tmp/sd must have mode 700.\n", running, Stopped, 0, ErrInaccessible, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := testBackend(t, lsOnly(tc.ls), tc.procs).Status(context.Background(), "msm-a", invocation)
			if !errors.Is(err, tc.errIs) || (tc.errIs == nil && err != nil) {
				t.Fatalf("err = %v", err)
			}
			if st.Liveness != tc.want || len(st.Stale) != tc.stale || (st.Detail != "") != tc.detailed {
				t.Fatalf("status = %+v", st)
			}
			if tc.want == Running && (st.Session.ID != "694.msm-a" || st.Process.PID != 695) {
				t.Fatalf("status = %+v", st)
			}
		})
	}
}

func TestStatusRunFailure(t *testing.T) {
	r := &fakeRunner{answer: func([]string) (process.Result, error) { return process.Result{}, errors.New("exec: not found") }}
	if _, err := testBackend(t, r, fakeProcs{}).Status(context.Background(), "msm-a", invocation); err == nil || errors.Is(err, ErrInaccessible) {
		t.Fatalf("err = %v", err)
	}
}

// launchWorld simulates screen: -dmS registers a session whose window
// process appears after `delay` polls, or vanishes if exitAfterStart.
type launchWorld struct {
	mu              sync.Mutex
	started         bool
	polls, delay    int
	exitAfterStart  bool
	procs           fakeProcs
	runner          *fakeRunner
	preexistingList string
}

func newLaunchWorld(delay int) *launchWorld {
	w := &launchWorld{delay: delay, procs: fakeProcs{}}
	w.runner = &fakeRunner{answer: w.answer}
	return w
}

func (w *launchWorld) answer(args []string) (process.Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch args[0] {
	case "-dmS":
		w.started = true
		w.procs[900] = ProcInfo{PID: 900, UID: os.Geteuid(), Argv: []string{"SCREEN"}}
		return process.Result{}, nil
	case "-ls":
		if !w.started {
			return process.Result{Stdout: w.preexistingList + "No Sockets found in /tmp/sd.\n"}, nil
		}
		w.polls++
		if w.exitAfterStart && w.polls > 1 {
			delete(w.procs, 900)
			return process.Result{Stdout: "No Sockets found in /tmp/sd.\n"}, nil
		}
		// Until it "execs", screen's forked window process still looks
		// like screen, which Launch must treat as not-yet-started.
		argv := []string{"SCREEN", "-dmS", "msm-a"}
		if w.polls > w.delay {
			argv = args2(invocation)
		}
		w.procs[901] = ProcInfo{PID: 901, PPID: 900, UID: os.Geteuid(), Argv: argv}
		return process.Result{Stdout: "There is a screen on:\n\t900.msm-a\t(Detached)\n"}, nil
	}
	return process.Result{}, fmt.Errorf("unexpected %q", args)
}

func args2(a []string) []string { return append([]string(nil), a...) }

func TestLaunch(t *testing.T) {
	w := newLaunchWorld(2)
	b := testBackend(t, w.runner, w.procs)
	spec := LaunchSpec{Name: "msm-a", Dir: "/srv/a", Argv: invocation, Env: []string{"EXTRA=1"}, Timeout: 10 * time.Second}
	st, err := b.Launch(context.Background(), spec)
	if err != nil || st.Liveness != Running || st.Process.PID != 901 {
		t.Fatalf("launch = %+v, %v", st, err)
	}
	var launch process.Command
	for _, c := range w.runner.calls {
		if c.Args[0] == "-dmS" {
			launch = c
		}
	}
	if want := append([]string{"-dmS", "msm-a"}, invocation...); !slices.Equal(launch.Args, want) {
		t.Fatalf("argv = %q", launch.Args)
	}
	if launch.Path != "/usr/bin/screen" || launch.Dir != "/srv/a" ||
		!slices.Equal(launch.Env, []string{"PATH=/usr/bin:/bin", "SCREENDIR=/tmp/sd", "EXTRA=1"}) {
		t.Fatalf("command = %+v", launch)
	}

	// A second launch refuses rather than starting a duplicate.
	if _, err := b.Launch(context.Background(), spec); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second launch: %v", err)
	}
}

func TestLaunchDeadlines(t *testing.T) {
	spec := LaunchSpec{Name: "msm-a", Dir: "/srv/a", Argv: invocation, Timeout: 5 * time.Second}

	slow := newLaunchWorld(100)
	if _, err := testBackend(t, slow.runner, slow.procs).Launch(context.Background(), spec); !errors.Is(err, ErrStartTimeout) || !strings.Contains(err.Error(), "SCREEN") {
		t.Fatalf("slow start: %v", err)
	}
	if slow.polls > 7 {
		t.Fatalf("polled %d times for a 5s deadline at 1s intervals", slow.polls)
	}

	dies := newLaunchWorld(100)
	dies.exitAfterStart = true
	if _, err := testBackend(t, dies.runner, dies.procs).Launch(context.Background(), spec); !errors.Is(err, ErrExited) {
		t.Fatalf("exiting start: %v", err)
	}

	for name, bad := range map[string]LaunchSpec{
		"relative dir": {Name: "msm-a", Dir: "srv", Argv: invocation, Timeout: time.Second},
		"no argv":      {Name: "msm-a", Dir: "/srv", Timeout: time.Second},
		"no timeout":   {Name: "msm-a", Dir: "/srv", Argv: invocation},
		"bad name":     {Name: "a.b", Dir: "/srv", Argv: invocation, Timeout: time.Second},
	} {
		w := newLaunchWorld(0)
		if _, err := testBackend(t, w.runner, w.procs).Launch(context.Background(), bad); err == nil || w.started {
			t.Errorf("%s: err=%v started=%v", name, err, w.started)
		}
	}
}

func TestLaunchRefusesForeignSession(t *testing.T) {
	w := newLaunchWorld(0)
	w.preexistingList = "There is a screen on:\n\t50.msm-a\t(Detached)\n"
	w.procs[50] = ProcInfo{PID: 50, UID: os.Geteuid()}
	w.procs[51] = ProcInfo{PID: 51, PPID: 50, UID: os.Geteuid(), Argv: []string{"bash"}}
	if _, err := testBackend(t, w.runner, w.procs).Launch(context.Background(),
		LaunchSpec{Name: "msm-a", Dir: "/srv", Argv: invocation, Timeout: time.Second}); !errors.Is(err, ErrAlreadyRunning) || w.started {
		t.Fatalf("err=%v started=%v", err, w.started)
	}
}

func runningStatus() Status {
	return Status{Name: "msm-a", Liveness: Running, Session: &Session{ID: "694.msm-a", PID: 694, Name: "msm-a", State: Detached}}
}

func TestSend(t *testing.T) {
	r := lsOnly("")
	b := testBackend(t, r, fakeProcs{})
	if err := b.Send(context.Background(), runningStatus(), `say $HOME ^C \n`); err != nil {
		t.Fatal(err)
	}
	want := []string{"-S", "694.msm-a", "-p", "0", "-X", "stuff", `say \$HOME \^C \\n` + "\r"}
	if got := r.argsOf("stuff"); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Fatalf("args = %q", got)
	}
	if err := b.Send(context.Background(), runningStatus(), strings.Repeat("x", MaxInputBytes)); err != nil {
		t.Fatal(err)
	}
	if got := len(r.argsOf("stuff")); got != 1+(MaxInputBytes+1+stuffChunkBytes-1)/stuffChunkBytes {
		t.Fatalf("long line took %d screen calls", got-1)
	}
	r.calls = nil
	if err := b.Send(context.Background(), runningStatus(), "two\nlines"); !errors.Is(err, ErrUnsafeInput) {
		t.Fatalf("unsafe: %v", err)
	}
	foreign := runningStatus()
	foreign.Liveness = Foreign
	if err := b.Send(context.Background(), foreign, "say hi"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("foreign: %v", err)
	}
	if len(r.argsOf("stuff")) != 0 {
		t.Fatal("refused input reached screen")
	}

	gone := &fakeRunner{answer: func([]string) (process.Result, error) {
		return process.Result{Stdout: "No screen session found.\n"}, errors.New("exit status 1")
	}}
	if err := testBackend(t, gone, fakeProcs{}).Send(context.Background(), runningStatus(), "say hi"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("gone: %v", err)
	}
}

func TestAttachRequiresTerminal(t *testing.T) {
	r := lsOnly("")
	b := testBackend(t, r, fakeProcs{})
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer out.Close()
	if err := b.Attach(context.Background(), runningStatus(), process.Stdio{In: in, Out: out, Err: out}, "xterm"); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("err = %v", err)
	}
	if err := b.Attach(context.Background(), Status{Name: "msm-a"}, process.Stdio{}, ""); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatal("attach ran screen")
	}
}

func TestWaitStoppedAndReady(t *testing.T) {
	me := os.Geteuid()
	procs := fakeProcs{694: {PID: 694, UID: me}, 695: {PID: 695, PPID: 694, UID: me, Argv: invocation}}
	up := "There is a screen on:\n\t694.msm-a\t(Detached)\n"
	remaining := 3
	r := &fakeRunner{answer: func([]string) (process.Result, error) {
		if remaining == 0 {
			return process.Result{Stdout: "No Sockets found in /tmp/sd.\n"}, nil
		}
		remaining--
		return process.Result{Stdout: up}, nil
	}}
	b := testBackend(t, r, procs)
	if st, err := b.WaitStopped(context.Background(), "msm-a", invocation, 10*time.Second); err != nil || st.Liveness != Stopped {
		t.Fatalf("stop = %+v, %v", st, err)
	}
	remaining = 100
	if _, err := b.WaitStopped(context.Background(), "msm-a", invocation, 3*time.Second); !errors.Is(err, ErrStopTimeout) {
		t.Fatalf("stop timeout: %v", err)
	}

	calls := 0
	probe := func(context.Context) (bool, error) { calls++; return calls == 2, nil }
	if _, err := b.WaitReady(context.Background(), "msm-a", invocation, 10*time.Second, probe); err != nil || calls != 2 {
		t.Fatalf("ready: %v after %d probes", err, calls)
	}
	never := func(context.Context) (bool, error) { return false, nil }
	if _, err := b.WaitReady(context.Background(), "msm-a", invocation, 3*time.Second, never); !errors.Is(err, ErrReadyTimeout) {
		t.Fatalf("ready timeout: %v", err)
	}
	remaining = 1
	if _, err := b.WaitReady(context.Background(), "msm-a", invocation, time.Hour, never); !errors.Is(err, ErrExited) {
		t.Fatalf("exit before ready: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.WaitReady(ctx, "msm-a", invocation, time.Hour, never); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}
