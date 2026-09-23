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

func TestRunAttachedUsesGivenFiles(t *testing.T) {
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	err = (ExecRunner{}).RunAttached(context.Background(), Command{
		Path: os.Args[0], Args: []string{"-test.run=^TestChildProcess$", "--", "attached"},
		Env: append(os.Environ(), "MSM_TEST_CHILD=1"),
	}, Stdio{In: nil, Out: out, Err: out})
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("expected child exit 7, got %v", err)
	}
	data, _ := os.ReadFile(out.Name())
	if !strings.Contains(string(data), "[attached]") || !strings.Contains(string(data), "fixture stderr") {
		t.Fatalf("output = %q", data)
	}
}

func TestCredentialRunsChildAsAnotherUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing a child's credentials requires root")
	}
	result, err := (ExecRunner{}).Run(context.Background(), Command{
		Path: "/usr/bin/id", Args: []string{"-u"}, Credential: &Credential{UID: 65534, GID: 65534},
	})
	if err != nil || strings.TrimSpace(result.Stdout) != "65534" {
		t.Fatalf("id -u = %q, %v", result.Stdout, err)
	}
}
