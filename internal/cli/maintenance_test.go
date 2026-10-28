package cli

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/servers"
)

func (f *serverFixture) archives() string { return filepath.Join(filepath.Dir(f.root), "archives") }

func (f *serverFixture) writeAged(t *testing.T, path, content string, days int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func (f *serverFixture) writeZip(t *testing.T, path string, days int) {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("world/level.dat")
	w.Write([]byte("x"))
	zw.Close()
	f.writeAged(t, path, b.String(), days)
}

// legacyServer creates a server for Minecraft 1.2, whose log is server.log.
func (f *serverFixture) legacyServer(t *testing.T, name string) string {
	t.Helper()
	f.mustRun(t, "server", "create", name)
	props := filepath.Join(f.root, name, "server.properties")
	data, err := os.ReadFile(props)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(props, append(data, "\nmsm-version=minecraft/1.2.0\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(f.root, name)
}

func gunzipFile(t *testing.T, path string) string {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(zr)
	return string(b)
}

// CT-CMD-034: msm <server> logroll archives a stopped server's log; "all"
// rolls every server; a running server is skipped untouched.
func TestLogrollCommand(t *testing.T) {
	f := newServerFixture(t)
	one := f.legacyServer(t, "one")
	two := f.legacyServer(t, "two")
	if err := os.WriteFile(filepath.Join(one, "server.log"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(two, "server.log"), []byte("d\ne\nf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := f.mustRun(t, "one", "logroll"); !strings.Contains(out, "Rolling server logs...") || !strings.Contains(out, "Done.") {
		t.Errorf("output = %q", out)
	}
	m, _ := filepath.Glob(filepath.Join(f.archives(), "logs", "one", "one-*.log.gz"))
	if len(m) != 1 || gunzipFile(t, m[0]) != "a\nb\nc\n" {
		t.Fatalf("archives = %v", m)
	}
	if got, _ := os.ReadFile(filepath.Join(one, "server.log")); !strings.HasPrefix(string(got), "Previous logs can be found at") {
		t.Errorf("log = %q", got)
	}
	f.mustRun(t, "all", "logroll")
	if m, _ := filepath.Glob(filepath.Join(f.archives(), "logs", "two", "two-*.log.gz")); len(m) != 1 {
		t.Errorf("all did not roll server two: %v", m)
	}
	// A one-line log (the note) is left alone.
	if out := f.mustRun(t, "one", "logroll"); !strings.Contains(out, "No new log entries to roll") {
		t.Errorf("rolled twice: %q", out)
	}
}

func TestLogrollRefusals(t *testing.T) {
	f := newServerFixture(t)
	one := f.legacyServer(t, "one")
	content := "a\nb\nc\n"
	if err := os.WriteFile(filepath.Join(one, "server.log"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := f.run(t, "", "ghost", "logroll"); code != ExitNotFound || !strings.Contains(errOut, `there is no server with the name "ghost"`) {
		t.Errorf("ghost = %d, %q", code, errOut)
	}
	if code, _, errOut := f.run(t, "", "one", "logroll", "extra"); code != ExitUsage || !strings.Contains(errOut, "No such command") {
		t.Errorf("extra argument = %d, %q", code, errOut)
	}
	// No way to reach its console, and not provably stopped: refused.
	f.states["one"] = servers.Running
	if code, _, errOut := f.run(t, "", "one", "logroll"); code != ExitRefused || !strings.Contains(errOut, "running") {
		t.Errorf("running server = %d, %q", code, errOut)
	}
	if got, _ := os.ReadFile(filepath.Join(one, "server.log")); string(got) != content {
		t.Errorf("the log of a server that could not be proven stopped was changed to %q", got)
	}
}

func TestMaintenancePrune(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "one")
	dir := filepath.Join(f.archives(), "backups", "one")
	old := filepath.Join(dir, "2025-01-01-00-00-00.zip")
	newest := filepath.Join(dir, "2025-01-02-00-00-00.zip")
	f.writeZip(t, old, 400)
	f.writeZip(t, newest, 399)
	if out := f.mustRun(t, "maintenance", "prune", "--dry-run"); !strings.Contains(out, "would remove "+old) || strings.Contains(out, newest) {
		t.Errorf("dry run output:\n%s", out)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("a dry run removed a file")
	}
	if out := f.mustRun(t, "maintenance", "prune"); !strings.Contains(out, "removed "+old) {
		t.Errorf("output:\n%s", out)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("the old backup was not removed")
	}
	if _, err := os.Stat(newest); err != nil {
		t.Error("the newest backup was removed")
	}
}

func TestMaintenancePruneOptions(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "one")
	for _, c := range []struct{ args, want string }{
		{"--days=0", "--days"},
		{"--keep=0", "newest recoverable backup"},
		{"--only=nonsense", "unknown category"},
	} {
		code, _, errOut := f.run(t, "", "maintenance", "prune", c.args)
		if code != ExitUsage || !strings.Contains(errOut, c.want) {
			t.Errorf("prune %s = %d, %q", c.args, code, errOut)
		}
	}
	if code, _, errOut := f.run(t, "", "maintenance", "prune", "extra"); code != ExitUsage {
		t.Errorf("an argument was accepted: %d, %q", code, errOut)
	}
}

func TestMaintenanceRun(t *testing.T) {
	f := newServerFixture(t)
	one := f.legacyServer(t, "one")
	if err := os.WriteFile(filepath.Join(one, "server.log"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(one, "worldstorage", "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(one, "worldstorage", "main", "level.dat"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, job := range []string{"logroll", "backup", "todisk", "prune"} {
		code, _, errOut := f.run(t, "", "maintenance", "run", job)
		if code != ExitOK {
			t.Fatalf("run %s = %d, %q", job, code, errOut)
		}
		if !strings.Contains(errOut, "maintenance "+job+": euid=") {
			t.Errorf("run %s did not record how it started: %q", job, errOut)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(f.archives(), "logs", "one", "*.log.gz")); len(m) != 1 {
		t.Errorf("logroll job archives = %v", m)
	}
	if m, _ := filepath.Glob(filepath.Join(f.archives(), "worlds", "one", "main", "*.zip")); len(m) != 1 {
		t.Errorf("backup job archives = %v", m)
	}
}

func TestMaintenanceRunRefusals(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "one")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"maintenance", "run"}, "accepts 1 arg"},
		{[]string{"maintenance", "run", "cleanup"}, "there is no maintenance job"},
		{[]string{"maintenance", "run", "backup", "--job-timeout=-1s"}, "must not be negative"},
		{[]string{"maintenance", "run", "backup", "--jobs=0"}, "--jobs"},
	} {
		code, _, errOut := f.run(t, "", c.args...)
		if code != ExitUsage || !strings.Contains(errOut, c.want) {
			t.Errorf("%v = %d, %q; want %q", c.args, code, errOut, c.want)
		}
	}
}

// A job that overlaps its previous run is skipped successfully.
func TestMaintenanceRunSkipsWhenTheJobIsRunning(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "one")
	sm, err := f.run2servers(t)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := sm.TryLockJob("prune")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	code, out, errOut := f.run(t, "", "maintenance", "run", "prune")
	if code != ExitOK || !strings.Contains(out, "skipped, the previous run is still going") {
		t.Errorf("= %d, %q, %q", code, out, errOut)
	}
}

func (f *serverFixture) run2servers(t *testing.T) (*servers.Manager, error) {
	t.Helper()
	cfg, _, err := hostConfig(io.Discard)
	if err != nil {
		return nil, err
	}
	cfg.Prober = f.states
	return servers.New(cfg)
}

// "maintenance" is a command word, so a server cannot take the name.
func TestMaintenanceIsAReservedName(t *testing.T) {
	f := newServerFixture(t)
	code, _, errOut := f.run(t, "", "server", "create", "maintenance")
	if code != ExitUsage || !strings.Contains(errOut, "reserved") {
		t.Errorf("= %d, %q", code, errOut)
	}
}
