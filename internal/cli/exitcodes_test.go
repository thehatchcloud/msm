package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/backup"
	"github.com/thehatchcloud/msm/internal/console"
	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/jars"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/lifecycle"
	"github.com/thehatchcloud/msm/internal/maintenance"
	"github.com/thehatchcloud/msm/internal/profiles"
	"github.com/thehatchcloud/msm/internal/servers"
	"github.com/thehatchcloud/msm/internal/worlds"
)

func TestExitCodeMapping(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("while doing it: %w", err) }
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, ExitOK},
		{errors.New("something else"), ExitError},
		{errNoSuchCommand, ExitUsage},
		{usageError{errors.New("accepts 1 arg(s)")}, ExitUsage},
		{wrap(profiles.ErrBadArgument), ExitUsage},
		{wrap(maintenance.ErrOptions), ExitUsage},
		{wrap(console.ErrAmbiguous), ExitUsage},
		{&legacyconf.SyntaxError{}, ExitConfig},
		{wrap(legacyconf.ErrUnsafeInvocation), ExitConfig},
		{wrap(lifecycle.ErrSettings), ExitConfig},
		{wrap(backup.ErrSettings), ExitConfig},
		{wrap(worlds.ErrRAMConfig), ExitConfig},
		{wrap(servers.ErrBadSetting), ExitConfig},
		{wrap(servers.ErrNotFound), ExitNotFound},
		{wrap(servers.ErrInvalidName), ExitNotFound},
		{wrap(jars.ErrNotFound), ExitNotFound},
		{wrap(worlds.ErrNoWorld), ExitNotFound},
		{noSuchWorld("x"), ExitNotFound},
		{wrap(servers.ErrNoProperty), ExitNotFound},
		{wrap(errNeedsConfirmation), ExitRefused},
		{wrap(servers.ErrRunning), ExitRefused},
		{wrap(servers.ErrNotStopped), ExitRefused},
		{wrap(servers.ErrExists), ExitRefused},
		{wrap(filelock.ErrBusy), ExitRefused},
		{wrap(jars.ErrInUse), ExitRefused},
		{wrap(errUnavailable), ExitUnavailable},
		{wrap(lifecycle.ErrNoScreen), ExitUnavailable},
		{wrap(console.ErrNoScreen), ExitUnavailable},
		{wrap(backup.ErrToolMissing), ExitUnavailable},
		{context.Canceled, ExitInterrupted},
		{wrap(lifecycle.ErrInterrupted), ExitInterrupted},
		// Several failures at once report the first cause in the table's
		// order, so a bulk command's status does not depend on which
		// server finished first.
		{errors.Join(wrap(servers.ErrNotFound), wrap(lifecycle.ErrNoScreen)), ExitNotFound},
		{errors.Join(wrap(lifecycle.ErrNoScreen), wrap(servers.ErrNotFound)), ExitNotFound},
		{errors.Join(errors.New("x"), wrap(servers.ErrRunning)), ExitRefused},
	} {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("exitCode(%v) = %d (%s), want %d (%s)", tc.err, got, ExitName(got), tc.want, ExitName(tc.want))
		}
	}
}

func TestExitNamesAreStableAndUnique(t *testing.T) {
	seen := map[string]int{}
	for _, code := range []int{ExitOK, ExitError, ExitUsage, ExitConfig, ExitNotFound, ExitRefused, ExitUnavailable, ExitInterrupted} {
		name := ExitName(code)
		if name == "unknown" || seen[name] != 0 {
			t.Errorf("code %d has name %q", code, name)
		}
		seen[name] = code + 1
	}
	if ExitName(99) != "unknown" {
		t.Error("an unassigned code must not have a name")
	}
}

// docs/cli.md and `msm help exit-codes` list the same statuses the code
// defines, so the documentation cannot silently drift.
func TestExitCodesAreDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/cli.md")
	if err != nil {
		t.Fatal(err)
	}
	f := newServerFixture(t)
	topic := f.mustRun(t, "help", "exit-codes")
	for _, code := range []int{ExitOK, ExitError, ExitUsage, ExitConfig, ExitNotFound, ExitRefused, ExitUnavailable, ExitInterrupted} {
		row := fmt.Sprintf("| %d | `%s` |", code, ExitName(code))
		if !strings.Contains(string(doc), row) {
			t.Errorf("docs/cli.md lacks the row %q", row)
		}
		line := fmt.Sprintf("%d    %s", code, ExitName(code))
		if !strings.Contains(strings.Join(strings.Fields(topic), " "), strings.Join(strings.Fields(line), " ")) {
			t.Errorf("msm help exit-codes lacks %q", line)
		}
	}
}

func TestHelpTopicsAndCommands(t *testing.T) {
	f := newServerFixture(t)
	for _, topic := range helpTopicNames() {
		if out := f.mustRun(t, "help", topic); !strings.HasSuffix(out, "\n") || len(out) < 100 {
			t.Errorf("topic %s = %q", topic, out)
		}
	}
	if out := f.mustRun(t, "help", "server", "delete"); !strings.Contains(out, "--yes") {
		t.Errorf("help server delete = %q", out)
	}
	for _, args := range [][]string{{"help", "nonsense"}, {"help", "version", "extra"}, {"help", "alpha"}, {"help", "exit-codes", "x"}} {
		if code, out, errOut := f.run(t, "", args...); code != ExitUsage || out != "" || !strings.HasPrefix(errOut, "No such command") {
			t.Errorf("msm %v = %d %q %q", args, code, out, errOut)
		}
	}
}

// Nothing waits for an answer that cannot come, and nothing destructive
// runs on a missing answer. The golden test already fails any command that
// reads standard input without a terminal; this adds that the refusals are
// the documented ones and change nothing.
func TestDestructiveCommandsNeedExplicitAuthorization(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "doomed")
	f.mustRun(t, "jargroup", "create", "doomedjars", "https://example.invalid/x.jar")
	f.stdin = failingReader{t, "destructive command"}
	for _, args := range [][]string{{"server", "delete", "doomed"}, {"jargroup", "delete", "doomedjars"}} {
		code, _, errOut := f.run(t, "", args...)
		if code != ExitRefused || !strings.Contains(errOut, "--yes") {
			t.Errorf("msm %v = %d %q", args, code, errOut)
		}
	}
	if _, err := os.Stat(f.root + "/doomed"); err != nil {
		t.Errorf("the server was removed without authorization: %v", err)
	}
	f.stdin = nil
	f.mustRun(t, "server", "delete", "--yes", "doomed")
	f.mustRun(t, "jargroup", "delete", "--yes", "doomedjars")
}
