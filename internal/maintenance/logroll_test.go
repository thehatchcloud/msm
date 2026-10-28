package maintenance

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/servers"
)

const archived = "alpha-2026-03-01-12-00-00.log.gz"

func gunzip(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), rollTmpPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestRollArchivesAStoppedServersLog(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	content := "line one\nline two\nline three\n"
	write(t, filepath.Join(dir, "server.log"), content)
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(e.logs, "alpha", archived)
	if got := gunzip(t, archive); got != content {
		t.Errorf("archive holds %q, want %q", got, content)
	}
	if got, want := readFile(t, filepath.Join(dir, "server.log")), "Previous logs can be found at \""+filepath.Join(e.logs, "alpha")+"\"\n"; got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
	if left := leftovers(t, filepath.Join(e.logs, "alpha")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
	info, err := os.Stat(archive)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("archive mode = %v (%v), want 0600", info.Mode().Perm(), err)
	}
}

func TestRollPreservesTheLogsMode(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	log := filepath.Join(dir, "server.log")
	write(t, log, "a\nb\nc\n")
	if err := os.Chmod(log, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(log); info.Mode().Perm() != 0o640 {
		t.Errorf("log mode = %v, want 0640", info.Mode().Perm())
	}
}

// The legacy command wc -l's the log and does nothing for one line or none.
func TestRollLeavesAShortLogAlone(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "the note only": "Previous logs can be found at \"/x\"\n", "no newline": "partial"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, "")
			dir := e.server(t, "alpha")
			write(t, filepath.Join(dir, "server.log"), content)
			if err := e.roller.Roll(t.Context(), "alpha", e.output()); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(dir, "server.log")); got != content {
				t.Errorf("log changed to %q", got)
			}
			if exists(filepath.Join(e.logs, "alpha", archived)) {
				t.Error("an archive was made")
			}
			if left := leftovers(t, filepath.Join(e.logs, "alpha")); len(left) != 0 {
				t.Errorf("temporary files left behind: %v", left)
			}
			if !strings.Contains(e.out.String(), "No new log entries to roll. No change made.") {
				t.Errorf("output:\n%s", e.out.String())
			}
		})
	}
}

func TestRollWithoutALog(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "server.log")) {
		t.Error("a log was created")
	}
	if !strings.Contains(e.out.String(), "No change made") {
		t.Errorf("output:\n%s", e.out.String())
	}
}

// A running server holds its log open; copying and truncating it would lose
// the lines written in between, so it is skipped untouched.
func TestRollSkipsARunningServer(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	content := "a\nb\nc\n"
	write(t, filepath.Join(dir, "server.log"), content)
	e.con.running = true
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); err != nil {
		t.Fatalf("a skipped roll must not fail: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "server.log")); got != content {
		t.Errorf("a running server's log was changed to %q", got)
	}
	if exists(filepath.Join(e.logs, "alpha")) {
		t.Error("an archive directory was made for a skipped roll")
	}
	if !strings.Contains(e.out.String(), "Skipped: the server is running") {
		t.Errorf("output:\n%s", e.out.String())
	}
}

func TestRollRefusesAServerThatCannotBeProvenStopped(t *testing.T) {
	for name, st := range map[string]servers.State{
		"unknown":  {Kind: servers.Unknown},
		"occupied": {Kind: servers.Occupied},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, "")
			dir := e.server(t, "alpha")
			content := "a\nb\nc\n"
			write(t, filepath.Join(dir, "server.log"), content)
			e.prober.states["alpha"] = st
			err := e.roller.Roll(t.Context(), "alpha", e.output())
			if !errors.Is(err, ErrNotProven) {
				t.Fatalf("err = %v, want ErrNotProven", err)
			}
			if readFile(t, filepath.Join(dir, "server.log")) != content {
				t.Error("the log was changed")
			}
		})
	}
}

func TestRollReportsAnUnreachableConsole(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	write(t, filepath.Join(dir, "server.log"), "a\nb\nc\n")
	e.con.err = errors.New("screen is broken")
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); err == nil || !strings.Contains(err.Error(), "screen is broken") {
		t.Fatalf("err = %v", err)
	}
}

func TestRollModernLogPath(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	write(t, filepath.Join(dir, "server.properties"), "msm-version=minecraft/1.7.0\n")
	log := filepath.Join(dir, "logs", "latest.log")
	write(t, log, "[12:00:00] [Server thread/INFO]: a\n[12:00:01] [Server thread/INFO]: b\n")
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); err != nil {
		t.Fatal(err)
	}
	if got := gunzip(t, filepath.Join(e.logs, "alpha", archived)); !strings.Contains(got, "Server thread/INFO]: b") {
		t.Errorf("archive = %q", got)
	}
	if !strings.HasPrefix(readFile(t, log), "Previous logs can be found at") {
		t.Error("latest.log was not replaced")
	}
}

func TestRollRefusesALogOutsideTheServer(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	outside := filepath.Join(e.base, "outside.log")
	write(t, outside, "a\nb\nc\n")
	write(t, filepath.Join(dir, "server.properties"), "msm-log-path="+outside+"\n")
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); !errors.Is(err, ErrLogPath) {
		t.Fatalf("err = %v, want ErrLogPath", err)
	}
	if readFile(t, outside) != "a\nb\nc\n" {
		t.Error("a log outside the server was changed")
	}
}

func TestRollRefusesALogThatIsALink(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	target := filepath.Join(e.base, "target.log")
	write(t, target, "a\nb\nc\n")
	if err := os.Symlink(target, filepath.Join(dir, "server.log")); err != nil {
		t.Fatal(err)
	}
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); !errors.Is(err, ErrLogPath) {
		t.Fatalf("err = %v, want ErrLogPath", err)
	}
	if readFile(t, target) != "a\nb\nc\n" {
		t.Error("the link target was changed")
	}
}

func TestRollRefusesALinkedLogDirectoryEscape(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	write(t, filepath.Join(dir, "server.properties"), "msm-version=minecraft/1.7.0\n")
	elsewhere := filepath.Join(e.base, "elsewhere")
	write(t, filepath.Join(elsewhere, "latest.log"), "a\nb\nc\n")
	if err := os.Symlink(elsewhere, filepath.Join(dir, "logs")); err != nil {
		t.Fatal(err)
	}
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); !errors.Is(err, ErrLogPath) {
		t.Fatalf("err = %v, want ErrLogPath", err)
	}
	if readFile(t, filepath.Join(elsewhere, "latest.log")) != "a\nb\nc\n" {
		t.Error("a log behind a linked directory was changed")
	}
}

// An archive that exists is never replaced, and the log stays as it was, so
// nothing is lost; a rerun in a later second succeeds.
func TestRollNeverReplacesAnArchive(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	content := "a\nb\nc\n"
	write(t, filepath.Join(dir, "server.log"), content)
	existing := filepath.Join(e.logs, "alpha", archived)
	write(t, existing, "precious")
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); err == nil {
		t.Fatal("rolling over an existing archive succeeded")
	}
	if readFile(t, existing) != "precious" {
		t.Error("an existing archive was replaced")
	}
	if readFile(t, filepath.Join(dir, "server.log")) != content {
		t.Error("the log was changed although it was not archived")
	}
	if left := leftovers(t, filepath.Join(e.logs, "alpha")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestRollKeepsTheLogWhenTheArchiveCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	content := "a\nb\nc\n"
	write(t, filepath.Join(dir, "server.log"), content)
	archiveDir := filepath.Join(e.logs, "alpha")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(archiveDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(archiveDir, 0o755)
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); err == nil {
		t.Fatal("rolling into a read-only directory succeeded")
	}
	if readFile(t, filepath.Join(dir, "server.log")) != content {
		t.Error("the log was changed although it was not archived")
	}
}

func TestRollAllRollsEveryServerAndReportsEach(t *testing.T) {
	e := newEnv(t, "")
	write(t, filepath.Join(e.server(t, "alpha"), "server.log"), "a\nb\nc\n")
	write(t, filepath.Join(e.server(t, "bravo"), "server.log"), "d\ne\nf\n")
	err := e.roller.ForAll(t.Context(), e.output(), func(name string, o Output) error {
		return e.roller.Roll(t.Context(), name, o)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := gunzip(t, filepath.Join(e.logs, "bravo", "bravo-2026-03-01-12-00-00.log.gz")); got != "d\ne\nf\n" {
		t.Errorf("bravo archive = %q", got)
	}
	if !strings.Contains(e.out.String(), "alpha: Rolling server logs...") || !strings.Contains(e.out.String(), "bravo: Done.") {
		t.Errorf("output:\n%s", e.out.String())
	}
}

func TestRollLargeLogIsArchivedExactly(t *testing.T) {
	e := newEnv(t, "")
	dir := e.server(t, "alpha")
	var b bytes.Buffer
	for i := 0; i < 50000; i++ {
		b.WriteString("[12:00:00] [Server thread/INFO]: a line of log output that repeats\n")
	}
	b.WriteString("final line without newline")
	write(t, filepath.Join(dir, "server.log"), b.String())
	if err := e.roller.Roll(t.Context(), "alpha", e.output()); err != nil {
		t.Fatal(err)
	}
	if gunzip(t, filepath.Join(e.logs, "alpha", archived)) != b.String() {
		t.Error("the archive does not hold the whole log")
	}
}

func TestRollUnknownServer(t *testing.T) {
	e := newEnv(t, "")
	if err := e.roller.Roll(t.Context(), "nope", e.output()); !errors.Is(err, servers.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
