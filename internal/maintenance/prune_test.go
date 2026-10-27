package maintenance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func prune(t *testing.T, e *env, o PruneOptions) error {
	t.Helper()
	if o.Days == 0 {
		o.Days = DefaultDays
	}
	if o.Keep == 0 {
		o.Keep = 1
	}
	return e.pruner.Prune(t.Context(), o, e.output())
}

func TestPruneKeepsRecentAndRemovesOld(t *testing.T) {
	e := newEnv(t, "")
	e.server(t, "alpha")
	dir := filepath.Join(e.backups, "alpha")
	old1 := filepath.Join(dir, "2026-01-01-00-00-00.zip")
	old2 := filepath.Join(dir, "2026-01-10-00-00-00.zip")
	recent := filepath.Join(dir, "2026-02-28-00-00-00.zip")
	zipFile(t, old1, 59)
	zipFile(t, old2, 50)
	zipFile(t, recent, 1)
	if err := prune(t, e, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if exists(old1) || exists(old2) || !exists(recent) {
		t.Errorf("old1=%v old2=%v recent=%v, want the two old ones removed and the recent one kept", exists(old1), exists(old2), exists(recent))
	}
	if !strings.Contains(e.out.String(), "Removed 2 files") {
		t.Errorf("summary missing:\n%s", e.out.String())
	}
}

// find -mtime +30 removes a file only once it is 31 whole days old.
func TestPruneAgeBoundaryMatchesFindMtime(t *testing.T) {
	e := newEnv(t, "")
	e.server(t, "alpha")
	dir := filepath.Join(e.backups, "alpha")
	newest := filepath.Join(dir, "2026-02-28-00-00-00.zip")
	at30 := filepath.Join(dir, "2026-01-30-00-00-00.zip")
	at31 := filepath.Join(dir, "2026-01-29-00-00-00.zip")
	zipFile(t, newest, 1)
	zipFile(t, at30, 30.9)
	zipFile(t, at31, 31)
	if err := prune(t, e, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if !exists(at30) || exists(at31) {
		t.Errorf("30.9 days old kept=%v, 31 days old kept=%v; want true, false", exists(at30), exists(at31))
	}
}

func TestPruneNeverRemovesTheNewestRecoverableBackup(t *testing.T) {
	e := newEnv(t, "")
	e.server(t, "alpha")
	dir := filepath.Join(e.backups, "alpha")
	older := filepath.Join(dir, "2026-01-01-00-00-00.zip")
	newest := filepath.Join(dir, "2026-01-02-00-00-00.zip")
	zipFile(t, older, 58)
	zipFile(t, newest, 57)
	if err := prune(t, e, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if exists(older) || !exists(newest) {
		t.Errorf("older kept=%v newest kept=%v; want false, true", exists(older), exists(newest))
	}
}

func TestPruneKeepCount(t *testing.T) {
	e := newEnv(t, "")
	e.server(t, "alpha")
	dir := filepath.Join(e.backups, "alpha")
	var files []string
	for i, d := range []string{"01", "02", "03", "04"} {
		f := filepath.Join(dir, "2026-01-"+d+"-00-00-00.zip")
		zipFile(t, f, float64(58-i))
		files = append(files, f)
	}
	if err := prune(t, e, PruneOptions{Keep: 3}); err != nil {
		t.Fatal(err)
	}
	if exists(files[0]) || !exists(files[1]) || !exists(files[2]) || !exists(files[3]) {
		t.Errorf("kept: %v %v %v %v; want only the oldest removed", exists(files[0]), exists(files[1]), exists(files[2]), exists(files[3]))
	}
}

func TestPruneUnrecoverableNewestIsNotKept(t *testing.T) {
	e := newEnv(t, "")
	e.server(t, "alpha")
	dir := filepath.Join(e.backups, "alpha")
	good := filepath.Join(dir, "2026-01-01-00-00-00.zip")
	damaged := filepath.Join(dir, "2026-01-02-00-00-00.zip")
	zipFile(t, good, 58)
	write(t, damaged, "PK truncated")
	aged(t, damaged, 57)
	if err := prune(t, e, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if !exists(good) {
		t.Error("the newest archive that verifies was removed")
	}
	if exists(damaged) {
		t.Error("an old damaged archive newer than the good one was kept as if it were the recovery point")
	}
	if !strings.Contains(e.errw.String(), "not recoverable") {
		t.Errorf("no warning about the damaged archive:\n%s", e.errw.String())
	}
}

func TestPruneRemovesNothingWhenNoArchiveVerifies(t *testing.T) {
	e := newEnv(t, "")
	e.server(t, "alpha")
	dir := filepath.Join(e.backups, "alpha")
	a := filepath.Join(dir, "2026-01-01-00-00-00.zip")
	b := filepath.Join(dir, "2026-01-02-00-00-00.zip")
	for _, f := range []string{a, b} {
		write(t, f, "not a zip")
		aged(t, f, 58)
	}
	if err := prune(t, e, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if !exists(a) || !exists(b) {
		t.Error("archives were removed although none could be recovered")
	}
	if !strings.Contains(e.errw.String(), "none of them is removed") {
		t.Errorf("no warning:\n%s", e.errw.String())
	}
}

func TestPruneDryRunRemovesNothing(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	old := filepath.Join(e.backups, "alpha", "2026-01-01-00-00-00.zip")
	newest := filepath.Join(e.backups, "alpha", "2026-01-02-00-00-00.zip")
	zipFile(t, old, 58)
	zipFile(t, newest, 57)
	log := filepath.Join(dir, "logs", "2025-12-01-1.log.gz")
	write(t, log, "x")
	aged(t, log, 90)
	if err := prune(t, e, PruneOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !exists(old) || !exists(newest) || !exists(log) {
		t.Fatal("a dry run removed a file")
	}
	for _, want := range []string{"would remove " + old, "would remove " + log, "Would remove 2 files"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, e.out.String())
		}
	}
}

func TestPruneLogCategories(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	serverOld := filepath.Join(dir, "logs", "2025-12-01-1.log.gz")
	serverNew := filepath.Join(dir, "logs", "2026-02-28-1.log.gz")
	serverPlain := filepath.Join(dir, "logs", "latest.log")
	nested := filepath.Join(dir, "logs", "deeper", "old.log.gz")
	archOld := filepath.Join(e.logs, "alpha", "alpha-2025-12-01-00-00-00.log.gz")
	archNew := filepath.Join(e.logs, "alpha", "alpha-2026-02-28-00-00-00.log.gz")
	archOther := filepath.Join(e.logs, "alpha", "notes.txt")
	for _, f := range []string{serverOld, serverPlain, nested, archOld, archOther} {
		write(t, f, "x")
		aged(t, f, 90)
	}
	for _, f := range []string{serverNew, archNew} {
		write(t, f, "x")
		aged(t, f, 2)
	}
	if err := prune(t, e, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]bool{serverOld: false, serverNew: true, serverPlain: true, nested: true, archOld: false, archNew: true, archOther: true} {
		if exists(f) != want {
			t.Errorf("%s exists=%v, want %v", f, exists(f), want)
		}
	}
}

func TestPruneWorldBackupsKeepNewestPerWorld(t *testing.T) {
	e := newEnv(t, "")
	e.server(t, "alpha")
	w1old := filepath.Join(e.worlds, "alpha", "world", "2026-01-01-00-00-00.zip")
	w1new := filepath.Join(e.worlds, "alpha", "world", "2026-01-02-00-00-00.zip")
	w2only := filepath.Join(e.worlds, "alpha", "nether", "2026-01-01-00-00-00.zip")
	zipFile(t, w1old, 58)
	zipFile(t, w1new, 57)
	zipFile(t, w2only, 58)
	if err := prune(t, e, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if exists(w1old) || !exists(w1new) || !exists(w2only) {
		t.Errorf("w1old=%v w1new=%v w2only=%v; want false, true, true", exists(w1old), exists(w1new), exists(w2only))
	}
}

func TestPruneOrphanArchivesKeepTheirNewest(t *testing.T) {
	e := newEnv(t, "")
	gone1 := filepath.Join(e.backups, "deleted", "2026-01-01-00-00-00.zip")
	gone2 := filepath.Join(e.backups, "deleted", "2026-01-02-00-00-00.zip")
	zipFile(t, gone1, 58)
	zipFile(t, gone2, 57)
	if err := prune(t, e, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if exists(gone1) || !exists(gone2) {
		t.Errorf("gone1=%v gone2=%v; want false, true", exists(gone1), exists(gone2))
	}
}

func TestPruneLeavesWhatIsNotAnMSMArchive(t *testing.T) {
	e := newEnv(t, "")
	e.server(t, "alpha")
	dir := filepath.Join(e.backups, "alpha")
	zipFile(t, filepath.Join(dir, "2026-02-28-00-00-00.zip"), 1)
	handmade := filepath.Join(dir, "before-upgrade.zip")
	zipFile(t, handmade, 90)
	outside := filepath.Join(e.base, "outside.zip")
	zipFile(t, outside, 90)
	link := filepath.Join(dir, "2026-01-01-00-00-00.zip")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "2026-01-02-00-00-00.zip")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prune(t, e, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{handmade, outside, link, subdir} {
		if !exists(f) {
			t.Errorf("%s was removed", f)
		}
	}
}

func TestPruneDoesNotFollowADirectoryLink(t *testing.T) {
	e := newEnv(t, "")
	e.server(t, "alpha")
	elsewhere := filepath.Join(e.base, "elsewhere")
	victim := filepath.Join(elsewhere, "2026-01-01-00-00-00.zip")
	zipFile(t, victim, 90)
	zipFile(t, filepath.Join(elsewhere, "2026-01-02-00-00-00.zip"), 89)
	if err := os.MkdirAll(e.backups, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(e.backups, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := prune(t, e, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if !exists(victim) {
		t.Error("a file behind a linked server directory was removed")
	}
}

func TestPruneOnlyCategories(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	log := filepath.Join(dir, "logs", "2025-12-01-1.log.gz")
	write(t, log, "x")
	aged(t, log, 90)
	arch := filepath.Join(e.logs, "alpha", "alpha-2025-12-01-00-00-00.log.gz")
	write(t, arch, "x")
	aged(t, arch, 90)
	if err := prune(t, e, PruneOptions{Only: []Category{LogArchives}}); err != nil {
		t.Fatal(err)
	}
	if !exists(log) || exists(arch) {
		t.Errorf("server log kept=%v archive kept=%v; want true, false", exists(log), exists(arch))
	}
}

func TestPruneRejectsBadOptions(t *testing.T) {
	e := newEnv(t, "")
	for name, o := range map[string]PruneOptions{
		"days zero":     {Days: 0, Keep: 1},
		"days negative": {Days: -3, Keep: 1},
		"keep zero":     {Days: 30, Keep: 0},
		"bad category":  {Days: 30, Keep: 1, Only: []Category{"everything"}},
	} {
		if err := e.pruner.Prune(t.Context(), o, e.output()); !errors.Is(err, ErrOptions) {
			t.Errorf("%s: err = %v, want ErrOptions", name, err)
		}
	}
	if _, err := ParseCategories("backups,nonsense"); !errors.Is(err, ErrOptions) {
		t.Errorf("ParseCategories error = %v", err)
	}
}

func TestPruneRejectsUnsafeRoots(t *testing.T) {
	cases := map[string]struct {
		conf string
		want string
	}{
		"filesystem root":        {`BACKUP_ARCHIVE_PATH="/"`, "filesystem root"},
		"shallow":                {`BACKUP_ARCHIVE_PATH="/opt"`, "too close"},
		"relative":               {`BACKUP_ARCHIVE_PATH="archives/backups"`, "absolute"},
		"unclean":                {`BACKUP_ARCHIVE_PATH="@BASE@/archives/../archives/backups"`, "not a clean path"},
		"inside the servers":     {`LOG_ARCHIVE_PATH="@BASE@/servers/logs"`, "overlap"},
		"contains the servers":   {`WORLD_ARCHIVE_PATH="@BASE@"`, "overlap"},
		"same as another root":   {`LOG_ARCHIVE_PATH="@BASE@/archives/backups"`, "overlap"},
		"inside another archive": {`WORLD_ARCHIVE_PATH="@BASE@/archives/backups/worlds"`, "overlap"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, c.conf)
			dir := e.server(t, "alpha")
			log := filepath.Join(dir, "logs", "2025-12-01-1.log.gz")
			write(t, log, "x")
			aged(t, log, 90)
			err := prune(t, e, PruneOptions{})
			if !errors.Is(err, ErrUnsafeRoot) {
				t.Fatalf("err = %v, want an unsafe-root refusal", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
			if !exists(log) {
				t.Error("files were removed although a root was refused")
			}
		})
	}
}

func TestPruneRejectsALinkedRoot(t *testing.T) {
	e := newEnv(t, "")
	real := filepath.Join(e.base, "real-backups")
	zipFile(t, filepath.Join(real, "alpha", "2026-01-01-00-00-00.zip"), 90)
	zipFile(t, filepath.Join(real, "alpha", "2026-01-02-00-00-00.zip"), 89)
	if err := os.MkdirAll(filepath.Dir(e.backups), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, e.backups); err != nil {
		t.Fatal(err)
	}
	if err := prune(t, e, PruneOptions{}); !errors.Is(err, ErrUnsafeRoot) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want a symbolic-link refusal", err)
	}
	if !exists(filepath.Join(real, "alpha", "2026-01-01-00-00-00.zip")) {
		t.Error("a file was removed through a linked root")
	}
}

func TestPruneMissingRootsHaveNothingToDo(t *testing.T) {
	e := newEnv(t, "")
	e.server(t, "alpha")
	if err := prune(t, e, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "Removed 0 files") {
		t.Errorf("output:\n%s", e.out.String())
	}
}

func TestPruneReportsAFileItCannotRemoveAndContinues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove from a read-only directory")
	}
	e := newEnv(t, "")
	e.server(t, "alpha")
	locked := filepath.Join(e.logs, "alpha")
	stuck := filepath.Join(locked, "alpha-2025-11-01-00-00-00.log.gz")
	write(t, stuck, "x")
	aged(t, stuck, 100)
	other := filepath.Join(e.backups, "alpha")
	zipFile(t, filepath.Join(other, "2026-01-01-00-00-00.zip"), 59)
	zipFile(t, filepath.Join(other, "2026-01-02-00-00-00.zip"), 58)
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)
	err := prune(t, e, PruneOptions{})
	if !errors.Is(err, ErrPrune) {
		t.Fatalf("err = %v, want ErrPrune", err)
	}
	if exists(filepath.Join(other, "2026-01-01-00-00-00.zip")) {
		t.Error("the failure stopped the other categories")
	}
}
