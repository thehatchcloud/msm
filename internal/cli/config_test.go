package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/servers"
)

func TestGlobalConfig(t *testing.T) {
	f := newServerFixture(t)
	out := f.mustRun(t, "config")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if lines[0] != `DEBUG="false"` {
		t.Fatalf("first line = %q", lines[0])
	}
	for _, want := range []string{
		`SERVER_STORAGE_PATH="` + f.root + `"`,
		`JAR_STORAGE_PATH="` + f.jars + `"`,
		`RAMDISK_STORAGE_ENABLED="false"`,
		`SERVER_PROPERTIES="server.properties"`,
		`DEFAULT_RAM="1024"`,
		`DEFAULT_INVOCATION="java -Xms{RAM}M -Xmx{RAM}M -jar {JAR} nogui"`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("config output lacks %s:\n%s", want, out)
		}
	}
	for _, l := range lines {
		if !strings.Contains(l, `="`) || !strings.HasSuffix(l, `"`) {
			t.Errorf("not a NAME=\"value\" line: %q", l)
		}
	}

	var doc globalConfigDocument
	decodeStrict(t, f.mustRun(t, "config", "--format", "json"), &doc)
	if doc.SchemaVersion != SchemaVersion || len(doc.Settings) != len(lines) {
		t.Fatalf("json = %+v", doc)
	}
	sources := map[string]string{}
	for _, s := range doc.Settings {
		sources[s.Name] = s.Source
	}
	if sources["SERVER_STORAGE_PATH"] != "msm.conf" || sources["DEFAULT_RAM"] != "default" {
		t.Errorf("sources = %v", sources)
	}
	if code, _, errOut := f.run(t, "", "config", "extra"); code != ExitUsage {
		t.Errorf("config extra = %d %q", code, errOut)
	}
}

// decodeStrict fails on any field the Go type does not declare, so the JSON
// documents cannot gain an undocumented field unnoticed.
func decodeStrict(t *testing.T, data string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decoding %q: %v", data, err)
	}
}

func TestServerConfigListReadWrite(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "alpha")
	props := filepath.Join(f.root, "alpha", "server.properties")
	original := "# Minecraft server properties\nmotd=Hello\nfuture-setting=\\u0041b\n\nmsm-stop-delay=3\n"
	if err := os.WriteFile(props, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	list := f.mustRun(t, "alpha", "config")
	for _, want := range []string{`msm-username="`, `msm-ram="1024"`, `msm-stop-delay="3"`, `msm-restart-delay="10"`} {
		if !strings.Contains(list, want) {
			t.Errorf("listing lacks %s:\n%s", want, list)
		}
	}
	if n := len(legacyconf.ServerSettingNames()); strings.Count(list, "\n") != n {
		t.Errorf("listing has %d lines, want one per registered setting (%d)", strings.Count(list, "\n"), n)
	}
	if out := f.mustRun(t, "alpha", "config", "motd"); out != "Hello\n" {
		t.Errorf("read motd = %q", out)
	}
	if out := f.mustRun(t, "alpha", "config", "msm-stop-delay"); out != "3\n" {
		t.Errorf("read msm-stop-delay = %q", out)
	}
	if code, out, errOut := f.run(t, "", "alpha", "config", "absent"); code != ExitNotFound || out != "" || !strings.Contains(errOut, `"absent"`) {
		t.Errorf("read absent = %d %q %q", code, out, errOut)
	}

	// A stopped server: the change is written in place and says when it applies.
	out := f.mustRun(t, "alpha", "config", "motd", "Hi there")
	if !strings.Contains(out, `Changed motd from "Hello" to "Hi there"`) || !strings.Contains(out, "takes effect the next time it starts") {
		t.Errorf("stopped write = %q", out)
	}
	data, _ := os.ReadFile(props)
	if want := strings.Replace(original, "motd=Hello", "motd=Hi there", 1); string(data) != want {
		t.Errorf("file = %q, want %q", data, want)
	}

	// A running server: the restart that is still due is spelled out.
	f.states["alpha"] = servers.Running
	out = f.mustRun(t, "alpha", "config", "server-port", "25570")
	if !strings.Contains(out, `Added server-port="25570"`) || !strings.Contains(out, "Restart required: server \"alpha\" is running") ||
		!strings.Contains(out, "msm alpha restart") {
		t.Errorf("running write = %q", out)
	}
	out = f.mustRun(t, "alpha", "config", "msm-stop-delay", "5")
	if !strings.Contains(out, "no restart is needed") || strings.Contains(out, "Restart required") {
		t.Errorf("immediate setting = %q", out)
	}
	// A setting the manager identifies the process by cannot change under it.
	code, _, errOut := f.run(t, "", "alpha", "config", "msm-ram", "2048")
	if code != ExitRefused || !strings.Contains(errOut, "only while the server is stopped") {
		t.Errorf("ram while running = %d %q", code, errOut)
	}
	f.states["alpha"] = servers.Unknown
	out = f.mustRun(t, "alpha", "config", "motd", "again")
	if !strings.Contains(out, "Restart required if server \"alpha\" is running (the manager could not tell") {
		t.Errorf("unknown state write = %q", out)
	}
}

func TestServerConfigRefusals(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "alpha")
	props := filepath.Join(f.root, "alpha", "server.properties")
	before, _ := os.ReadFile(props)
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"alpha", "config", "motd", "a\nop-permission-level=4"}, ExitUsage},
		{[]string{"alpha", "config", "bad=key", "x"}, ExitUsage},
		{[]string{"alpha", "config", "msm-bogus", "x"}, ExitConfig},
		{[]string{"alpha", "config", "msm-ram", "lots"}, ExitConfig},
		{[]string{"alpha", "config", "msm-invocation", "java; reboot"}, ExitConfig},
		{[]string{"ghost", "config"}, ExitNotFound},
		{[]string{"ghost", "config", "motd", "x"}, ExitNotFound},
		{[]string{"alpha", "config", "a", "b", "c"}, ExitUsage},
	} {
		code, out, errOut := f.run(t, "", tc.args...)
		if code != tc.code || out != "" || errOut == "" {
			t.Errorf("msm %v = %d %q %q, want %d", tc.args, code, out, errOut, tc.code)
		}
	}
	if after, _ := os.ReadFile(props); string(after) != string(before) {
		t.Errorf("a refused change altered the file: %q", after)
	}
}

func TestServerConfigAll(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "alpha")
	f.mustRun(t, "server", "create", "beta")
	out := f.mustRun(t, "all", "config", "msm-stop-delay", "7")
	for _, name := range []string{"alpha", "beta"} {
		if !strings.Contains(out, name+": Added msm-stop-delay=\"7\"") {
			t.Errorf("all write lacks %s:\n%s", name, out)
		}
		if got := f.mustRun(t, name, "config", "msm-stop-delay"); got != "7\n" {
			t.Errorf("%s stop-delay = %q", name, got)
		}
	}
	read := f.mustRun(t, "all", "config", "msm-stop-delay")
	if read != "alpha: 7\nbeta: 7\n" {
		t.Errorf("all read = %q", read)
	}
	// One server without the property does not hide the others.
	f.mustRun(t, "alpha", "config", "motd", "x")
	code, out, errOut := f.run(t, "", "all", "config", "motd")
	if code != ExitNotFound || out != "alpha: x\n" || !strings.Contains(errOut, `server "beta"`) {
		t.Errorf("all read partial = %d %q %q", code, out, errOut)
	}
	if code, _, _ := f.run(t, "", "all", "config", "--format", "json"); code != ExitUsage {
		t.Errorf("all config json = %d", code)
	}
	empty := newServerFixture(t)
	if out := empty.mustRun(t, "all", "config"); out != "[There are no servers]\n" {
		t.Errorf("no servers = %q", out)
	}
}

func TestServerConfigJSON(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "alpha")
	f.mustRun(t, "alpha", "config", "msm-ram", "4096")
	var doc serverConfigDocument
	decodeStrict(t, f.mustRun(t, "alpha", "config", "--format", "json"), &doc)
	if doc.SchemaVersion != SchemaVersion || doc.Server != "alpha" || len(doc.Settings) != len(legacyconf.ServerSettingNames()) {
		t.Fatalf("doc = %+v", doc)
	}
	for _, s := range doc.Settings {
		if s.Key == "msm-ram" && (s.Value != "4096" || s.Source != "server.properties") {
			t.Errorf("ram = %+v", s)
		}
		if s.Key == "msm-stop-delay" && s.Source != "default" {
			t.Errorf("stop-delay = %+v", s)
		}
	}
	// JSON is only offered where a program can rely on it.
	for _, args := range [][]string{
		{"alpha", "config", "motd", "--format", "json"},
		{"alpha", "config", "motd", "x", "--format", "json"},
		{"alpha", "start", "--format", "json"},
		{"stop", "--format", "json"},
		{"jargroup", "list", "--format", "json"},
		{"alpha", "status", "--format", "yaml"},
	} {
		if code, out, errOut := f.run(t, "", args...); code != ExitUsage || out != "" || !strings.Contains(errOut, "--format") {
			t.Errorf("msm %v = %d %q %q", args, code, out, errOut)
		}
	}
}
