package cli

import (
	"strings"
	"testing"
)

// The server-first console grammar: every legacy signature is accepted
// (and reaches the console, which fails here only because screen is
// missing), and anything else is the legacy "No such command".
func TestConsoleGrammar(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "a")
	f.mustRun(t, "server", "create", "b")
	valid := [][]string{
		{"connected"},
		{"console"},
		{"whitelist", "on"}, {"wl", "off"}, {"whitelist", "add", "x"}, {"wl", "add", "x", "y"}, {"wl", "remove", "x"}, {"whitelist", "list"},
		{"blacklist", "player", "add", "x", "y"}, {"bl", "player", "remove", "x"},
		{"bl", "ip", "add", "1.2.3.4"}, {"bl", "ip", "remove", "1.2.3.4"}, {"bl", "list"},
		{"operator", "add", "x", "y"}, {"op", "remove", "x"}, {"op", "list"},
		{"gamemode", "creative", "x"}, {"gm", "0", "x", "y"},
		{"kick", "x"}, {"say", "hello", "world"},
		{"time", "set", "100"}, {"time", "add", "5"},
		{"toggledownfall"}, {"tdf"},
		{"give", "x", "1"}, {"give", "x", "1", "2"}, {"give", "x", "1", "2", "3"},
		{"xp", "x", "5"},
		{"save", "on"}, {"save", "off"}, {"save", "all"},
		{"cmd", "weather", "clear"}, {"cmdlog", "list"},
	}
	for _, args := range valid {
		code, _, errOut := f.run(t, "", append([]string{"a"}, args...)...)
		want := "screen is not installed"
		if args[0] == "whitelist" && args[1] == "list" || args[0] == "wl" && args[1] == "list" || args[0] == "op" && args[1] == "list" ||
			args[0] == "bl" && args[1] == "list" {
			want = "" // the list commands read files and need no screen
		}
		if args[0] == "connected" {
			want = "screen is not installed"
		}
		if want == "" {
			if code != ExitOK {
				t.Errorf("%v: %d %q", args, code, errOut)
			}
			continue
		}
		if code != ExitError || !strings.Contains(errOut, want) {
			t.Errorf("%v: %d %q; want an error naming screen", args, code, errOut)
		}
		if strings.Contains(errOut, "No such command") {
			t.Errorf("%v rejected by the grammar", args)
		}
	}
	for _, args := range [][]string{
		{"whitelist"}, {"whitelist", "on", "x"}, {"whitelist", "add"}, {"wl", "toggle"},
		{"blacklist", "player"}, {"blacklist", "ip", "add"}, {"blacklist", "host", "add", "x"}, {"bl", "player", "list"},
		{"op"}, {"op", "list", "x"}, {"gamemode"}, {"gamemode", "creative"}, {"kick"}, {"say"},
		{"time"}, {"time", "set"}, {"time", "set", "1", "2"}, {"time", "fly", "1"}, {"toggledownfall", "x"},
		{"give"}, {"give", "x"}, {"give", "x", "1", "2", "3", "4"}, {"xp", "x"}, {"xp", "x", "1", "2"},
		{"save"}, {"save", "now"}, {"cmd"}, {"cmdlog"}, {"connected", "x"}, {"console", "x"},
	} {
		code, out, errOut := f.run(t, "", append([]string{"a"}, args...)...)
		if code != ExitError || out != "" || !strings.HasPrefix(errOut, "No such command") && !strings.Contains(errOut, "command") {
			t.Errorf("%v: %d %q %q", args, code, out, errOut)
		}
	}
}

func TestConsoleAllTargets(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "a")
	f.mustRun(t, "server", "create", "b")
	for _, args := range [][]string{{"all", "console"}, {"all", "cmdlog", "list"}} {
		code, _, errOut := f.run(t, "", args...)
		if code != ExitError || !strings.Contains(errOut, "needs one server") {
			t.Errorf("%v: %d %q", args, code, errOut)
		}
	}
	// Every other command reaches every server and reports each failure.
	code, _, errOut := f.run(t, "", "all", "say", "hi")
	if code != ExitError || strings.Count(errOut, "screen is not installed") != 2 {
		t.Fatalf("all say: %d %q", code, errOut)
	}
	if code, out, _ := f.run(t, "", "all", "wl", "list"); code != ExitOK || strings.Count(out, "No players are whitelisted.") != 2 {
		t.Fatalf("all wl list: %d %q", code, out)
	}
	if code, _, errOut := f.run(t, "", "missing", "say", "hi"); code != ExitError || !strings.Contains(errOut, "no server with the name") {
		t.Fatalf("missing: %d %q", code, errOut)
	}
	if code, _, errOut := f.run(t, "", "a", "say", "hi", "--timeout", "-1s"); code != ExitError || !strings.Contains(errOut, "--timeout") {
		t.Fatalf("negative timeout: %d %q", code, errOut)
	}
}

// A stopped server reports no players without needing screen's answer.
func TestConnectedStopped(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "a")
	code, _, errOut := f.run(t, "", "a", "connected")
	if code != ExitError || !strings.Contains(errOut, "screen is not installed") {
		t.Fatalf("%d %q", code, errOut)
	}
}
