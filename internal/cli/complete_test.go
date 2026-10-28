package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// complete runs Cobra's completion entry point, which the generated bash
// and zsh scripts call, and returns the candidates it printed.
func (f *serverFixture) complete(t *testing.T, words ...string) []string {
	t.Helper()
	code, out, errOut := f.run(t, "", append([]string{"__complete"}, words...)...)
	if code != ExitOK {
		t.Fatalf("__complete %q: exit %d, stderr %q", words, code, errOut)
	}
	if errOut != "" && !strings.HasPrefix(errOut, "Completion ended with directive") {
		t.Fatalf("__complete %q wrote to stderr: %q", words, errOut)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, ":") {
		t.Fatalf("__complete %q: no directive line in %q", words, out)
	}
	var names []string
	for _, l := range lines[:len(lines)-1] {
		name, _, _ := strings.Cut(l, "\t")
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func completionFixture(t *testing.T) *serverFixture {
	t.Helper()
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "survival")
	f.mustRun(t, "server", "create", "creative")
	dir := filepath.Join(f.root, "survival")
	for _, w := range []string{"worldstorage/main", "worldstorage/nether", "worldstorage_inactive/archive"} {
		if err := os.MkdirAll(filepath.Join(dir, w), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"whitelist.json":      `[{"uuid":"1","name":"Notch"},{"uuid":"2","name":"jeb_"},{"uuid":"3","name":"bad name\nrm -rf /"},{"uuid":"4","name":"$(touch x)"},{"uuid":"5","name":"semi;colon"}]`,
		"banned-players.json": `[{"uuid":"1","name":"Griefer"}]`,
		"banned-ips.json":     `[{"ip":"203.0.113.9"},{"ip":"2001:db8::1"}]`,
		"ops.json":            `[{"uuid":"1","name":"Admin","level":4}]`,
		"server.properties":   "motd=hi\nlevel-name=main\nmsm-ram=512\n# comment=1\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.mustRun(t, "jargroup", "create", "paper", "https://example.invalid/paper.jar")
	f.mustRun(t, "jargroup", "create", "vanilla", "minecraft")
	for _, j := range []string{"paper/2026-01-01-00-00-00-paper.jar", "paper/2026-02-01-00-00-00-paper.jar"} {
		if err := os.WriteFile(filepath.Join(f.jars, j), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestCompletionOfServerFirstGrammar(t *testing.T) {
	f := completionFixture(t)
	tests := []struct {
		name  string
		words []string
		want  []string
		// subset: want must be included, not exact
		subset bool
	}{
		{"commands and servers", []string{""}, []string{"all", "config", "creative", "survival", "server", "jargroup", "start", "stop", "restart", "version", "update", "maintenance"}, true},
		{"server prefix", []string{"su"}, []string{"survival"}, false},
		{"server verbs", []string{"survival", ""}, serverCommandWords, false},
		{"verbs for all", []string{"all", ""}, serverCommandWords, false},
		{"verb prefix", []string{"survival", "wor"}, []string{"worlds"}, false},
		{"unknown server", []string{"nobody", ""}, nil, false},
		{"stop now", []string{"survival", "stop", ""}, []string{"now"}, false},
		{"restart now", []string{"survival", "restart", ""}, []string{"now"}, false},
		{"no more after now", []string{"survival", "stop", "now", ""}, nil, false},
		{"global stop now", []string{"stop", ""}, []string{"now"}, true},
		{"worlds verbs", []string{"survival", "worlds", ""}, []string{"list", "load", "ram", "todisk", "backup", "on", "off"}, false},
		{"worlds ram active", []string{"survival", "worlds", "ram", ""}, []string{"main", "nether"}, false},
		{"worlds off active", []string{"survival", "worlds", "off", ""}, []string{"main", "nether"}, false},
		{"worlds on inactive", []string{"survival", "worlds", "on", ""}, []string{"archive"}, false},
		{"worlds on prefix", []string{"survival", "worlds", "on", "x"}, nil, false},
		{"worlds for all", []string{"all", "worlds", "off", ""}, nil, false},
		{"worlds list takes nothing", []string{"survival", "worlds", "list", ""}, nil, false},
		{"jar groups", []string{"survival", "jar", ""}, []string{"paper", "vanilla"}, false},
		{"jar files", []string{"survival", "jar", "paper", ""}, []string{"2026-01-01-00-00-00-paper.jar", "2026-02-01-00-00-00-paper.jar"}, false},
		{"jar files of an empty group", []string{"survival", "jar", "vanilla", ""}, nil, false},
		{"jar files of no group", []string{"survival", "jar", "nope", ""}, nil, false},
		{"whitelist verbs", []string{"survival", "whitelist", ""}, []string{"on", "off", "add", "remove", "list"}, false},
		{"wl alias", []string{"survival", "wl", ""}, []string{"on", "off", "add", "remove", "list"}, false},
		{"whitelist remove", []string{"survival", "wl", "remove", ""}, []string{"Notch", "jeb_"}, false},
		{"whitelist remove second", []string{"survival", "wl", "remove", "Notch", ""}, []string{"Notch", "jeb_"}, false},
		{"whitelist add", []string{"survival", "wl", "add", ""}, nil, false},
		{"blacklist verbs", []string{"survival", "bl", ""}, []string{"player", "ip", "list"}, false},
		{"blacklist player verbs", []string{"survival", "bl", "player", ""}, []string{"add", "remove"}, false},
		{"blacklist ip verbs", []string{"survival", "blacklist", "ip", ""}, []string{"add", "remove"}, false},
		{"blacklist player remove", []string{"survival", "bl", "player", "remove", ""}, []string{"Griefer"}, false},
		{"blacklist ip remove", []string{"survival", "bl", "ip", "remove", ""}, []string{"2001:db8::1", "203.0.113.9"}, false},
		{"operator verbs", []string{"survival", "op", ""}, []string{"add", "remove", "list"}, false},
		{"operator remove", []string{"survival", "op", "remove", ""}, []string{"Admin"}, false},
		{"gamemode", []string{"survival", "gm", ""}, []string{"survival", "creative", "adventure", "spectator"}, false},
		{"time", []string{"survival", "time", ""}, []string{"set", "add"}, false},
		{"save", []string{"survival", "save", ""}, []string{"on", "off", "all"}, false},
		{"config keys", []string{"survival", "config", ""}, []string{"level-name", "motd", "msm-ram", "msm-stop-delay"}, true},
		{"config msm prefix", []string{"survival", "config", "msm-rest"}, []string{"msm-restart-delay"}, false},
		{"config for all", []string{"all", "config", ""}, nil, false},
		{"server delete", []string{"server", "delete", ""}, []string{"creative", "survival"}, false},
		{"server delete done", []string{"server", "delete", "creative", ""}, nil, false},
		{"server rename", []string{"server", "rename", ""}, []string{"creative", "survival"}, false},
		{"server rename new name", []string{"server", "rename", "creative", ""}, nil, false},
		{"server verbs list", []string{"server", ""}, []string{"list", "create", "delete", "rename"}, true},
		{"jargroup delete", []string{"jargroup", "delete", ""}, []string{"paper", "vanilla"}, false},
		{"jargroup changeurl", []string{"jargroup", "changeurl", ""}, []string{"paper", "vanilla"}, false},
		{"jargroup getlatest", []string{"jargroup", "getlatest", "p"}, []string{"paper"}, false},
		{"jargroup rename", []string{"jargroup", "rename", ""}, []string{"paper", "vanilla"}, false},
		{"jargroup rename second", []string{"jargroup", "rename", "paper", ""}, nil, false},
		{"jargroup verbs", []string{"jargroup", ""}, []string{"list", "create", "delete", "rename", "changeurl", "getlatest"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := f.complete(t, tt.words...)
			want := slices.Clone(tt.want)
			slices.Sort(want)
			if tt.subset {
				for _, w := range want {
					if !slices.Contains(got, w) {
						t.Fatalf("%q: %v lacks %q", tt.words, got, w)
					}
				}
				return
			}
			if !slices.Equal(got, want) {
				t.Fatalf("%q = %v, want %v", tt.words, got, want)
			}
		})
	}
}

// Whatever the files hold, a candidate cannot carry a newline, a quote or
// a shell metacharacter into the user's command line.
func TestCompletionOutputIsSafe(t *testing.T) {
	f := completionFixture(t)
	for _, words := range [][]string{
		{"survival", "wl", "remove", ""}, {"survival", "config", ""}, {"survival", "worlds", "ram", ""}, {""},
	} {
		for _, name := range f.complete(t, words...) {
			if !completableName.MatchString(name) {
				t.Errorf("%q offered %q", words, name)
			}
		}
	}
	// A world or property with an unsafe name is dropped, not escaped.
	if err := os.MkdirAll(filepath.Join(f.root, "survival", "worldstorage", "a;b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "survival", "server.properties"), []byte("ok=1\nbad key=1\n$(x)=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.complete(t, "survival", "worlds", "off", ""); slices.Contains(got, "a;b") {
		t.Errorf("unsafe world offered: %v", got)
	}
	got := f.complete(t, "survival", "config", "")
	if !slices.Contains(got, "ok") || slices.Contains(got, "bad key") || slices.Contains(got, "$(x)") {
		t.Errorf("config keys = %v", got)
	}
}

// Completion runs on every Tab: it must not need the configuration to be
// valid, must not write anything and must not print warnings or errors.
func TestCompletionIsSilentWhenTheConfigurationIsBroken(t *testing.T) {
	f := completionFixture(t)
	if err := os.WriteFile(os.Getenv("MSM_CONF"), []byte("SERVER_STORAGE_PATH=$(touch /tmp/pwned)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, words := range [][]string{{""}, {"survival", ""}, {"server", "delete", ""}, {"jargroup", "delete", ""}, {"survival", "jar", ""}} {
		for _, name := range f.complete(t, words...) {
			if name == "survival" || name == "paper" {
				t.Errorf("%q still offered %q from a configuration that cannot be read", words, name)
			}
		}
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		t.Fatal("configuration was evaluated")
	}
}

func TestCompletionSkipsOversizeAndMalformedLists(t *testing.T) {
	f := completionFixture(t)
	wl := filepath.Join(f.root, "survival", "whitelist.json")
	for name, data := range map[string]string{
		"not json":     "Notch\njeb_\n",
		"not an array": `{"name":"Notch"}`,
		"oversize":     `[{"name":"Notch"}]` + strings.Repeat(" ", maxListBytes+1),
		"wrong type":   `[{"name":5},{"name":null},"Notch"]`,
		"unreadable":   "",
	} {
		if err := os.WriteFile(wl, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if name == "unreadable" {
			os.Remove(wl)
		}
		if got := f.complete(t, "survival", "wl", "remove", ""); len(got) != 0 {
			t.Errorf("%s: offered %v", name, got)
		}
	}
}

func TestGeneratedScriptsCallTheGoCompleter(t *testing.T) {
	f := newServerFixture(t)
	for _, shell := range []string{"bash", "zsh"} {
		out := f.mustRun(t, "completion", shell)
		if !strings.Contains(out, "__complete") {
			t.Errorf("%s script does not call msm __complete", shell)
		}
		for _, bad := range []string{"/etc/init.d", "source \"$MSM", "msm.sh", "eval $(", "$(ls "} {
			if strings.Contains(out, bad) {
				t.Errorf("%s script contains %q", shell, bad)
			}
		}
	}
}

// completions/ holds what `msm completion bash|zsh` prints, so the installers
// can ship them without running the binary. Regenerate them with
// `make completions` after a Cobra upgrade.
func TestShippedCompletionsAreCurrent(t *testing.T) {
	f := newServerFixture(t)
	for shell, file := range map[string]string{"bash": "msm.bash", "zsh": "_msm"} {
		want := f.mustRun(t, "completion", shell)
		got, err := os.ReadFile(filepath.Join("..", "..", "completions", file))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("completions/%s is stale; run: make completions", file)
		}
	}
}

// A FIFO (or any non-regular file) where a list should be must not make
// completion wait for a writer.
func TestCompletionNeverBlocksOnASpecialFile(t *testing.T) {
	f := completionFixture(t)
	dir := filepath.Join(f.root, "survival")
	for _, name := range []string{"whitelist.json", "server.properties"} {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.complete(t, "survival", "wl", "remove", "")
		f.complete(t, "survival", "config", "msm-ra")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("completion blocked on a FIFO")
	}
}
