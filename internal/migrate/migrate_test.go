package migrate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/servers"
)

// fakeProber reports the state a test chose for each server.
type fakeProber map[string]servers.StateKind

func (p fakeProber) Probe(_ context.Context, s *legacyconf.ServerSettings, _ identity.Identity) servers.State {
	return servers.State{Kind: p[s.Name], Detail: "test"}
}

// fakeCommands records every command and runs a hook first, so a test can
// look at the file system at the moment a command runs.
type fakeCommands struct {
	log     []string
	enabled string // what "is-enabled" prints
	failOn  string // a command line that fails
	hook    func(line string)
}

func (c *fakeCommands) Have(string) bool { return true }
func (c *fakeCommands) Run(name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	c.log = append(c.log, line)
	if c.hook != nil {
		c.hook(line)
	}
	if line == c.failOn {
		return "", errors.New("systemctl failed")
	}
	if strings.Contains(line, "is-enabled") {
		return c.enabled + "\n", nil
	}
	return "", nil
}

type fixture struct {
	t      *testing.T
	root   string
	layout Layout
	bin    string
}

func readRepo(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (f *fixture) write(logical string, data string, mode fs.FileMode) {
	f.t.Helper()
	p := f.layout.P(logical)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		f.t.Fatal(err)
	}
}

// newFixture builds a legacy installation from the files this repository
// ships (msm.conf, cron/msm, init/msm), with two servers.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, root: root, layout: DefaultLayout("").under(root)}
	f.write("/etc/msm.conf", string(readRepo(t, "msm.conf")), 0o644)
	f.write("/etc/cron.d/msm", string(readRepo(t, "cron/msm")), 0o644)
	f.write("/etc/init.d/msm", string(readRepo(t, "init/msm")), 0o755)
	f.write("/etc/profile.d/msm.sh", "", 0o644)
	if err := os.Remove(f.layout.P("/etc/profile.d/msm.sh")); err != nil {
		t.Fatal(err)
	}
	f.write("/etc/bash_completion.d/msm", string(readRepo(t, "bash_completion/msm")), 0o644)
	if err := os.MkdirAll(f.layout.P("/usr/local/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/init.d/msm", f.layout.P("/usr/local/bin/msm")); err != nil {
		t.Fatal(err)
	}
	f.write("/opt/msm/jars/minecraft/target.txt", "https://example.invalid/server.jar\n", 0o644)
	f.write("/opt/msm/jars/minecraft/2024-01-01-00-00-00-server.jar", "PK\x03\x04", 0o644)
	for _, name := range []string{"alpha", "beta"} {
		dir := "/opt/msm/servers/" + name
		f.write(dir+"/server.properties", "motd=hello\n", 0o644)
		f.write(dir+"/worldstorage/world/level.dat", "level-"+name, 0o644)
		f.write(dir+"/worldstorage/world/region/r.0.0.mca", strings.Repeat(name, 100), 0o644)
		f.write(dir+"/worldstorage_inactive/spare/level.dat", "spare", 0o644)
		if err := os.Symlink("/opt/msm/jars/minecraft/2024-01-01-00-00-00-server.jar", f.layout.P(dir+"/server.jar")); err != nil {
			t.Fatal(err)
		}
	}
	f.write("/opt/msm/servers/alpha/active", "", 0o644)
	f.bin = filepath.Join(root, "msm-go")
	if err := os.WriteFile(f.bin, []byte("\x7fELF go executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// under is Layout.Root set, for the fixture only.
func (l Layout) under(root string) Layout { l.Root = root; return l }

func (f *fixture) env(states fakeProber) Env {
	return Env{
		Layout: f.layout, Prober: states,
		Lookup: func(name string) (identity.Identity, error) {
			return identity.Identity{Username: name, UID: 1000, GID: 1000}, nil
		},
	}
}

func (f *fixture) options(c Commands) Options {
	return Options{
		Env: f.env(fakeProber{"alpha": servers.Stopped, "beta": servers.Stopped}), Commands: c,
		Binary: f.bin, ExternalBackup: true, Out: io.Discard,
	}
}

// snapshot records every entry below dir: type, mode, size and content hash,
// and the modification time when withTimes is set.
func snapshot(t *testing.T, dir string, withTimes bool, skip ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		for _, s := range skip {
			if rel == s || strings.HasPrefix(rel, s+"/") {
				return filepath.SkipDir
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		desc := fmt.Sprintf("%v", info.Mode())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			desc += " -> " + target
		case info.Mode().IsRegular():
			data, _ := os.ReadFile(p)
			desc += fmt.Sprintf(" %x", sha256.Sum256(data))
		}
		if withTimes {
			desc += fmt.Sprintf(" %d", info.ModTime().UnixNano())
		}
		out[rel] = desc
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diff(a, b map[string]string) []string {
	var out []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			out = append(out, "removed: "+k)
		} else if v != w {
			out = append(out, "changed: "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, "added: "+k)
		}
	}
	sort.Strings(out)
	return out
}

func has(inv *Inventory, sev Severity, area, substr string) bool {
	for _, f := range inv.Findings {
		if f.Severity == sev && f.Area == area && strings.Contains(f.Message+f.Subject, substr) {
			return true
		}
	}
	return false
}

func TestPreflightInventoriesTheShippedInstallation(t *testing.T) {
	f := newFixture(t)
	inv, err := Collect(context.Background(), f.env(fakeProber{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(inv.Servers); strings.Join(got, ",") != "alpha,beta" {
		t.Fatalf("servers = %v", got)
	}
	alpha := inv.Servers[0]
	if !alpha.Active || inv.Servers[1].Active {
		t.Errorf("active flags: alpha=%v beta=%v, want true false", alpha.Active, inv.Servers[1].Active)
	}
	if alpha.ScreenName != "msm-alpha" || alpha.Owner != "minecraft" || len(alpha.Worlds) != 2 {
		t.Errorf("alpha = %+v", alpha)
	}
	if len(inv.JarGroups) != 1 || inv.JarGroups[0].Name != "minecraft" || inv.JarGroups[0].JARs != 1 {
		t.Errorf("jar groups = %+v", inv.JarGroups)
	}
	if len(inv.Archives) != 3 || inv.InitVersion != "0.11.0" || inv.ManagerUser != "minecraft" {
		t.Errorf("archives=%d init=%q user=%q", len(inv.Archives), inv.InitVersion, inv.ManagerUser)
	}
	jobs, retention := 0, 0
	for _, e := range inv.Cron {
		switch e.Kind {
		case KindJob:
			jobs++
		case KindRetention:
			retention++
		default:
			t.Errorf("unexpected cron entry %+v", e)
		}
	}
	if jobs != 4 || retention != 4 {
		t.Errorf("cron jobs=%d retention=%d, want 4 and 4", jobs, retention)
	}
	if blockers := inv.Blocked(nil); len(blockers) != 0 {
		t.Errorf("a stock installation has blockers: %+v", blockers)
	}
}

func TestPreflightChangesNothing(t *testing.T) {
	f := newFixture(t)
	before := snapshot(t, f.root, true)
	if _, err := Collect(context.Background(), f.env(fakeProber{})); err != nil {
		t.Fatal(err)
	}
	if _, err := Preflight(context.Background(), f.options(nil)); err != nil {
		t.Fatal(err)
	}
	if d := diff(before, snapshot(t, f.root, true)); len(d) > 0 {
		t.Fatalf("the preflight changed the installation: %v", d)
	}
	// A dry run of apply and of rollback is read-only too.
	o := f.options(&fakeCommands{})
	o.DryRun = true
	if err := Apply(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if d := diff(before, snapshot(t, f.root, true)); len(d) > 0 {
		t.Fatalf("apply --dry-run changed the installation: %v", d)
	}
}

func TestPreflightReportsUnsupportedCustomizations(t *testing.T) {
	f := newFixture(t)
	f.write("/etc/cron.d/msm", string(readRepo(t, "cron/msm"))+
		"30 3 * * * minecraft /opt/scripts/nightly.sh >> /var/log/x 2>&1\n"+
		"0 6 * * 0 minecraft /etc/init.d/msm alpha restart\n", 0o644)
	f.write("/etc/profile.d/msm.sh", "export JAVA_HOME=/opt/java\n", 0o644)
	f.write("/opt/msm/versioning/minecraft/1.0.sh", "#!/bin/bash\n", 0o644)
	f.write("/opt/msm/servers/beta/worldstorage/world/inram", "", 0o644)
	inv, err := Collect(context.Background(), f.env(fakeProber{}))
	if err != nil {
		t.Fatal(err)
	}
	if !has(inv, Blocker, "schedule", "nightly.sh") {
		t.Errorf("an unsupported cron line is not reported: %+v", inv.Findings)
	}
	if has(inv, Blocker, "schedule", "alpha restart") {
		t.Error("a plain msm command line was refused")
	}
	for _, want := range []struct {
		sev        Severity
		area, text string
	}{
		{Warning, "customization", "/etc/profile.d/msm.sh"},
		{Info, "customization", "versioning scripts"},
		{Warning, "worlds", "RAM flag"},
	} {
		if !has(inv, want.sev, want.area, want.text) {
			t.Errorf("missing %s %s finding %q", want.sev, want.area, want.text)
		}
	}
	if len(inv.Blocked(nil)) == 0 {
		t.Error("the unsupported cron line does not block")
	}
	if left := inv.Blocked(map[string]bool{CodeUnsupportedSchedule: true}); len(left) != 0 {
		t.Errorf("acknowledging the schedule leaves blockers: %+v", left)
	}
}

func TestPreflightRejectsShellInConfigAndInvocation(t *testing.T) {
	f := newFixture(t)
	f.write("/opt/msm/servers/beta/server.properties", "msm-invocation=java -jar {JAR} $EXTRA\n", 0o644)
	inv, _ := Collect(context.Background(), f.env(fakeProber{}))
	if !has(inv, Blocker, "customization", "beta") {
		t.Errorf("a shell INVOCATION is not a blocker: %+v", inv.Findings)
	}

	f = newFixture(t)
	f.write("/etc/msm.conf", "USERNAME=\"$(whoami)\"\n", 0o644)
	inv, _ = Collect(context.Background(), f.env(fakeProber{}))
	if !has(inv, Blocker, "config", "evaluated") {
		t.Errorf("shell in msm.conf is not a blocker: %+v", inv.Findings)
	}
}

func TestPreflightNeedsAStoppedWindow(t *testing.T) {
	f := newFixture(t)
	for _, k := range []servers.StateKind{servers.Running, servers.Occupied, servers.Unknown} {
		inv, _ := Collect(context.Background(), f.env(fakeProber{"alpha": k}))
		if !has(inv, Blocker, "screen", "alpha") {
			t.Errorf("a %v server does not block", k)
		}
	}
	env := f.env(nil)
	env.Prober = nil
	inv, _ := Collect(context.Background(), env)
	if !has(inv, Blocker, "screen", "alpha") {
		t.Error("without a prober nothing is proven stopped, yet nothing blocks")
	}
}

func TestApplyAndRollbackRestoreTheLegacyInstallation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	state := "var"
	before := snapshot(t, f.root, false)
	serversBefore := snapshot(t, f.layout.P("/opt/msm"), true)

	c := &fakeCommands{enabled: "enabled"}
	// At the moment the Go service is enabled, the legacy manager must
	// already be unable to run: that is the "never two writers" rule.
	var atEnable []string
	c.hook = func(line string) {
		if line != "systemctl enable msm.service" {
			return
		}
		if _, err := os.Lstat(f.layout.P("/etc/cron.d/msm")); err == nil {
			atEnable = append(atEnable, "legacy cron file exists")
		}
		if info, err := os.Lstat(f.layout.P("/etc/init.d/msm")); err == nil && info.Mode()&0o111 != 0 {
			atEnable = append(atEnable, "legacy init script is executable")
		}
		if _, err := os.Lstat(f.layout.P("/etc/cron.d/msm-maintenance")); err != nil {
			atEnable = append(atEnable, "Go cron file not yet written")
		}
	}
	if err := Apply(ctx, f.options(c)); err != nil {
		t.Fatal(err)
	}
	if len(atEnable) != 0 {
		t.Errorf("when the Go service was enabled: %v", atEnable)
	}

	// After apply: legacy out of action, Go in place, servers untouched.
	if _, err := os.Lstat(f.layout.P("/etc/cron.d/msm")); err == nil {
		t.Error("the legacy cron file is still there")
	}
	if info, _ := os.Stat(f.layout.P("/etc/init.d/msm")); info.Mode()&0o111 != 0 {
		t.Error("the legacy init script is still executable")
	}
	if data, err := os.ReadFile(f.layout.P("/usr/local/bin/msm")); err != nil || !strings.HasPrefix(string(data), "\x7fELF") {
		t.Errorf("the Go executable is not installed: %v", err)
	}
	cron, err := os.ReadFile(f.layout.P("/etc/cron.d/msm-maintenance"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"maintenance run backup", "maintenance run logroll", "maintenance run todisk", "maintenance run recover", "maintenance run prune --days 30"} {
		if !strings.Contains(string(cron), want) {
			t.Errorf("Go cron file lacks %q:\n%s", want, cron)
		}
	}
	if _, err := os.Stat(f.layout.P("/etc/systemd/system/msm.service")); err != nil {
		t.Errorf("no Go unit: %v", err)
	}
	if d := diff(serversBefore, snapshot(t, f.layout.P("/opt/msm"), true)); len(d) > 0 {
		t.Errorf("apply wrote in the server tree: %v", d)
	}
	j, err := ReadJournal(f.layout)
	if err != nil || j.Phase != PhaseApplied {
		t.Fatalf("journal = %+v, %v", j, err)
	}
	if err := Apply(ctx, f.options(&fakeCommands{})); !errors.Is(err, ErrMigrated) {
		t.Errorf("a second apply = %v, want ErrMigrated", err)
	}

	// Rollback restores every control file byte for byte and leaves the
	// servers alone.
	var rollbackLog []string
	c2 := &fakeCommands{enabled: "enabled", hook: func(l string) { rollbackLog = append(rollbackLog, l) }}
	if err := Rollback(ctx, f.options(c2)); err != nil {
		t.Fatal(err)
	}
	if d := diff(before, snapshot(t, f.root, false, state)); len(d) > 0 {
		t.Errorf("rollback left differences: %v", d)
	}
	if d := diff(serversBefore, snapshot(t, f.layout.P("/opt/msm"), true)); len(d) > 0 {
		t.Errorf("rollback wrote in the server tree: %v", d)
	}
	joined := strings.Join(rollbackLog, "\n")
	if !strings.Contains(joined, "systemctl enable msm.service") || !strings.Contains(joined, "systemctl disable msm.service") {
		t.Errorf("rollback did not move the service back; commands:\n%s", joined)
	}
	if err := Rollback(ctx, f.options(&fakeCommands{})); !errors.Is(err, ErrNoMigration) {
		t.Errorf("a second rollback = %v, want ErrNoMigration", err)
	}

	// It can be applied again after a rollback.
	if err := Apply(ctx, f.options(&fakeCommands{enabled: "enabled"})); err != nil {
		t.Fatalf("apply after rollback: %v", err)
	}
}

func TestApplyDisablesTheLegacyServiceBeforeEnablingTheGoOne(t *testing.T) {
	f := newFixture(t)
	f.write("/etc/systemd/system/msm.service", "[Service]\nExecStart=/etc/init.d/msm start\n", 0o644)
	c := &fakeCommands{enabled: "enabled"}
	if err := Apply(context.Background(), f.options(c)); err != nil {
		t.Fatal(err)
	}
	disable, enable := -1, -1
	for i, l := range c.log {
		if l == "systemctl disable msm.service" && disable < 0 {
			disable = i
		}
		if l == "systemctl enable msm.service" {
			enable = i
		}
	}
	if disable < 0 || enable < 0 || disable > enable {
		t.Fatalf("commands = %v", c.log)
	}
	unit, _ := os.ReadFile(f.layout.P("/etc/systemd/system/msm.service"))
	if !strings.Contains(string(unit), "oneshot") {
		t.Errorf("the legacy unit was not replaced by the Go unit:\n%s", unit)
	}
	// Rollback puts the legacy unit back.
	if err := Rollback(context.Background(), f.options(c)); err != nil {
		t.Fatal(err)
	}
	unit, _ = os.ReadFile(f.layout.P("/etc/systemd/system/msm.service"))
	if !strings.Contains(string(unit), "/etc/init.d/msm start") {
		t.Errorf("rollback did not restore the legacy unit:\n%s", unit)
	}
}

func TestApplyRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("running server", func(t *testing.T) {
		f := newFixture(t)
		o := f.options(&fakeCommands{})
		o.Env = f.env(fakeProber{"alpha": servers.Running})
		before := snapshot(t, f.root, true)
		if err := Apply(ctx, o); !errors.Is(err, ErrBlocked) {
			t.Fatalf("err = %v, want ErrBlocked", err)
		}
		if d := diff(before, snapshot(t, f.root, true)); len(d) > 0 {
			t.Errorf("a refused apply changed the installation: %v", d)
		}
	})
	t.Run("no data backup choice", func(t *testing.T) {
		f := newFixture(t)
		o := f.options(&fakeCommands{})
		o.ExternalBackup = false
		if err := Apply(ctx, o); !errors.Is(err, ErrNoBackup) {
			t.Fatalf("err = %v, want ErrNoBackup", err)
		}
	})
	t.Run("not root", func(t *testing.T) {
		f := newFixture(t)
		o := f.options(&fakeCommands{})
		o.Env.Layout.Root = ""
		o.EUID = 1000
		if err := Apply(ctx, o); !errors.Is(err, ErrPrivilege) {
			t.Fatalf("err = %v, want ErrPrivilege", err)
		}
		if err := Rollback(ctx, o); !errors.Is(err, ErrPrivilege) {
			t.Fatalf("rollback err = %v, want ErrPrivilege", err)
		}
	})
	t.Run("another migration holds the lock", func(t *testing.T) {
		f := newFixture(t)
		if err := os.MkdirAll(f.layout.P(f.layout.StateDir), 0o700); err != nil {
			t.Fatal(err)
		}
		lock, err := filelock.TryAcquire(lockPath(f.layout))
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Release()
		if err := Apply(ctx, f.options(&fakeCommands{})); !errors.Is(err, filelock.ErrBusy) {
			t.Fatalf("err = %v, want ErrBusy", err)
		}
	})
	t.Run("unsupported cron line", func(t *testing.T) {
		f := newFixture(t)
		f.write("/etc/cron.d/msm", string(readRepo(t, "cron/msm"))+"0 1 * * * minecraft /opt/x.sh\n", 0o644)
		o := f.options(&fakeCommands{})
		if err := Apply(ctx, o); !errors.Is(err, ErrBlocked) {
			t.Fatalf("err = %v, want ErrBlocked", err)
		}
		o.AcceptUnsupported = true
		if err := Apply(ctx, o); err != nil {
			t.Fatal(err)
		}
		cron, _ := os.ReadFile(f.layout.P("/etc/cron.d/msm-maintenance"))
		if !strings.Contains(string(cron), "# NOT MIGRATED") || !strings.Contains(string(cron), "/opt/x.sh") {
			t.Errorf("the dropped line is not kept as a comment:\n%s", cron)
		}
		backed, _ := os.ReadFile(filepath.Join(f.layout.backupDir(), "01-msm"))
		if !strings.Contains(string(backed), "/opt/x.sh") {
			t.Error("the backup does not hold the legacy cron file")
		}
	})
	t.Run("a script is not the Go executable", func(t *testing.T) {
		f := newFixture(t)
		if err := os.WriteFile(f.bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, f.root, false, "var")
		if err := Apply(ctx, f.options(&fakeCommands{})); err == nil || !strings.Contains(err.Error(), "script") {
			t.Fatalf("err = %v", err)
		}
		if d := diff(before, snapshot(t, f.root, false, "var")); len(d) > 0 {
			t.Errorf("the failed apply was not rolled back: %v", d)
		}
	})
}

func TestFailedApplyRollsItselfBack(t *testing.T) {
	for _, failOn := range []string{
		"systemctl disable msm.service",
		"systemctl daemon-reload",
		"systemctl enable msm.service",
	} {
		t.Run(failOn, func(t *testing.T) {
			f := newFixture(t)
			f.write("/etc/systemd/system/msm.service", "[Service]\nExecStart=/etc/init.d/msm start\n", 0o644)
			before := snapshot(t, f.root, false)
			c := &fakeCommands{enabled: "enabled", failOn: failOn}
			err := Apply(context.Background(), f.options(c))
			if err == nil {
				t.Fatal("apply succeeded although a command failed")
			}
			// When the failing command is also needed to undo, the automatic
			// rollback says so and the journal stays; the administrator then
			// runs "msm migrate rollback" with systemctl working.
			if j, _ := ReadJournal(f.layout); j != nil && j.Phase != PhaseRolledBack {
				if !strings.Contains(err.Error(), "msm migrate rollback") {
					t.Fatalf("err = %v, want it to point at msm migrate rollback", err)
				}
				if err := Rollback(context.Background(), f.options(&fakeCommands{enabled: "enabled"})); err != nil {
					t.Fatal(err)
				}
			} else if !strings.Contains(err.Error(), "restored") {
				t.Fatalf("err = %v, want it to say everything was restored", err)
			}
			if d := diff(before, snapshot(t, f.root, false, "var")); len(d) > 0 {
				t.Errorf("not restored: %v", d)
			}
		})
	}
}

func TestInterruptedApplyMustBeRolledBack(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	before := snapshot(t, f.root, false)
	// Make the run stop part-way, as a crash would: the hook panics after
	// the legacy manager is disabled.
	c := &fakeCommands{enabled: "enabled"}
	c.hook = func(line string) {
		if line == "systemctl enable msm.service" {
			panic("power loss")
		}
	}
	func() {
		defer func() { _ = recover() }()
		_ = Apply(ctx, f.options(c))
	}()
	if err := Apply(ctx, f.options(&fakeCommands{})); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("apply after a crash = %v, want ErrInterrupted", err)
	}
	if err := Rollback(ctx, f.options(&fakeCommands{enabled: "enabled"})); err != nil {
		t.Fatal(err)
	}
	if d := diff(before, snapshot(t, f.root, false, "var")); len(d) > 0 {
		t.Errorf("rollback after a crash left differences: %v", d)
	}
}

func TestRollbackNeedsAStoppedWindowAndKeepsWhatItRemoves(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := Apply(ctx, f.options(&fakeCommands{enabled: "enabled"})); err != nil {
		t.Fatal(err)
	}
	o := f.options(&fakeCommands{})
	o.Env = f.env(fakeProber{"beta": servers.Running})
	if err := Rollback(ctx, o); !errors.Is(err, ErrNotStopped) {
		t.Fatalf("rollback with a running server = %v, want ErrNotStopped", err)
	}
	// The administrator updated the Go executable meanwhile; rollback keeps
	// a copy of what it removes.
	if err := os.WriteFile(f.layout.P("/usr/local/bin/msm"), []byte("\x7fELF updated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(ctx, f.options(&fakeCommands{enabled: "enabled"})); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadDir(f.layout.savedDir())
	found := false
	for _, e := range saved {
		if data, _ := os.ReadFile(filepath.Join(f.layout.savedDir(), e.Name())); string(data) == "\x7fELF updated" {
			found = true
		}
	}
	if !found {
		t.Errorf("the removed Go executable was not kept in %s: %v", f.layout.savedDir(), saved)
	}
	if target, err := os.Readlink(f.layout.P("/usr/local/bin/msm")); err != nil || target != "/etc/init.d/msm" {
		t.Errorf("command link = %q, %v", target, err)
	}
}

func TestRollbackRestoresAChangedConfiguration(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	orig, _ := os.ReadFile(f.layout.P("/etc/msm.conf"))
	if err := Apply(ctx, f.options(&fakeCommands{enabled: "enabled"})); err != nil {
		t.Fatal(err)
	}
	f.write("/etc/msm.conf", string(orig)+"\nRDIFF_BACKUP_ENABLED=\"true\"\n", 0o644)
	if err := Rollback(ctx, f.options(&fakeCommands{enabled: "enabled"})); err != nil {
		t.Fatal(err)
	}
	now, _ := os.ReadFile(f.layout.P("/etc/msm.conf"))
	if string(now) != string(orig) {
		t.Error("the configuration was not restored")
	}
}

func TestVerifyCatchesAChangedWorld(t *testing.T) {
	f := newFixture(t)
	c := &fakeCommands{enabled: "enabled"}
	// Something writes to a world while the migration runs: apply must fail
	// and undo itself.
	c.hook = func(line string) {
		if line == "systemctl enable msm.service" {
			f.write("/opt/msm/servers/alpha/worldstorage/world/level.dat", "tampered", 0o644)
		}
	}
	err := Apply(context.Background(), f.options(c))
	if err == nil || !strings.Contains(err.Error(), "worlds of alpha changed") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Lstat(f.layout.P("/etc/cron.d/msm")); statErr != nil {
		t.Error("the legacy cron file was not restored")
	}
}

// flipProber reports every server stopped for the first n probes and running
// after that, as when a server is started between two looks.
type flipProber struct{ n, calls *int }

func (p flipProber) Probe(context.Context, *legacyconf.ServerSettings, identity.Identity) servers.State {
	*p.calls++
	if *p.calls > *p.n {
		return servers.State{Kind: servers.Running}
	}
	return servers.State{Kind: servers.Stopped}
}

func TestTheWindowIsCheckedAgainUnderTheLock(t *testing.T) {
	f := newFixture(t)
	n, calls := 2, 0 // two servers: the first look sees them stopped, the second does not
	o := f.options(&fakeCommands{})
	o.Env.Prober = flipProber{&n, &calls}
	before := snapshot(t, f.root, false, "var")
	if err := Apply(context.Background(), o); !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if d := diff(before, snapshot(t, f.root, false, "var")); len(d) > 0 {
		t.Errorf("a server started after the first look, yet apply changed: %v", d)
	}

	// The same for rollback.
	f = newFixture(t)
	if err := Apply(context.Background(), f.options(&fakeCommands{enabled: "enabled"})); err != nil {
		t.Fatal(err)
	}
	calls = 0
	o = f.options(&fakeCommands{})
	o.Env.Prober = flipProber{&n, &calls}
	if err := Rollback(context.Background(), o); !errors.Is(err, ErrNotStopped) {
		t.Fatalf("rollback err = %v, want ErrNotStopped", err)
	}
	if _, err := os.Lstat(f.layout.P("/etc/cron.d/msm-maintenance")); err != nil {
		t.Error("a refused rollback removed the Go cron file")
	}
}

func TestUnsafePathsAreRefusedBeforeAnythingChanges(t *testing.T) {
	f := newFixture(t)
	conf, _ := os.ReadFile(f.layout.P("/etc/msm.conf"))
	f.write("/etc/msm.conf", strings.Replace(string(conf), `SERVER_STORAGE_PATH="/opt/msm/servers"`, `SERVER_STORAGE_PATH="opt/../msm/servers"`, 1), 0o644)
	inv, err := Collect(context.Background(), f.env(fakeProber{}))
	if err != nil {
		t.Fatal(err)
	}
	if !has(inv, Blocker, "config", "SERVER_STORAGE_PATH") {
		t.Errorf("a relative server root is not a blocker: %+v", inv.Findings)
	}

	// A configuration path that would put a second line into the cron file.
	f = newFixture(t)
	o := f.options(&fakeCommands{})
	o.Env.Layout.Conf = "/etc/msm conf"
	f.write("/etc/msm conf", string(readRepo(t, "msm.conf")), 0o644)
	before := snapshot(t, f.root, false, "var")
	if err := Apply(context.Background(), o); err == nil || !strings.Contains(err.Error(), "configuration file") {
		t.Fatalf("err = %v", err)
	}
	if d := diff(before, snapshot(t, f.root, false, "var")); len(d) > 0 {
		t.Errorf("not restored: %v", d)
	}
}
