package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/buildinfo"
)

func TestRun(t *testing.T) {
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
		{"version", []string{"version"}, ExitOK, "go-port-test (commit abc123)"},
		{"version flag", []string{"--version"}, ExitOK, "go-port-test (commit abc123)"},
		{"unsupported", []string{"start"}, ExitUsage, ""},
		{"server command", []string{"example", "start"}, ExitUsage, ""},
		{"version extra", []string{"version", "extra"}, ExitUsage, ""},
		{"shell text", []string{"$(touch SHOULD_NOT_EXIST)"}, ExitUsage, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code := Run(tt.args, &out, &errOut, buildinfo.Info{Version: "go-port-test", Commit: "abc123"})
			if code != tt.code {
				t.Fatalf("exit=%d, want %d", code, tt.code)
			}
			if code == ExitOK {
				if !strings.Contains(out.String(), tt.want) || errOut.Len() != 0 {
					t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
				}
			} else if out.Len() != 0 || errOut.Len() == 0 {
				t.Fatalf("failure must use stderr only: stdout=%q stderr=%q", out.String(), errOut.String())
			}
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestWriteFailure(t *testing.T) {
	for _, command := range []string{"help", "version"} {
		var errOut bytes.Buffer
		if got := Run([]string{command}, brokenWriter{}, &errOut, buildinfo.Current()); got != ExitError {
			t.Fatalf("%s: exit=%d", command, got)
		}
		if !strings.Contains(errOut.String(), "closed") {
			t.Fatalf("missing error: %q", errOut.String())
		}
	}
}
