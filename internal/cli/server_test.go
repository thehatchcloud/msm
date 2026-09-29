package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/buildinfo"
	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/lifecycle"
	"github.com/thehatchcloud/msm/internal/servers"
)

type stateProber map[string]servers.StateKind

func (p stateProber) Probe(_ context.Context, s *legacyconf.ServerSettings, _ identity.Identity) servers.State {
	return servers.State{Kind: p[s.Name], Detail: "test"}
}

type serverFixture struct {
	root     string
	states   stateProber
	terminal bool
}

// newServerFixture points MSM_CONF at a temporary msm.conf whose storage
// root and manager user are the test's, so the real host wiring is used
// with only the screen prober and terminal check replaced.
func newServerFixture(t *testing.T) *serverFixture {
	t.Helper()
	isolateConfig(t)
	me, err := identity.Current()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	root := filepath.Join(base, "servers")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(base, "msm.conf")
	data := "USERNAME=\"" + me.Username + "\"\nSERVER_STORAGE_PATH=\"" + root + "\"\nDEFAULT_USERNAME=\"" + me.Username + "\"\n"
	if err := os.WriteFile(conf, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MSM_CONF", conf)
	return &serverFixture{root: root, states: stateProber{}}
}

func (f *serverFixture) run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	d := deps{
		isTerminal: func(io.Reader) bool { return f.terminal },
		program:    "msm",
		servers: func(w io.Writer) (*servers.Manager, error) {
			cfg, err := hostConfig(w)
			if err != nil {
				return nil, err
			}
			cfg.Prober = f.states
			return servers.New(cfg)
		},
	}
	// Without screen nothing can run, so lifecycle commands exercise intent
	// and dispatch here; internal/lifecycle tests the running cases.
	d.lifecycle = func(w io.Writer, o lifecycleOptions) (*lifecycle.Manager, error) {
		m, err := d.servers(w)
		if err != nil {
			return nil, err
		}
		return lifecycle.New(lifecycle.Config{Servers: m, Clock: clock.Real{}, Timeout: o.timeout, Jobs: o.jobs,
			Sessions: func(identity.Identity) (lifecycle.Sessions, error) { return nil, lifecycle.ErrNoScreen }})
	}
	var out, errOut bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errOut, buildinfo.Info{Version: "test"}, d)
	return code, out.String(), errOut.String()
}

func (f *serverFixture) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := f.run(t, "", args...)
	if code != ExitOK {
		t.Fatalf("msm %s: exit %d, stderr %q", strings.Join(args, " "), code, errOut)
	}
	return out
}

func TestServerCommands(t *testing.T) {
	f := newServerFixture(t)
	if out := f.mustRun(t, "server", "list"); out != "[There are no servers]\n" {
		t.Fatalf("empty list = %q", out)
	}
	out := f.mustRun(t, "server", "create", "survival")
	if !strings.Contains(out, "eula.txt") {
		t.Fatalf("create output = %q", out)
	}
	f.mustRun(t, "server", "create", "creative")
	os.WriteFile(filepath.Join(f.root, "creative", "active"), nil, 0o644)
	f.states["survival"] = servers.Running
	want := "[ ACTIVE ] \"creative\" is stopped. Server is down!\n" +
		"[INACTIVE] \"survival\" is running. It should not be running!\n"
	if out := f.mustRun(t, "server", "list"); out != want {
		t.Fatalf("list = %q, want %q", out, want)
	}

	if code, _, errOut := f.run(t, "", "server", "rename", "survival", "hardcore"); code != ExitError || !strings.Contains(errOut, "stop it first") {
		t.Fatalf("renaming a running server: exit %d, %q", code, errOut)
	}
	f.states["survival"] = servers.Stopped
	if out := f.mustRun(t, "server", "rename", "survival", "hardcore"); !strings.HasPrefix(out, "Renamed server \"survival\" to \"hardcore\".\n") {
		t.Fatalf("rename output = %q", out)
	}
	for _, args := range [][]string{
		{"server", "create", "hardcore"},
		{"server", "create", "all"},
		{"server", "create"},
		{"server", "rename", "missing", "x"},
		{"server", "list", "extra"},
	} {
		if code, out, errOut := f.run(t, "", args...); code != ExitError || out != "" || strings.Count(errOut, "msm: ") != 1 {
			t.Errorf("msm %v: exit %d stdout %q stderr %q", args, code, out, errOut)
		}
	}
}

// CT-SURF-038: server deletion requires y/Y/yes and defaults to no; the Go
// port also refuses to wait for an answer that cannot come.
func TestServerDeleteConfirmation(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "doomed")
	dir := filepath.Join(f.root, "doomed")
	exists := func() bool { _, err := os.Stat(dir); return err == nil }

	// Not a terminal and no --yes: refuse without reading stdin.
	code, out, errOut := f.run(t, "yes\n", "server", "delete", "doomed")
	if code != ExitError || !strings.Contains(errOut, "--yes") || !strings.Contains(out, dir) || !exists() {
		t.Fatalf("noninteractive delete: exit %d out %q err %q", code, out, errOut)
	}

	f.terminal = true
	for _, answer := range []string{"", "\n", "n\n", "no\n", "Yes\n", "YES\n", "y es\n", " y\n"} {
		code, out, _ := f.run(t, answer, "server", "delete", "doomed")
		if code != ExitOK || !strings.Contains(out, "Server was NOT deleted.") || !exists() {
			t.Fatalf("answer %q: exit %d out %q", answer, code, out)
		}
	}
	f.states["doomed"] = servers.Running
	if code, _, errOut := f.run(t, "y\n", "server", "delete", "doomed"); code != ExitError || !exists() || !strings.Contains(errOut, "stop it") {
		t.Fatalf("deleted a running server: %d %q", code, errOut)
	}
	f.states["doomed"] = servers.Stopped
	for _, answer := range []string{"y\n", "Y\n", "yes\n"} {
		code, out, errOut := f.run(t, answer, "server", "delete", "doomed")
		if code != ExitOK || !strings.HasSuffix(out, "Server deleted.\n") || exists() {
			t.Fatalf("answer %q: exit %d out %q err %q", answer, code, out, errOut)
		}
		f.mustRun(t, "server", "create", "doomed")
	}

	f.terminal = false
	if out := f.mustRun(t, "server", "delete", "--yes", "doomed"); !strings.HasSuffix(out, "Server deleted.\n") || exists() {
		t.Fatalf("--yes delete: %q", out)
	}
}

func TestServerCommandsReportBadMsmConf(t *testing.T) {
	f := newServerFixture(t)
	conf := os.Getenv("MSM_CONF")
	os.WriteFile(conf, []byte("SERVER_STORAGE_PATH=$(rm -rf /)\n"), 0o644)
	if code, _, errOut := f.run(t, "", "server", "list"); code != ExitError || !strings.Contains(errOut, conf) {
		t.Fatalf("exit %d, %q", code, errOut)
	}
}

// CT-CMD-001 to CT-CMD-005 and CT-CMD-020 to CT-CMD-025 through the CLI:
// the server-first grammar, "all", and which commands change intent.
func TestLifecycleCommands(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "a")
	f.mustRun(t, "server", "create", "b")
	active := func(name string) bool {
		_, err := os.Lstat(filepath.Join(f.root, name, "active"))
		return err == nil
	}
	if out := f.mustRun(t, "a", "status"); out != "Server \"a\" is stopped.\n" {
		t.Fatalf("status = %q", out)
	}
	// Start marks the server active even though it cannot start here.
	if code, _, errOut := f.run(t, "", "a", "start"); code != ExitError || !strings.Contains(errOut, "screen is not installed") || !active("a") {
		t.Fatalf("start: %d %q active=%v", code, errOut, active("a"))
	}
	if code, _, _ := f.run(t, "", "b", "restart", "now"); code != ExitError || !active("b") {
		t.Fatal("restart now did not mark b active")
	}
	// The global stop leaves intent alone.
	if out := f.mustRun(t, "stop", "now"); !strings.Contains(out, "No servers were running.") || !active("a") || !active("b") {
		t.Fatalf("global stop: %q", out)
	}
	if code, _, errOut := f.run(t, "", "start", "--jobs", "1"); code != ExitError || !strings.Contains(errOut, "2 of 2 servers failed") {
		t.Fatalf("global start: %d %q", code, errOut)
	}
	// msm a stop marks only a inactive; msm all stop marks every server.
	if out := f.mustRun(t, "a", "stop"); !strings.Contains(out, "not running") || active("a") || !active("b") {
		t.Fatalf("a stop: %q", out)
	}
	f.mustRun(t, "all", "stop", "now")
	if active("a") || active("b") {
		t.Fatal("all stop left a server active")
	}
	if out := f.mustRun(t, "all", "status"); !strings.Contains(out, "a: Server \"a\" is stopped.") || !strings.Contains(out, "b: Server \"b\" is stopped.") {
		t.Fatalf("all status: %q", out)
	}
	if code, _, errOut := f.run(t, "", "missing", "start"); code != ExitError || !strings.Contains(errOut, "no server with the name") {
		t.Fatalf("missing: %d %q", code, errOut)
	}
	if code, _, errOut := f.run(t, "", "a", "stop", "--timeout", "-1s"); code != ExitError || !strings.Contains(errOut, "--timeout") {
		t.Fatalf("negative timeout: %d %q", code, errOut)
	}
}
