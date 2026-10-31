package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/backup"
	"github.com/thehatchcloud/msm/internal/buildinfo"
	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/console"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/jars"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/lifecycle"
	"github.com/thehatchcloud/msm/internal/process"
	"github.com/thehatchcloud/msm/internal/servers"
	"github.com/thehatchcloud/msm/internal/worlds"
)

type stateProber map[string]servers.StateKind

func (p stateProber) Probe(_ context.Context, s *legacyconf.ServerSettings, _ identity.Identity) servers.State {
	return servers.State{Kind: p[s.Name], Detail: "test"}
}

type serverFixture struct {
	root     string
	jars     string
	http     *http.Client
	states   stateProber
	terminal bool
	// stdin, when set, replaces the string a run is given as standard input.
	stdin io.Reader
	// info, updateEnv and installEnv, when set, replace the development
	// build and the real host the update and install commands run against.
	info       buildinfo.Info
	updateEnv  updateEnv
	installEnv installEnv
	migrateEnv migrateEnv
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
	jarRoot := filepath.Join(base, "jars")
	if err := os.Mkdir(jarRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	data := "USERNAME=\"" + me.Username + "\"\nSERVER_STORAGE_PATH=\"" + root + "\"\nDEFAULT_USERNAME=\"" + me.Username + "\"\nJAR_STORAGE_PATH=\"" + jarRoot + "\"\nWORLD_ARCHIVE_PATH=\"" + filepath.Join(base, "archives", "worlds") + "\"\nBACKUP_ARCHIVE_PATH=\"" + filepath.Join(base, "archives", "backups") + "\"\nLOG_ARCHIVE_PATH=\"" + filepath.Join(base, "archives", "logs") + "\"\n"
	if err := os.WriteFile(conf, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MSM_CONF", conf)
	return &serverFixture{root: root, jars: jarRoot, states: stateProber{}}
}

func (f *serverFixture) run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	d := deps{
		isTerminal: func(io.Reader) bool { return f.terminal },
		program:    "msm",
		servers: func(w io.Writer) (*servers.Manager, error) {
			cfg, _, err := hostConfig(w)
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
	d.worlds = func(w io.Writer) (*worlds.Manager, error) {
		m, err := d.servers(w)
		if err != nil {
			return nil, err
		}
		return worlds.New(worlds.Config{Servers: m})
	}
	d.backup = func(w io.Writer, o lifecycleOptions) (*backup.Manager, error) {
		sm, err := d.servers(w)
		if err != nil {
			return nil, err
		}
		wm, err := d.worlds(w)
		if err != nil {
			return nil, err
		}
		cm, err := d.console(w, o)
		if err != nil {
			return nil, err
		}
		return backup.New(backup.Config{Servers: sm, Worlds: wm, Console: cm, Clock: clock.Real{}, Timeout: o.backupTimeout, JarRoot: f.jars})
	}
	d.stdio = func() process.Stdio { return process.Stdio{} }
	d.console = func(w io.Writer, o lifecycleOptions) (*console.Manager, error) {
		m, err := d.servers(w)
		if err != nil {
			return nil, err
		}
		return console.New(console.Config{Servers: m, Clock: clock.Real{}, Timeout: o.timeout,
			Sessions: func(identity.Identity) (console.Sessions, error) { return nil, console.ErrNoScreen }})
	}
	d.jars = func(w io.Writer) (*jars.Manager, error) {
		cfg, js, err := hostConfig(w)
		if err != nil {
			return nil, err
		}
		cfg.Prober = f.states
		sm, err := servers.New(cfg)
		if err != nil {
			return nil, err
		}
		return jars.New(jars.Config{Root: js.root, Target: js.target, DownloadDir: js.downloadDir,
			Owner: cfg.Owner, Servers: sm, Clock: clock.Real{}, HTTP: f.http})
	}
	d.roller = hostRoller(d)
	d.pruner = hostPruner(d)
	d.globalConfig = hostGlobalConfig
	var out, errOut bytes.Buffer
	var in io.Reader = strings.NewReader(stdin)
	if f.stdin != nil {
		in = f.stdin
	}
	d.update, d.install, d.migrate = f.updateEnv, f.installEnv, f.migrateEnv
	info := f.info
	if info.Version == "" {
		info.Version = "test"
	}
	code := run(args, in, &out, &errOut, info, d)
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

	if code, _, errOut := f.run(t, "", "server", "rename", "survival", "hardcore"); code != ExitRefused || !strings.Contains(errOut, "stop it first") {
		t.Fatalf("renaming a running server: exit %d, %q", code, errOut)
	}
	f.states["survival"] = servers.Stopped
	if out := f.mustRun(t, "server", "rename", "survival", "hardcore"); !strings.HasPrefix(out, "Renamed server \"survival\" to \"hardcore\".\n") {
		t.Fatalf("rename output = %q", out)
	}
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"server", "create", "hardcore"}, ExitRefused},
		{[]string{"server", "create", "all"}, ExitUsage},
		{[]string{"server", "create"}, ExitUsage},
		{[]string{"server", "rename", "missing", "x"}, ExitNotFound},
		{[]string{"server", "list", "extra"}, ExitUsage},
	} {
		if code, out, errOut := f.run(t, "", tc.args...); code != tc.code || out != "" || strings.Count(errOut, "msm: ") != 1 {
			t.Errorf("msm %v: exit %d stdout %q stderr %q", tc.args, code, out, errOut)
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
	if code != ExitRefused || !strings.Contains(errOut, "--yes") || !strings.Contains(out, dir) || !exists() {
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
	if code, _, errOut := f.run(t, "y\n", "server", "delete", "doomed"); code != ExitRefused || !exists() || !strings.Contains(errOut, "stop it") {
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
	if code, _, errOut := f.run(t, "", "server", "list"); code != ExitConfig || !strings.Contains(errOut, conf) {
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
	if code, _, errOut := f.run(t, "", "a", "start"); code != ExitUnavailable || !strings.Contains(errOut, "screen is not installed") || !active("a") {
		t.Fatalf("start: %d %q active=%v", code, errOut, active("a"))
	}
	if code, _, _ := f.run(t, "", "b", "restart", "now"); code != ExitUnavailable || !active("b") {
		t.Fatal("restart now did not mark b active")
	}
	// The global stop leaves intent alone.
	if out := f.mustRun(t, "stop", "now"); !strings.Contains(out, "No servers were running.") || !active("a") || !active("b") {
		t.Fatalf("global stop: %q", out)
	}
	if code, _, errOut := f.run(t, "", "start", "--jobs", "1"); code != ExitUnavailable || !strings.Contains(errOut, "2 of 2 servers failed") {
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
	if code, _, errOut := f.run(t, "", "missing", "start"); code != ExitNotFound || !strings.Contains(errOut, "no server with the name") {
		t.Fatalf("missing: %d %q", code, errOut)
	}
	if code, _, errOut := f.run(t, "", "a", "stop", "--timeout", "-1s"); code != ExitUsage || !strings.Contains(errOut, "--timeout") {
		t.Fatalf("negative timeout: %d %q", code, errOut)
	}
}

// CT-CMD-013 to CT-CMD-018, CT-CMD-036, CT-CMD-037, CT-SURF-039 through the
// CLI, with the legacy messages.
func TestJargroupCommands(t *testing.T) {
	f := newServerFixture(t)
	jar := []byte("PK\x03\x04jar")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(jar) }))
	defer srv.Close()
	f.http = srv.Client()

	if out := f.mustRun(t, "jargroup", "list"); out != "" {
		t.Fatalf("empty list = %q", out)
	}
	if out := f.mustRun(t, "jargroup", "create", "paper", srv.URL+"/paper.jar"); out != "Creating jar group... Done.\n" {
		t.Fatalf("create = %q", out)
	}
	if code, _, errOut := f.run(t, "", "jargroup", "create", "old", "http://dl.bukkit.org/craftbukkit.jar"); code != ExitConfig || !strings.Contains(errOut, "https") {
		t.Fatalf("http target: %d %q", code, errOut)
	}
	out := f.mustRun(t, "jargroup", "getlatest", "paper")
	if !strings.HasPrefix(out, "Downloading latest version... Done.\nSaved as \"") {
		t.Fatalf("getlatest = %q", out)
	}
	if out := f.mustRun(t, "jargroup", "getlatest", "paper"); !strings.Contains(out, "was already up to date.") {
		t.Fatalf("second getlatest = %q", out)
	}
	if out := f.mustRun(t, "jargroup", "list"); !strings.HasPrefix(out, "paper\n    20") {
		t.Fatalf("list = %q", out)
	}
	if out := f.mustRun(t, "jargroup", "changeurl", "paper", srv.URL+"/other.jar"); out != "Changing target URL... Done.\n" {
		t.Fatalf("changeurl = %q", out)
	}

	f.mustRun(t, "server", "create", "survival")
	if out := f.mustRun(t, "survival", "jar", "paper"); !strings.Contains(out, "Server \"survival\" is now using \"") {
		t.Fatalf("jar = %q", out)
	}
	if code, _, errOut := f.run(t, "", "survival", "jar", "paper", "nope.jar"); code != ExitNotFound || !strings.Contains(errOut, `no jar named "nope.jar"`) {
		t.Fatalf("missing file: %d %q", code, errOut)
	}
	if code, _, errOut := f.run(t, "", "nobody", "jar", "paper"); code != ExitNotFound || !strings.Contains(errOut, "no server with the name") {
		t.Fatalf("missing server: %d %q", code, errOut)
	}

	// Rename re-points the server; delete refuses while it is used.
	out = f.mustRun(t, "jargroup", "rename", "paper", "paper2")
	if !strings.HasPrefix(out, "Renamed jar group \"paper\" to \"paper2\".\nServer \"survival\" is now using \"") {
		t.Fatalf("rename = %q", out)
	}
	if target, _ := os.Readlink(filepath.Join(f.root, "survival", "server.jar")); !strings.HasPrefix(target, filepath.Join(f.jars, "paper2")+"/") {
		t.Fatalf("link after rename = %s", target)
	}
	f.terminal = true
	if code, _, errOut := f.run(t, "y\n", "jargroup", "delete", "paper2"); code != ExitRefused || !strings.Contains(errOut, `server "survival"`) {
		t.Fatalf("delete in use: %d %q", code, errOut)
	}
	os.Remove(filepath.Join(f.root, "survival", "server.jar"))
	if code, out, _ := f.run(t, "n\n", "jargroup", "delete", "paper2"); code != ExitOK || !strings.HasSuffix(out, "Jar group was NOT deleted.\n") {
		t.Fatalf("declined delete: %d %q", code, out)
	}
	f.terminal = false
	if code, _, errOut := f.run(t, "y\n", "jargroup", "delete", "paper2"); code != ExitRefused || !strings.Contains(errOut, "--yes") {
		t.Fatalf("noninteractive delete: %d %q", code, errOut)
	}
	if out := f.mustRun(t, "jargroup", "delete", "--yes", "paper2"); out != "Jar group deleted.\n" {
		t.Fatalf("delete = %q", out)
	}
	if code, _, errOut := f.run(t, "", "jargroup", "delete", "--yes", "paper2"); code != ExitNotFound || !strings.Contains(errOut, "no jar group") {
		t.Fatalf("delete missing: %d %q", code, errOut)
	}
}

// CT-CMD-027, CT-CMD-028, CT-CMD-032, CT-CMD-033: the server-first worlds
// grammar, including the all-target forms and their errors.
func TestWorldsCommands(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "one")
	f.mustRun(t, "server", "create", "two")
	for _, w := range []string{"one/worldstorage/main", "one/worldstorage_inactive/spare", "two/worldstorage/other"} {
		if err := os.MkdirAll(filepath.Join(f.root, w), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if out := f.mustRun(t, "one", "worlds", "list"); out != "     main\n     spare\n" {
		t.Errorf("list = %q", out)
	}
	if out := f.mustRun(t, "all", "worlds", "list"); out != "one:      main\none:      spare\ntwo:      other\n" {
		t.Errorf("all list = %q", out)
	}
	if out := f.mustRun(t, "one", "worlds", "load"); !strings.Contains(out, "Maintaining world symbolic links...") {
		t.Errorf("load = %q", out)
	}
	if got, err := os.Readlink(filepath.Join(f.root, "one", "main")); err != nil || got != filepath.Join(f.root, "one", "worldstorage", "main") {
		t.Errorf("link = %q, %v", got, err)
	}
	if out := f.mustRun(t, "one", "worlds", "off", "main"); !strings.Contains(out, "inactive worldstorage directory... Done.") {
		t.Errorf("off = %q", out)
	}
	if out := f.mustRun(t, "one", "worlds", "on", "spare"); !strings.Contains(out, "active worldstorage directory... Done.") {
		t.Errorf("on = %q", out)
	}
	if out := f.mustRun(t, "all", "worlds", "off", "all"); !strings.Contains(out, "one: Moving world \"spare\"") || !strings.Contains(out, "two: Moving world \"other\"") {
		t.Errorf("all off all = %q", out)
	}
	if _, err := os.Stat(filepath.Join(f.root, "two", "worldstorage_inactive", "other")); err != nil {
		t.Error(err)
	}
	f.mustRun(t, "all", "worlds", "on", "all")
	if _, err := os.Stat(filepath.Join(f.root, "two", "worldstorage", "other")); err != nil {
		t.Error(err)
	}

	for _, tc := range []struct {
		args []string
		want string
		code int
	}{
		{[]string{"all", "worlds", "on", "main"}, `when specifying "all" servers, "all" worlds must be specified also`, ExitUsage},
		{[]string{"all", "worlds", "off", "main"}, `"all" worlds must be specified also`, ExitUsage},
		{[]string{"one", "worlds", "on", "all"}, `there is no world with the name "all"`, ExitNotFound},
		{[]string{"one", "worlds", "on", "ghost"}, `there is no world with the name "ghost"`, ExitNotFound},
		{[]string{"ghost", "worlds", "list"}, `there is no server with the name "ghost"`, ExitNotFound},
	} {
		code, _, errOut := f.run(t, "", tc.args...)
		if code != tc.code || !strings.Contains(errOut, tc.want) {
			t.Errorf("msm %s = %d, %q; want %q", strings.Join(tc.args, " "), code, errOut, tc.want)
		}
	}
	// Commands another task owns, and malformed ones, are the legacy answer.
	for _, args := range [][]string{{"one", "worlds"}, {"one", "worlds", "list", "x"}, {"one", "worlds", "on"}, {"one", "worlds", "ram"},
		{"one", "worlds", "todisk", "x"}, {"one", "worlds", "backup", "x"}, {"one", "backup", "x"}, {"one", "worlds", "bogus"}} {
		code, _, errOut := f.run(t, "", args...)
		if code != ExitUsage || !strings.Contains(errOut, "No such command") {
			t.Errorf("msm %s = %d, %q; want No such command", strings.Join(args, " "), code, errOut)
		}
	}

	// RAM worlds are off in this configuration: both commands say so.
	code, _, errOut := f.run(t, "", "one", "worlds", "ram", "main")
	if code != ExitConfig || !strings.Contains(errOut, "RAMDISK_STORAGE_ENABLED") {
		t.Errorf("ram = %d, %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(f.root, "one", "worldstorage", "main", "inram")); err == nil {
		t.Error("the RAM flag was set although RAM worlds are off")
	}
	if out := f.mustRun(t, "one", "worlds", "todisk"); !strings.Contains(out, "RAM worlds are off") {
		t.Errorf("todisk = %q", out)
	}
	if out := f.mustRun(t, "all", "worlds", "todisk"); !strings.Contains(out, "one: RAM worlds are off") || !strings.Contains(out, "two: RAM worlds are off") {
		t.Errorf("all todisk = %q", out)
	}
	code, _, errOut = f.run(t, "", "all", "worlds", "ram", "main")
	if code != ExitUsage || !strings.Contains(errOut, "one server at a time") {
		t.Errorf("all ram = %d, %q", code, errOut)
	}
	code, _, errOut = f.run(t, "", "one", "worlds", "ram", "ghost")
	if code != ExitNotFound || !strings.Contains(errOut, `there is no world with the name "ghost"`) {
		t.Errorf("ram ghost = %d, %q", code, errOut)
	}

	// A server not proven stopped is refused, and nothing moves.
	f.states["one"] = servers.Running
	code, _, errOut = f.run(t, "", "one", "worlds", "off", "main")
	if code != ExitRefused || !strings.Contains(errOut, "running") {
		t.Errorf("off while running = %d, %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(f.root, "one", "worldstorage", "main")); err != nil {
		t.Error(err)
	}
	// Listing is read-only and works whatever the server is doing.
	if code, _, errOut := f.run(t, "", "one", "worlds", "list"); code != ExitOK {
		t.Errorf("list while running = %d, %q", code, errOut)
	}
}

// CT-CMD-031, CT-CMD-035: msm <server> worlds backup and msm <server> backup
// write ZIP archives where the configuration says; "all" backs up every
// server, and a server not proven stopped (here: with no way to reach its
// console) is refused rather than copied while it may be writing.
func TestBackupCommands(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "one")
	f.mustRun(t, "server", "create", "two")
	for _, w := range []string{"one/worldstorage/main", "one/worldstorage_inactive/spare", "two/worldstorage/other"} {
		if err := os.MkdirAll(filepath.Join(f.root, w), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.root, w, "level.dat"), []byte(w), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archives := filepath.Join(filepath.Dir(f.root), "archives")
	count := func(dir string) int {
		m, _ := filepath.Glob(filepath.Join(dir, "*.zip"))
		return len(m)
	}
	if out := f.mustRun(t, "one", "worlds", "backup"); !strings.Contains(out, "Backup took") {
		t.Errorf("worlds backup output = %q", out)
	}
	if count(filepath.Join(archives, "worlds", "one", "main")) != 1 || count(filepath.Join(archives, "worlds", "one", "spare")) != 0 {
		t.Errorf("worlds backup wrote the wrong archives")
	}
	f.mustRun(t, "one", "backup")
	if count(filepath.Join(archives, "backups", "one")) != 1 {
		t.Errorf("complete backup not written")
	}
	f.mustRun(t, "all", "worlds", "backup")
	if count(filepath.Join(archives, "worlds", "two", "other")) != 1 {
		t.Errorf("all did not back up server two")
	}
	if code, _, errOut := f.run(t, "", "ghost", "backup"); code != ExitNotFound || !strings.Contains(errOut, `there is no server with the name "ghost"`) {
		t.Errorf("ghost backup = %d, %q", code, errOut)
	}
	if code, _, errOut := f.run(t, "", "one", "backup", "--backup-timeout=-1s"); code != ExitUsage || !strings.Contains(errOut, "--backup-timeout") {
		t.Errorf("negative timeout = %d, %q", code, errOut)
	}
	f.states["two"] = servers.Running
	before := count(filepath.Join(archives, "backups", "two"))
	if code, _, errOut := f.run(t, "", "two", "backup"); code != ExitRefused || !strings.Contains(errOut, "running") {
		t.Errorf("backup of a running server = %d, %q", code, errOut)
	}
	if count(filepath.Join(archives, "backups", "two")) != before {
		t.Errorf("a server that could not be proven stopped was archived")
	}
}
