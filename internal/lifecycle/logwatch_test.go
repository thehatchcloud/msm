package lifecycle

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

func TestLogWatcher(t *testing.T) {
	done := regexp.MustCompile(`^\[.*\] Done`)
	dir := t.TempDir()
	path := filepath.Join(dir, "latest.log")
	write := func(flag int, text string) {
		t.Helper()
		f, err := os.OpenFile(path, flag|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(text)
		f.Close()
	}
	poll := func(w *logWatcher) bool {
		t.Helper()
		ok, err := w.poll(done)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// No log yet, then a log appears.
	w, err := watchLog(path)
	if err != nil || poll(w) {
		t.Fatal("matched before any log", err)
	}
	write(os.O_APPEND, "[1] Done old\n")
	if !poll(w) {
		t.Fatal("a log created after the mark is fresh")
	}
	w.close()

	// Appended: only lines after the mark count, even split across polls.
	w, _ = watchLog(path)
	if poll(w) {
		t.Fatal("old line matched")
	}
	write(os.O_APPEND, "[2] Do")
	if poll(w) {
		t.Fatal("partial line matched")
	}
	write(os.O_APPEND, "ne new\r\n")
	if !poll(w) {
		t.Fatal("completed fresh line not matched")
	}
	w.close()

	// Replaced by rename, as Minecraft 1.7+ does at startup.
	w, _ = watchLog(path)
	next := filepath.Join(dir, "next.log")
	os.WriteFile(next, []byte("[3] Done replaced\n"), 0o644)
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	if !poll(w) {
		t.Fatal("replacement log not read from the start")
	}
	w.close()

	// Truncated and rewritten in place.
	w, _ = watchLog(path)
	write(os.O_TRUNC, "[4] Done rewritten\n")
	if !poll(w) {
		t.Fatal("rewritten log not read from the start")
	}
	w.close()

	// An over-long line is skipped, and the next line still matches;
	// failure hints are kept.
	w, _ = watchLog(path)
	write(os.O_APPEND, strings.Repeat("x", maxLogLine+10)+" Done\n[5] **** FAILED TO BIND TO PORT!\n")
	if poll(w) {
		t.Fatal("over-long line matched")
	}
	write(os.O_APPEND, "[6] Done\n")
	if !poll(w) || len(w.hints) != 1 || !strings.Contains(w.hints[0], "FAILED TO BIND") {
		t.Fatalf("hints %q", w.hints)
	}
	w.close()

	// A FIFO in place of the log is refused, never blocked on.
	os.Remove(path)
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	w, _ = watchLog(path)
	if _, err := w.poll(done); err == nil {
		t.Fatal("FIFO log accepted")
	}
}
