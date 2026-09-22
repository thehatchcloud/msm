package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The test executable acts as the child, avoiding Java, screen or shell.
func TestChildProcess(t *testing.T) {
	if os.Getenv("MSM_TEST_CHILD") != "1" {
		return
	}
	at := slices.Index(os.Args, "--")
	if at == -1 {
		os.Exit(2)
	}
	cwd, _ := os.Getwd()
	fmt.Println(cwd)
	fmt.Println(os.Args[at+1:])
	fmt.Fprintln(os.Stderr, "fixture stderr")
	os.Exit(7)
}

func TestExecRunner(t *testing.T) {
	dir := t.TempDir()
	// macOS /var is commonly a symlink, while os.Getwd may canonicalize it.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (ExecRunner{}).Run(context.Background(), Command{
		Path: os.Args[0], Args: []string{"-test.run=^TestChildProcess$", "--", "$(not-evaluated)", "two words"},
		Dir: dir, Env: append(os.Environ(), "MSM_TEST_CHILD=1"),
	})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("expected child exit 7, got %v", err)
	}
	if result.Stderr != "fixture stderr\n" {
		t.Fatalf("stderr=%q", result.Stderr)
	}
	if !strings.Contains(result.Stdout, "[$(not-evaluated) two words]\n") {
		t.Fatalf("arguments were not preserved: %q", result.Stdout)
	}
	if !strings.HasPrefix(result.Stdout, resolved+"\n") {
		t.Fatalf("working directory was not preserved: %q", result.Stdout)
	}
}

func TestCanceledRunner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (ExecRunner{}).Run(ctx, Command{Path: os.Args[0]})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
