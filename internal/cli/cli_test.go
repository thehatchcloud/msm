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
		{"empty", nil, ExitError, ""},
		{"help", []string{"help"}, ExitOK, " command:\n\n--Setup Commands"},
		{"server help", []string{"server"}, ExitOK, "rename"},
		{"long help", []string{"--help"}, ExitOK, " command:\n\n--Setup Commands"},
		{"short help", []string{"-h"}, ExitOK, " command:\n\n--Setup Commands"},
		{"command help", []string{"help", "version"}, ExitOK, "Show the Go-port version"},
		{"version", []string{"version"}, ExitOK, "go-port-test (commit abc123)"},
		{"version flag", []string{"--version"}, ExitOK, "go-port-test (commit abc123)"},
		{"unknown server command", []string{"example", "frobnicate"}, ExitError, ""},
		{"server without command", []string{"example"}, ExitError, ""},
		{"global extra argument", []string{"stop", "later"}, ExitError, ""},
		{"start now", []string{"start", "now"}, ExitError, ""},
		{"status now", []string{"example", "status", "now"}, ExitError, ""},
		{"bad jobs", []string{"stop", "--jobs", "0"}, ExitError, ""},
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
			code := Run(tt.args, nil, &out, &errOut, buildinfo.Info{Version: "go-port-test", Commit: "abc123"})
			if code != tt.code {
				t.Fatalf("exit=%d, want %d; stderr=%q", code, tt.code, errOut.String())
			}
			if code == ExitOK {
				if !strings.Contains(out.String(), tt.want) || errOut.Len() != 0 {
					t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
				}
			} else if out.Len() != 0 || strings.Count(errOut.String(), "msm: ") != 1 && errOut.String() != noSuchCommand(programPath(os.Args[0]))+"\n" {
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
		if code := Run(tt.args, nil, &out, &errOut, buildinfo.Current()); code != ExitOK {
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
		if code := Run([]string{"version", "--config", file}, nil, &out, &errOut, buildinfo.Current()); code != ExitError {
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
			if code := Run(args, nil, &out, &errOut, buildinfo.Current()); code != ExitOK || errOut.Len() != 0 {
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
			if code := Run([]string{"completion", shell}, nil, &out, &errOut, buildinfo.Current()); code != ExitOK {
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
	if got := Run([]string{"version"}, nil, brokenWriter{}, &errOut, buildinfo.Current()); got != ExitError {
		t.Fatalf("exit=%d", got)
	}
	if !strings.Contains(errOut.String(), "closed") {
		t.Fatalf("missing error: %q", errOut.String())
	}
}

// msm help prints init/msm's command_help verbatim, and anything that
// matches no command gets the legacy "No such command" line (on stderr,
// with a nonzero status).
func TestLegacyHelpAndNoSuchCommand(t *testing.T) {
	isolateConfig(t)
	src, err := os.ReadFile("../../init/msm")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src[strings.Index(string(src), "command_help() {"):])
	body = body[:strings.Index(body, "\n}\n")]
	var want strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "echo -e":
			want.WriteString("\n")
		case strings.HasPrefix(line, `echo -e "`):
			text := strings.TrimSuffix(strings.TrimPrefix(line, `echo -e "`), `"`)
			text = strings.ReplaceAll(strings.ReplaceAll(text, `\"`, `"`), "$0", programPath(os.Args[0]))
			want.WriteString(text + "\n")
		}
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, buildinfo.Current()); code != ExitOK || out.String() != want.String() || errOut.Len() != 0 {
			t.Fatalf("msm %v: exit %d stderr %q\ngot:\n%s", args, code, errOut.String(), out.String())
		}
	}
	for _, args := range [][]string{nil, {"survival"}, {"survival", "frobnicate"}, {"survival", "status", "now"},
		{"stop", "later"}, {"start", "now"}, {"jargroup", "list"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, buildinfo.Current()); code != ExitError || out.Len() != 0 || errOut.String() != "No such command. See "+programPath(os.Args[0])+" help\n" {
			t.Errorf("msm %v: exit %d stdout %q stderr %q", args, code, out.String(), errOut.String())
		}
	}
}

// The legacy messages name the executable as bash's $0 would: as typed
// when it contains a '/', else the full path found on PATH.
func TestProgramPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "msm"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for arg0, want := range map[string]string{
		"msm":                filepath.Join(dir, "msm"),
		"./bin/msm":          "./bin/msm",
		"/usr/local/bin/msm": "/usr/local/bin/msm",
		"not-on-path":        self,
		"":                   self,
	} {
		if got := programPath(arg0); got != want {
			t.Errorf("programPath(%q) = %q, want %q", arg0, got, want)
		}
	}
	var out, errOut bytes.Buffer
	d := defaultDeps()
	d.program = "/opt/msm/msm"
	if code := run([]string{"help"}, nil, &out, &errOut, buildinfo.Current(), d); code != ExitOK || !strings.HasPrefix(out.String(), "Usage: /opt/msm/msm command:\n") {
		t.Fatalf("help: %d %q", code, out.String())
	}
	out.Reset()
	if code := run(nil, nil, &out, &errOut, buildinfo.Current(), d); code != ExitError || errOut.String() != "No such command. See /opt/msm/msm help\n" {
		t.Fatalf("no command: %d %q", code, errOut.String())
	}
}
