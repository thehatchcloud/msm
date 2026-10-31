package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/servers"
)

// legacyTree writes a legacy installation below a temporary --root, from the
// files this repository ships, and returns the root.
func legacyTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	put := func(logical, repo string, mode os.FileMode) {
		data, err := os.ReadFile(filepath.Join("..", "..", repo))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(root, logical)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	put("etc/msm.conf", "msm.conf", 0o644)
	put("etc/cron.d/msm", "cron/msm", 0o644)
	put("etc/init.d/msm", "init/msm", 0o755)
	if err := os.MkdirAll(filepath.Join(root, "usr/local/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/init.d/msm", filepath.Join(root, "usr/local/bin/msm")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"opt/msm/servers/alpha/worldstorage/world", "opt/msm/jars"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "opt/msm/servers/alpha/worldstorage/world/level.dat"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func migrateFixture(t *testing.T) (*serverFixture, string) {
	t.Helper()
	f := newServerFixture(t)
	root := legacyTree(t)
	bin := filepath.Join(root, "go-msm")
	if err := os.WriteFile(bin, []byte("\x7fELF go"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.migrateEnv = migrateEnv{
		euid:       1000,
		executable: func() (string, error) { return bin, nil },
		lookup: func(name string) (identity.Identity, error) {
			return identity.Identity{Username: name, UID: 1000, GID: 1000}, nil
		},
		prober:   func() servers.Prober { return f.states },
		commands: execCommands{},
	}
	return f, root
}

func TestMigratePreflightReportsAndChangesNothing(t *testing.T) {
	f, root := migrateFixture(t)
	code, out, errOut := f.run(t, "", "migrate", "preflight", "--root", root)
	if code != ExitOK {
		t.Fatalf("preflight = %d %q %q", code, out, errOut)
	}
	for _, want := range []string{"alpha", "maintenance run backup", "prune world-backups (30 days)", "No blockers. Nothing was changed.", "install-service"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/msm-migration")); err == nil {
		t.Error("the preflight created the state directory")
	}

	f.states["alpha"] = servers.Running
	code, out, errOut = f.run(t, "", "migrate", "preflight", "--root", root)
	if code != ExitRefused || !strings.Contains(out, "[BLOCKER] screen alpha") || !strings.Contains(errOut, "blockers") {
		t.Errorf("running server = %d %q %q", code, out, errOut)
	}
}

func TestMigrateApplyAndRollbackThroughTheCommand(t *testing.T) {
	f, root := migrateFixture(t)
	cron := filepath.Join(root, "etc/cron.d/msm")
	orig, _ := os.ReadFile(cron)

	// Neither a backup choice nor a confirmation: nothing happens.
	if code, _, errOut := f.run(t, "", "migrate", "apply", "--root", root, "--yes"); code != ExitUsage || !strings.Contains(errOut, "--backup-servers") {
		t.Errorf("no backup choice = %d %q", code, errOut)
	}
	if code, _, errOut := f.run(t, "", "migrate", "apply", "--root", root, "--external-backup"); code != ExitRefused || !strings.Contains(errOut, "--yes") {
		t.Errorf("no confirmation = %d %q", code, errOut)
	}
	if code, _, errOut := f.run(t, "", "migrate", "apply", "--root", root, "--backup-servers", "--external-backup", "--yes"); code != ExitUsage {
		t.Errorf("both backup choices = %d %q", code, errOut)
	}
	if _, err := os.Stat(cron); err != nil {
		t.Fatal("a refused apply removed the cron file")
	}
	if code, out, _ := f.run(t, "", "migrate", "apply", "--root", root, "--external-backup", "--dry-run"); code != ExitOK || !strings.Contains(out, "Dry run") {
		t.Errorf("dry run = %d %q", code, out)
	}
	if _, err := os.Stat(cron); err != nil {
		t.Fatal("a dry run removed the cron file")
	}

	code, out, errOut := f.run(t, "", "migrate", "apply", "--root", root, "--external-backup", "--yes")
	if code != ExitOK {
		t.Fatalf("apply = %d %q %q", code, out, errOut)
	}
	for _, want := range []string{"(rehearsal) would run: systemctl enable msm.service", "Nothing was started"} {
		if !strings.Contains(out, want) {
			t.Errorf("apply output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(cron); err == nil {
		t.Error("the legacy cron file is still there")
	}
	if code, _, errOut := f.run(t, "", "migrate", "apply", "--root", root, "--external-backup", "--yes"); code != ExitRefused || !strings.Contains(errOut, "already migrated") {
		t.Errorf("second apply = %d %q", code, errOut)
	}

	if code, _, errOut := f.run(t, "", "migrate", "rollback", "--root", root); code != ExitRefused || !strings.Contains(errOut, "--yes") {
		t.Errorf("rollback without confirmation = %d %q", code, errOut)
	}
	if code, out, errOut := f.run(t, "", "migrate", "rollback", "--root", root, "--yes"); code != ExitOK || !strings.Contains(out, "world data was not touched") {
		t.Fatalf("rollback = %d %q %q", code, out, errOut)
	}
	if now, _ := os.ReadFile(cron); string(now) != string(orig) {
		t.Error("the cron file was not restored")
	}
	if code, _, errOut := f.run(t, "", "migrate", "rollback", "--root", root, "--yes"); code != ExitRefused || !strings.Contains(errOut, "already rolled back") {
		t.Errorf("second rollback = %d %q", code, errOut)
	}
}

func TestMigrateNeedsRootOutsideARehearsal(t *testing.T) {
	f, _ := migrateFixture(t)
	code, _, errOut := f.run(t, "", "migrate", "apply", "--external-backup", "--yes", "--legacy-conf", "/nonexistent/msm.conf")
	if code != ExitRefused || !strings.Contains(errOut, "needs root") {
		t.Errorf("apply as non-root = %d %q", code, errOut)
	}
}

func TestMigrateBackupServersNeedsTheRealInstallation(t *testing.T) {
	f, root := migrateFixture(t)
	code, _, errOut := f.run(t, "", "migrate", "apply", "--root", root, "--backup-servers", "--yes")
	if code != ExitUsage || !strings.Contains(errOut, "--external-backup when rehearsing") {
		t.Errorf("--backup-servers with --root = %d %q", code, errOut)
	}
}

func TestMigrateRefusesBeforeAskingWhenBlocked(t *testing.T) {
	f, root := migrateFixture(t)
	f.states["alpha"] = servers.Running
	f.terminal = true
	code, out, errOut := f.run(t, "y\n", "migrate", "apply", "--root", root, "--external-backup")
	if code != ExitRefused || strings.Contains(out, "[y/N]") || !strings.Contains(errOut, "blockers") {
		t.Errorf("blocked apply = %d %q %q: it must refuse without a question", code, out, errOut)
	}
}
