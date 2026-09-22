package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/buildinfo"
)

func isolateConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MSM_DEBUG", "")
}

func TestRun(t *testing.T) {
	isolateConfig(t)
	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"empty", nil, ExitOK, "Usage:"},
		{"help", []string{"help"}, ExitOK, "not implemented yet"},
		{"long help", []string{"--help"}, ExitOK, "Usage:"},
		{"short help", []string{"-h"}, ExitOK, "Usage:"},
		{"command help", []string{"help", "version"}, ExitOK, "Show the Go-port version"},
		{"version", []string{"version"}, ExitOK, "go-port-test (commit abc123)"},
		{"version flag", []string{"--version"}, ExitOK, "go-port-test (commit abc123)"},
		{"unsupported", []string{"start"}, ExitError, ""},
		{"server command", []string{"example", "start"}, ExitError, ""},
		{"version extra", []string{"version", "extra"}, ExitError, ""},
		{"invalid flag", []string{"--does-not-exist"}, ExitError, ""},
		{"invalid bool", []string{"version", "--debug=bogus"}, ExitError, ""},
		{"missing flag value", []string{"version", "--config"}, ExitError, ""},
		{"shell text", []string{"$(touch SHOULD_NOT_EXIST)"}, ExitError, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := Run(tt.args, &out, &errOut, buildinfo.Info{Version: "go-port-test", Commit: "abc123"})
			if code != tt.code {
				t.Fatalf("exit=%d, want %d; stderr=%q", code, tt.code, errOut.String())
			}
			if code == ExitOK {
				if !strings.Contains(out.String(), tt.want) || errOut.Len() != 0 {
					t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
				}
			} else if out.Len() != 0 || strings.Count(errOut.String(), "msm: ") != 1 {
				t.Fatalf("failure must be printed once to stderr: stdout=%q stderr=%q", out.String(), errOut.String())
			}
		})
	}
}

func TestPersistentFlagsAndIsolation(t *testing.T) {
	isolateConfig(t)
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("debug: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		args  []string
		debug bool
	}{
		{[]string{"--config", file, "version"}, true},
		{[]string{"version", "--config", file}, true},
		{[]string{"version", "--config", file, "--debug=false"}, false},
		{[]string{"version", "--debug"}, true},
		{[]string{"version"}, false}, // A previous flag/file must not leak.
	}
	for _, tt := range tests {
		var out, errOut bytes.Buffer
		if code := Run(tt.args, &out, &errOut, buildinfo.Current()); code != ExitOK {
			t.Fatalf("args=%v exit=%d stderr=%s", tt.args, code, errOut.String())
		}
		if strings.Contains(errOut.String(), "debug:") != tt.debug {
			t.Fatalf("args=%v stderr=%q", tt.args, errOut.String())
		}
	}
}

func TestConfigurationErrors(t *testing.T) {
	isolateConfig(t)
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("debug: [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{bad, filepath.Join(t.TempDir(), "missing.yaml")} {
		var out, errOut bytes.Buffer
		if code := Run([]string{"version", "--config", file}, &out, &errOut, buildinfo.Current()); code != ExitError {
			t.Fatalf("config error returned %d", code)
		}
		if out.Len() != 0 || !strings.Contains(errOut.String(), "read configuration") {
			t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
		}
	}
}

func TestInformationalFlagsSkipConfig(t *testing.T) {
	isolateConfig(t)
	file := filepath.Join(t.TempDir(), "missing.yaml")
	// Cobra handles help and the built-in version flag before pre-run hooks.
	for _, arg := range []string{"--help", "--version"} {
		for _, args := range [][]string{{arg, "--config", file}, {"--config", file, arg}} {
			var out, errOut bytes.Buffer
			if code := Run(args, &out, &errOut, buildinfo.Current()); code != ExitOK || errOut.Len() != 0 {
				t.Fatalf("args=%v exit=%d stderr=%s", args, code, errOut.String())
			}
		}
	}
}

func TestCobraCompletion(t *testing.T) {
	isolateConfig(t)
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := Run([]string{"completion", shell}, &out, &errOut, buildinfo.Current()); code != ExitOK {
				t.Fatalf("exit=%d stderr=%s", code, errOut.String())
			}
			if out.Len() == 0 || !strings.Contains(out.String(), "msm") {
				t.Fatal("missing generated completion script")
			}
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestRunEWriteFailure(t *testing.T) {
	isolateConfig(t)
	var errOut bytes.Buffer
	if got := Run([]string{"version"}, brokenWriter{}, &errOut, buildinfo.Current()); got != ExitError {
		t.Fatalf("exit=%d", got)
	}
	if !strings.Contains(errOut.String(), "closed") {
		t.Fatalf("missing error: %q", errOut.String())
	}
}
