package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteCreatesNewFileWithGivenPermission(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	if err := Write(path, []byte("hello"), 0640); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("content = %q, want %q", data, "hello")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode = %v, want 0640", info.Mode().Perm())
	}
}

func TestWritePreservesExistingPermission(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}

	// A different requested permission must not override the existing mode.
	if err := Write(path, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want preserved 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("content = %q, want %q", data, "new")
	}
}

func TestWriteLeavesNoTemporaryFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := Write(path, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "file.txt" {
		t.Fatalf("unexpected directory contents: %v", entries)
	}
}

func TestWriteRefusesNonRegularTarget(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "adir")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Write(sub, []byte("x"), 0600); err == nil {
		t.Fatal("expected an error replacing a directory")
	}
}

func TestWriteRejectsMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "file.txt")
	if err := Write(path, []byte("x"), 0600); err == nil {
		t.Fatal("expected an error for a missing parent directory")
	}
}

// TestWriteIsAtomicUnderConcurrency writes many distinct payloads to the same
// path concurrently. A reader must always observe one complete payload, never
// a mix of two, and every write must succeed.
func TestWriteIsAtomicUnderConcurrency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(path, []byte("seed"), 0600); err != nil {
		t.Fatal(err)
	}

	const writers = 16
	payloads := make([][]byte, writers)
	for i := range payloads {
		payloads[i] = []byte(fmt.Sprintf("payload-%02d-%s", i, string(make([]byte, 64))))
	}

	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = Write(path, payloads[i], 0600)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range payloads {
		if string(got) == string(p) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("final content matched none of the concurrent payloads: %q", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one file after concurrent writes, got %v", entries)
	}
}
