package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/process"
	"github.com/thehatchcloud/msm/internal/profiles"
)

const (
	rsyncConf = "RSYNC_BACKUP_ENABLED=true\nWORLD_RSYNC_PATH=\"@BASE@/rsync/worlds\"\n"
	rdiffConf = "RDIFF_BACKUP_ENABLED=true\nWORLD_RDIFF_PATH=\"@BASE@/rdiff/worlds\"\nRDIFF_BACKUP_ROTATION=\"3\"\n"
)

// rdiffRunner stands in for rdiff-backup. It records every run, answers
// --version, and lets a test act at a run.
type rdiffRunner struct {
	mu      sync.Mutex
	version string
	calls   []process.Command
	// run is called for every other invocation, with nice (if any) removed.
	run func(args []string, c process.Command) (process.Result, error)
}

func (r *rdiffRunner) Run(_ context.Context, c process.Command) (process.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	args := c.Args
	if filepath.Base(c.Path) == "nice" {
		args = args[3:] // -n N rdiff-backup
	}
	if len(args) == 1 && args[0] == "--version" {
		return process.Result{Stdout: "rdiff-backup " + r.version + "\n"}, nil
	}
	if r.run != nil {
		return r.run(args, c)
	}
	return process.Result{}, nil
}

// invocations are the runs that were not --version, as "nice N" prefixes and
// the arguments.
func (r *rdiffRunner) invocations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		args := c.Args
		prefix := ""
		if filepath.Base(c.Path) == "nice" {
			prefix, args = "nice "+args[1]+" ", args[3:]
		}
		if len(args) == 1 && args[0] == "--version" {
			continue
		}
		out = append(out, prefix+strings.Join(args, " "))
	}
	return out
}

func lookPaths(found ...string) func(string) (string, error) {
	return func(name string) (string, error) {
		for _, f := range found {
			if f == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
	}
}

func (e *env) withRdiff(t *testing.T, version string) *rdiffRunner {
	t.Helper()
	r := &rdiffRunner{version: version}
	e.rebuild(t, func(c *Config) {
		c.Runner, c.LookPath, c.EUID = r, lookPaths("rdiff-backup", "nice"), func() int { return 1000 }
	})
	return r
}

func (e *env) rsyncDir() string { return filepath.Join(e.base, "rsync", "worlds", "srv", "alpha") }
func (e *env) rdiffDir() string { return filepath.Join(e.base, "rdiff", "worlds", "srv", "alpha") }

// Hard-link snapshots are made without rsync, without a zip, and with
// the latest link; the next one shares what did not change.
func TestRsyncSnapshotsReplaceTheTool(t *testing.T) {
	e := newEnv(t, "WORLD_ARCHIVE_ENABLED=false\n"+rsyncConf)
	dir := e.create(t, "srv")
	w := world(t, filepath.Join(dir, "worldstorage"), "alpha")
	e.rebuild(t, func(c *Config) {
		c.Runner = &rdiffRunner{}
		c.LookPath = lookPaths() // no program is installed at all
	})
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("Worlds: %v\n%s", err, e.errw.String())
	}
	first := filepath.Join(e.rsyncDir(), stamp)
	if got := readFile(t, filepath.Join(first, "alpha", "level.dat")); got != "level alpha" {
		t.Errorf("level.dat = %q", got)
	}
	if target, _ := os.Readlink(filepath.Join(e.rsyncDir(), "latest")); target != stamp {
		t.Errorf("latest -> %q", target)
	}
	if exists(filepath.Join(e.archives, "worlds", "srv")) {
		t.Errorf("a ZIP directory was made although ZIP backups are off")
	}
	if _, err := os.Stat(pendingPath(dir)); err == nil {
		t.Errorf("marker left")
	}
	if !strings.Contains(e.out.String(), "4 copied") {
		t.Errorf("output: %s", e.out.String())
	}

	write(t, filepath.Join(w, "level.dat"), "level CHANGED", 0o644)
	e.clk.Advance(time.Hour)
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(e.rsyncDir(), "2026-01-02-04-04-05")
	_, i1 := ino(t, filepath.Join(first, "alpha", "region", "r.0.0.mca"))
	_, i2 := ino(t, filepath.Join(second, "alpha", "region", "r.0.0.mca"))
	if i1 != i2 {
		t.Errorf("an unchanged file is not shared between snapshots")
	}
	if readFile(t, filepath.Join(first, "alpha", "level.dat")) != "level alpha" || readFile(t, filepath.Join(second, "alpha", "level.dat")) != "level CHANGED" {
		t.Errorf("snapshots hold the wrong content")
	}
	if !strings.Contains(e.out.String(), "3 files linked") {
		t.Errorf("output: %s", e.out.String())
	}
}

// Each enabled method is made, for every world, in the legacy order, and a
// missing rdiff-backup is not needed when rdiff is off.
func TestAllMethodsTogether(t *testing.T) {
	e := newEnv(t, rsyncConf+rdiffConf)
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	r := e.withRdiff(t, "2.2.6")
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("Worlds: %v\n%s", err, e.errw.String())
	}
	if got := zipContent(t, filepath.Join(e.archives, "worlds", "srv", "alpha", stamp+".zip"), "alpha/level.dat"); got != "level alpha" {
		t.Errorf("zip level.dat = %q", got)
	}
	if got := readFile(t, filepath.Join(e.rsyncDir(), stamp, "alpha", "level.dat")); got != "level alpha" {
		t.Errorf("snapshot level.dat = %q", got)
	}
	src := filepath.Join(dir, "worldstorage", "alpha")
	want := []string{
		"nice 19 backup " + src + " " + e.rdiffDir(),
		"nice 19 --force remove increments --older-than 3D " + e.rdiffDir(),
	}
	if got := r.invocations(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rdiff-backup runs\n got %q\nwant %q", got, want)
	}
	zi, ri, si := strings.Index(e.out.String(), "Compressing"), strings.Index(e.out.String(), "rdiff-backup alpha"), strings.Index(e.out.String(), "hard-link snapshot")
	if !(zi >= 0 && zi < ri && ri < si) {
		t.Errorf("methods ran in the wrong order:\n%s", e.out.String())
	}
	for _, c := range r.calls[1:] { // the first is --version
		if c.Dir != e.rdiffDir() {
			t.Errorf("ran in %q", c.Dir)
		}
	}
}

// ZIP backups need neither rsync nor rdiff-backup, even when the program
// is not installed and the manager is configured to run without them.
func TestZipNeedsNoOptionalTool(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	e.rebuild(t, func(c *Config) {
		c.Runner = process.Runner(failRunner{})
		c.LookPath = func(string) (string, error) { t.Errorf("looked for a program"); return "", errors.New("none") }
	})
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("Worlds: %v", err)
	}
}

type failRunner struct{}

func (failRunner) Run(context.Context, process.Command) (process.Result, error) {
	return process.Result{}, errors.New("a program was run")
}

// Only the enabled mode fails when its program is missing, and it fails
// before the server is touched, saying what to do.
func TestMissingRdiffFailsOnlyThatMode(t *testing.T) {
	e := newEnv(t, rdiffConf)
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	e.rebuild(t, func(c *Config) { c.Runner, c.LookPath = failRunner{}, lookPaths() })
	e.con.running = true
	err := e.bm.Worlds(context.Background(), "srv", e.output())
	if !errors.Is(err, ErrToolMissing) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"RDIFF_BACKUP_ENABLED", "install", "RDIFF_BACKUP_ENABLED=false"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if cmds := e.con.commands(); len(cmds) != 0 {
		t.Errorf("the server was touched: %v", cmds)
	}
	if exists(pendingPath(dir)) || len(archives(t, filepath.Join(e.archives, "worlds", "srv", "alpha"))) != 0 {
		t.Errorf("something was made")
	}
}

// The adapter drives each rdiff-backup generation with the command line
// that generation has, and refuses what it cannot drive.
func TestRdiffVersions(t *testing.T) {
	for _, tc := range []struct {
		version string
		backup  string // the first run, after src
		retain  string
		err     error
	}{
		{"1.2.8", "SRC REPO", "--remove-older-than 3D --force REPO", nil},
		{"2.0.5", "SRC REPO", "--remove-older-than 3D --force REPO", nil},
		{"2.2.6", "backup SRC REPO", "--force remove increments --older-than 3D REPO", nil},
		{"3.0.1", "backup SRC REPO", "--force remove increments --older-than 3D REPO", nil},
		{"0.13.4", "", "", ErrToolVersion},
		{"2", "", "", ErrToolVersion},
	} {
		t.Run(tc.version, func(t *testing.T) {
			e := newEnv(t, rdiffConf+"WORLD_ARCHIVE_ENABLED=false\n")
			dir := e.create(t, "srv")
			world(t, filepath.Join(dir, "worldstorage"), "alpha")
			r := e.withRdiff(t, tc.version)
			err := e.bm.Worlds(context.Background(), "srv", e.output())
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			sub := strings.NewReplacer("SRC", filepath.Join(dir, "worldstorage", "alpha"), "REPO", e.rdiffDir())
			want := []string{"nice 19 " + sub.Replace(tc.backup), "nice 19 " + sub.Replace(tc.retain)}
			if got := r.invocations(); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("runs\n got %q\nwant %q", got, want)
			}
		})
	}
	t.Run("version unreadable", func(t *testing.T) {
		e := newEnv(t, rdiffConf)
		dir := e.create(t, "srv")
		world(t, filepath.Join(dir, "worldstorage"), "alpha")
		e.rebuild(t, func(c *Config) { c.Runner, c.LookPath = failRunner{}, lookPaths("rdiff-backup", "nice") })
		if err := e.bm.Worlds(context.Background(), "srv", e.output()); !errors.Is(err, ErrToolVersion) {
			t.Errorf("err = %v", err)
		}
	})
}

// RDIFF_BACKUP_NICE sets the priority the program runs at, directly through
// nice; 0 runs it as is, and a priority only root can set is refused.
func TestRdiffPriority(t *testing.T) {
	cases := []struct {
		nice, euid string
		prefix     string
		err        error
	}{
		{"19", "1000", "nice 19 ", nil},
		{"5", "1000", "nice 5 ", nil},
		{"0", "1000", "", nil},
		{"-5", "0", "nice -5 ", nil},
		{"-5", "1000", "", ErrSettings},
		{"20", "0", "", ErrSettings},
		{"low", "0", "", ErrSettings},
	}
	for _, tc := range cases {
		t.Run(tc.nice+"/"+tc.euid, func(t *testing.T) {
			e := newEnv(t, rdiffConf+"WORLD_ARCHIVE_ENABLED=false\nRDIFF_BACKUP_NICE=\""+tc.nice+"\"\n")
			dir := e.create(t, "srv")
			world(t, filepath.Join(dir, "worldstorage"), "alpha")
			r := e.withRdiff(t, "2.2.6")
			uid := 0
			if tc.euid != "0" {
				uid = 1000
			}
			e.bm.cfg.EUID = func() int { return uid }
			err := e.bm.Worlds(context.Background(), "srv", e.output())
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := r.invocations(); len(got) != 2 || !strings.HasPrefix(got[0], tc.prefix+"backup ") {
				t.Errorf("runs %q, want prefix %q", got, tc.prefix)
			}
		})
	}
	t.Run("nice missing", func(t *testing.T) {
		e := newEnv(t, rdiffConf)
		dir := e.create(t, "srv")
		world(t, filepath.Join(dir, "worldstorage"), "alpha")
		e.withRdiff(t, "2.2.6")
		e.bm.cfg.LookPath = lookPaths("rdiff-backup")
		err := e.bm.Worlds(context.Background(), "srv", e.output())
		if !errors.Is(err, ErrToolMissing) || !strings.Contains(err.Error(), "RDIFF_BACKUP_NICE=0") {
			t.Errorf("err = %v", err)
		}
	})
}

// Settings are validated before anything is touched; an unreadable value is
// an error, not a method silently left off.
func TestMethodSettingsAreValidated(t *testing.T) {
	for name, tc := range map[string]struct {
		conf string
		want error
	}{
		"flag yes":           {"RSYNC_BACKUP_ENABLED=\"yes\"", ErrSettings},
		"archive flag":       {"WORLD_ARCHIVE_ENABLED=\"TRUE\"", ErrSettings},
		"rotation zero":      {rdiffConf + "RDIFF_BACKUP_ROTATION=\"0\"", ErrSettings},
		"rotation text":      {rdiffConf + "RDIFF_BACKUP_ROTATION=\"7d\"", ErrSettings},
		"rotation padded":    {rdiffConf + "RDIFF_BACKUP_ROTATION=\" 7\"", ErrSettings},
		"relative rsync":     {"RSYNC_BACKUP_ENABLED=true\nWORLD_RSYNC_PATH=\"rsync\"", ErrSettings},
		"same directory":     {"RSYNC_BACKUP_ENABLED=true\nWORLD_RSYNC_PATH=\"@BASE@/archives/worlds\"", ErrSettings},
		"nested directories": {"RSYNC_BACKUP_ENABLED=true\nWORLD_RSYNC_PATH=\"@BASE@/archives/worlds/rsync\"", ErrSettings},
		"nothing enabled":    {"WORLD_ARCHIVE_ENABLED=false", ErrDisabled},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, tc.conf)
			dir := e.create(t, "srv")
			world(t, filepath.Join(dir, "worldstorage"), "alpha")
			e.withRdiff(t, "2.2.6")
			e.con.running = true
			if err := e.bm.Worlds(context.Background(), "srv", e.output()); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if len(e.con.commands()) != 0 {
				t.Errorf("the server was touched")
			}
		})
	}
}

// A repository that rdiff-backup would refuse, or cannot read, is refused
// before the server is touched.
func TestRdiffRepositoryChecks(t *testing.T) {
	t.Run("not a repository", func(t *testing.T) {
		e := newEnv(t, rdiffConf)
		dir := e.create(t, "srv")
		world(t, filepath.Join(dir, "worldstorage"), "alpha")
		write(t, filepath.Join(e.rdiffDir(), "stranger.txt"), "mine", 0o644)
		r := e.withRdiff(t, "2.2.6")
		e.con.running = true
		err := e.bm.Worlds(context.Background(), "srv", e.output())
		if !errors.Is(err, ErrRepository) || !strings.Contains(err.Error(), "not an rdiff-backup repository") {
			t.Fatalf("err = %v", err)
		}
		if len(e.con.commands()) != 0 || len(r.invocations()) != 0 {
			t.Errorf("something ran: %v %v", e.con.commands(), r.invocations())
		}
	})
	t.Run("unreadable repository", func(t *testing.T) {
		e := newEnv(t, rdiffConf)
		dir := e.create(t, "srv")
		world(t, filepath.Join(dir, "worldstorage"), "alpha")
		write(t, filepath.Join(e.rdiffDir(), rdiffData, "marker"), "x", 0o644)
		r := e.withRdiff(t, "2.2.6")
		r.run = func(args []string, c process.Command) (process.Result, error) {
			return process.Result{Stderr: "ERROR: bad metadata\n"}, errors.New("exit status 1")
		}
		e.con.running = true
		err := e.bm.Worlds(context.Background(), "srv", e.output())
		if !errors.Is(err, ErrRepository) || !strings.Contains(err.Error(), "bad metadata") {
			t.Fatalf("err = %v", err)
		}
		if got := r.invocations(); len(got) != 1 || !strings.HasPrefix(got[0], "nice 19 list increments ") {
			t.Errorf("runs %q", got)
		}
		if len(e.con.commands()) != 0 {
			t.Errorf("the server was touched")
		}
	})
	t.Run("overlapping the world", func(t *testing.T) {
		e := newEnv(t, rdiffConf)
		dir := e.create(t, "srv")
		world(t, filepath.Join(dir, "worldstorage"), "alpha")
		os.Remove(filepath.Join(e.base, "msm.conf"))
		e.withRdiff(t, "2.2.6")
		if !overlaps(filepath.Join(dir, "worldstorage", "alpha", "backup"), filepath.Join(dir, "worldstorage", "alpha")) {
			t.Errorf("overlaps misses a repository inside a world")
		}
	})
}

// A live backup saves, stages, resumes saving, and only then runs the
// slow methods, from the staged copy: they hold what was saved, with the
// world's modes and times, not what the server wrote afterwards.
func TestLiveRdiffAndRsyncUseTheStagedCopy(t *testing.T) {
	e := newEnv(t, rsyncConf+rdiffConf+"WORLD_ARCHIVE_ENABLED=false\n")
	dir := e.create(t, "srv")
	w := world(t, filepath.Join(dir, "worldstorage"), "alpha")
	at := time.Date(2025, 5, 6, 7, 8, 9, 0, time.UTC)
	os.Chtimes(filepath.Join(w, "level.dat"), at, at)
	r := e.withRdiff(t, "2.2.6")
	e.con.running = true
	e.con.hook = func(_ context.Context, c string) {
		switch c {
		case profiles.SaveAll:
			write(t, filepath.Join(w, "level.dat"), "saved state", 0o644)
			os.Chtimes(filepath.Join(w, "level.dat"), at, at)
		case profiles.SaveOn:
			write(t, filepath.Join(w, "level.dat"), "written after resume", 0o644)
		}
	}
	var staged string
	r.run = func(args []string, c process.Command) (process.Result, error) {
		if args[0] != "backup" {
			return process.Result{}, nil
		}
		src := args[1]
		staged = src
		cmds := e.con.commands()
		if cmds[len(cmds)-2] != profiles.SaveOn && cmds[len(cmds)-1] != profiles.SaveOn {
			t.Errorf("rdiff-backup ran before saving resumed: %v", cmds)
		}
		if src == w || !strings.HasPrefix(src, filepath.Join(e.archives, "..", "rsync")) && !strings.Contains(src, ".msm-backup-staging-") {
			t.Errorf("rdiff-backup read %s, not a staged copy", src)
		}
		if got := readFile(t, filepath.Join(src, "level.dat")); got != "saved state" {
			t.Errorf("staged level.dat = %q", got)
		}
		info, err := os.Lstat(filepath.Join(src, "region", "r.0.0.mca"))
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Errorf("staged mode: %v %v", info, err)
		}
		if info, _ := os.Lstat(filepath.Join(src, "level.dat")); !info.ModTime().Equal(at) {
			t.Errorf("staged mtime = %v", info.ModTime())
		}
		return process.Result{}, nil
	}
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("Worlds: %v\n%s", err, e.errw.String())
	}
	if got := readFile(t, filepath.Join(e.rsyncDir(), stamp, "alpha", "level.dat")); got != "saved state" {
		t.Errorf("snapshot level.dat = %q", got)
	}
	if staged == "" {
		t.Fatal("rdiff-backup did not run")
	}
	if exists(staged) {
		t.Errorf("the staged copy %s was left", staged)
	}
	if exists(pendingPath(dir)) {
		t.Errorf("marker left")
	}
	noLeftovers(t, filepath.Join(e.base, "rsync"))
	noLeftovers(t, filepath.Join(e.base, "rdiff"))
}

// A failing method is an error naming what was already published, saving is
// back on, and the marker is gone; nothing partial is left.
func TestFailingMethodIsReported(t *testing.T) {
	e := newEnv(t, rsyncConf+rdiffConf)
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	r := e.withRdiff(t, "2.2.6")
	r.run = func(args []string, c process.Command) (process.Result, error) {
		if args[0] == "backup" {
			return process.Result{Stderr: "ERROR: disk on fire\n"}, errors.New("exit status 1")
		}
		return process.Result{}, nil
	}
	e.con.running = true
	err := e.bm.Worlds(context.Background(), "srv", e.output())
	if err == nil || !strings.Contains(err.Error(), "disk on fire") || !strings.Contains(err.Error(), stamp+".zip") {
		t.Fatalf("err = %v", err)
	}
	cmds := e.con.commands()
	if !has(cmds, profiles.SaveOn) {
		t.Errorf("saving was not resumed: %v", cmds)
	}
	if exists(pendingPath(dir)) {
		t.Errorf("marker left")
	}
	if exists(filepath.Join(e.rsyncDir(), stamp)) {
		t.Errorf("a snapshot was made after the rdiff-backup failed")
	}
	t.Run("retention", func(t *testing.T) {
		r.run = func(args []string, c process.Command) (process.Result, error) {
			if args[0] == "--force" {
				return process.Result{Stderr: "ERROR: cannot remove\n"}, exitStatus(2)
			}
			return process.Result{}, nil
		}
		e.clk.Advance(time.Hour)
		err := e.bm.Worlds(context.Background(), "srv", e.output())
		if err == nil || !strings.Contains(err.Error(), "increment was made") || !strings.Contains(err.Error(), "older than 3 days") {
			t.Errorf("err = %v", err)
		}
	})
}

// A snapshot that was being made when the manager died is removed by the
// next backup, like a partial archive.
func TestInterruptedSnapshotIsSwept(t *testing.T) {
	e := newEnv(t, "WORLD_ARCHIVE_ENABLED=false\n"+rsyncConf)
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	partial := filepath.Join(e.rsyncDir(), tmpPrefix+snapshotTmpKind+"0123456789abcdef"+tmpSuffix)
	write(t, filepath.Join(partial, "alpha", "ro", "f"), "x", 0o444)
	os.Chmod(filepath.Join(partial, "alpha", "ro"), 0o555)
	write(t, pendingPath(dir), `{"version":1,"operation":"worlds","saving_off":false}`, 0o600)
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("Worlds: %v", err)
	}
	if exists(partial) {
		t.Errorf("the partial snapshot was left")
	}
	if !strings.Contains(e.errw.String(), "left by an interrupted backup") {
		t.Errorf("not reported: %s", e.errw.String())
	}
}

// realRdiff returns a runnable rdiff-backup, or skips. Some installations
// ship it for a Python other than the default one, so a wrapper is tried.
func realRdiff(t *testing.T) string {
	t.Helper()
	try := func(path string) bool {
		out, err := exec.Command(path, "--version").Output()
		return err == nil && strings.HasPrefix(string(out), "rdiff-backup")
	}
	if p, err := exec.LookPath("rdiff-backup"); err == nil && try(p) {
		return p
	}
	if p, err := exec.LookPath("rdiff-backup"); err == nil {
		for _, py := range []string{"python3.12", "python3.11", "python3.10"} {
			if pyPath, err := exec.LookPath(py); err == nil {
				wrapper := filepath.Join(t.TempDir(), "rdiff-backup")
				script := "#!/bin/sh\nexec " + pyPath + " " + p + " \"$@\"\n"
				if os.WriteFile(wrapper, []byte(script), 0o755) == nil && try(wrapper) {
					return wrapper
				}
			}
		}
	}
	t.Skip("no working rdiff-backup is installed")
	return ""
}

// With the real program: a world is backed up, a second backup adds an
// increment, and a repository made by the legacy command line is read and
// extended.
func TestRealRdiffBackup(t *testing.T) {
	tool := realRdiff(t)
	e := newEnv(t, rdiffConf+"WORLD_ARCHIVE_ENABLED=false\nRDIFF_BACKUP_NICE=\"10\"\n")
	dir := e.create(t, "srv")
	w := world(t, filepath.Join(dir, "worldstorage"), "alpha")
	e.rebuild(t, func(c *Config) {
		c.LookPath = func(n string) (string, error) {
			if n == "rdiff-backup" {
				return tool, nil
			}
			return exec.LookPath(n)
		}
	})

	// An existing repository, made the way the legacy manager made it.
	os.MkdirAll(e.rdiffDir(), 0o755)
	if out, err := exec.Command(tool, w, e.rdiffDir()).CombinedOutput(); err != nil {
		t.Fatalf("legacy backup: %v\n%s", err, out)
	}
	// rdiff-backup refuses a backup in the same second as the last one.
	time.Sleep(1200 * time.Millisecond)
	write(t, filepath.Join(w, "level.dat"), "level two", 0o644)
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("Worlds: %v\n%s", err, e.errw.String())
	}
	if got := readFile(t, filepath.Join(e.rdiffDir(), "level.dat")); got != "level two" {
		t.Errorf("repository mirror level.dat = %q", got)
	}
	out, err := exec.Command(tool, "list", "increments", e.rdiffDir()).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Found 1 increments") {
		t.Errorf("increments: %v\n%s", err, out)
	}
	if out, err := exec.Command(tool, "verify", e.rdiffDir()).CombinedOutput(); err != nil {
		t.Errorf("verify: %v\n%s", err, out)
	}

	// Live: from the staged copy.
	e.con.running = true
	e.clk.Advance(time.Hour)
	time.Sleep(1200 * time.Millisecond)
	write(t, filepath.Join(w, "level.dat"), "level three", 0o644)
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("live Worlds: %v\n%s", err, e.errw.String())
	}
	if got := readFile(t, filepath.Join(e.rdiffDir(), "level.dat")); got != "level three" {
		t.Errorf("repository mirror level.dat = %q", got)
	}
}

// exitStatus is the error a program that exited with code produces.
func exitStatus(code int) error {
	return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
}

// rdiff-backup exits with status 2 when no increment is old enough to
// remove, which is not a failure; the same status with another message is.
func TestNothingToRemoveIsNotAFailure(t *testing.T) {
	e := newEnv(t, rdiffConf+"WORLD_ARCHIVE_ENABLED=false\n")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	r := e.withRdiff(t, "2.2.6")
	stderr := "WARNING: No increment is older than '3D'\nWARNING: Action remove emitted warnings, see previous messages for details\n"
	r.run = func(args []string, c process.Command) (process.Result, error) {
		if args[0] == "--force" {
			return process.Result{Stderr: stderr}, exitStatus(2)
		}
		return process.Result{}, nil
	}
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatalf("a young repository failed the backup: %v", err)
	}
	stderr = "ERROR: something else\n"
	e.clk.Advance(time.Hour)
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err == nil {
		t.Errorf("status 2 with another message was accepted")
	}
}
