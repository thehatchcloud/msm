package servers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/serverprops"
)

func writeProps(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "server.properties")
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigListIsEffectiveSettingsInRegistrationOrder(t *testing.T) {
	e := newEnv(t, "DEFAULT_RAM=\"2048\"\n")
	dir := e.create(t, "alpha")
	writeProps(t, dir, "motd=hi\nmsm-stop-delay=3\n")
	list, _, err := e.mgr.ConfigList("alpha")
	if err != nil {
		t.Fatal(err)
	}
	names := legacyconf.ServerSettingNames()
	if len(list) != len(names) || list[0].Key != "msm-username" {
		t.Fatalf("list = %v", list)
	}
	got := map[string]ConfigSetting{}
	for _, c := range list {
		got[c.Key] = c
	}
	if c := got["msm-stop-delay"]; c.Value != "3" || c.Source != legacyconf.FromServer {
		t.Errorf("stop-delay = %+v", c)
	}
	if c := got["msm-ram"]; c.Value != "2048" || c.Source != legacyconf.FromGlobal {
		t.Errorf("ram = %+v", c)
	}
	if c := got["msm-jar-path"]; c.Value != filepath.Join(dir, "server.jar") {
		t.Errorf("jar-path = %+v", c)
	}
}

func TestConfigGet(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "alpha")
	writeProps(t, dir, "Motd=Upper\nmotd=\"lower\"\nmsm-ram=512\n")
	for key, want := range map[string]string{"motd": "lower", "Motd": "Upper", "msm-ram": "512", "MSM-RAM": "512", "msm-stop-delay": "10"} {
		if got, err := e.mgr.ConfigGet("alpha", key); err != nil || got != want {
			t.Errorf("ConfigGet(%q) = %q, %v; want %q", key, got, err, want)
		}
	}
	if _, err := e.mgr.ConfigGet("alpha", "absent"); !errors.Is(err, ErrNoProperty) {
		t.Errorf("absent property: %v", err)
	}
	if _, err := e.mgr.ConfigGet("alpha", "msm-bogus"); !errors.Is(err, ErrUnknownSetting) {
		t.Errorf("unknown setting: %v", err)
	}
	if _, err := e.mgr.ConfigGet("missing", "motd"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing server: %v", err)
	}
}

func TestConfigSetPreservesCommentsUnknownKeysAndMode(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "alpha")
	path := writeProps(t, dir, "# header\n\n!bang\nfuture-key=\\u0041 x\nmotd=old\nmsm-ram=512\n")
	c, err := e.mgr.ConfigSet(context.Background(), "alpha", "motd", "new motd")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Existed || c.Old != "old" || c.New != "new motd" || c.Effect != OnRestart {
		t.Fatalf("change = %+v", c)
	}
	data, _ := os.ReadFile(path)
	if want := "# header\n\n!bang\nfuture-key=\\u0041 x\nmotd=new motd\nmsm-ram=512\n"; string(data) != want {
		t.Fatalf("file = %q, want %q", data, want)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", info.Mode())
	}
	// A new key is appended, nothing else moves.
	if c, err = e.mgr.ConfigSet(context.Background(), "alpha", "server-port", "25566"); err != nil || c.Existed {
		t.Fatalf("append: %+v, %v", c, err)
	}
	data, _ = os.ReadFile(path)
	if !strings.HasSuffix(string(data), "msm-ram=512\nserver-port=25566\n") {
		t.Fatalf("file = %q", data)
	}
}

func TestConfigSetMSMSettings(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "alpha")
	path := writeProps(t, dir, "motd=x\nmsm-stop-delay=3\n")
	ctx := context.Background()
	// The key is canonicalised and its effect classified.
	c, err := e.mgr.ConfigSet(ctx, "alpha", "MSM-Restart-Delay", "20")
	if err != nil || c.Key != "msm-restart-delay" || c.Effect != Immediate || c.Existed {
		t.Fatalf("restart-delay: %+v, %v", c, err)
	}
	if c, err = e.mgr.ConfigSet(ctx, "alpha", "msm-stop-delay", "4"); err != nil || c.Old != "3" || !c.Existed {
		t.Fatalf("stop-delay: %+v, %v", c, err)
	}
	if c, err = e.mgr.ConfigSet(ctx, "alpha", "msm-log-path", "logs/latest.log"); err != nil || c.Effect != OnRestart {
		t.Fatalf("log-path: %+v, %v", c, err)
	}
	data, _ := os.ReadFile(path)
	if want := "motd=x\nmsm-stop-delay=4\nmsm-restart-delay=20\nmsm-log-path=logs/latest.log\n"; string(data) != want {
		t.Fatalf("file = %q, want %q", data, want)
	}
	// An empty value is kept as an explicit unset.
	if _, err = e.mgr.ConfigSet(ctx, "alpha", "msm-restart-delay", ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.mgr.ConfigGet("alpha", "msm-restart-delay"); v != "10" {
		t.Fatalf("an emptied override must fall back to the default, got %q", v)
	}
}

func TestConfigSetRefusals(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "alpha")
	original := "motd=x\n"
	path := writeProps(t, dir, original)
	ctx := context.Background()
	for _, tc := range []struct {
		key, value string
		want       error
	}{
		{"motd", "a\nop-permission-level=4", serverprops.ErrUnsafeProperty},
		{"mo=td", "x", serverprops.ErrUnsafeProperty},
		{"#motd", "x", serverprops.ErrUnsafeProperty},
		{"msm-bogus", "x", ErrUnknownSetting},
		{"msm-ram", "lots", ErrBadSetting},
		{"msm-ram", "0", ErrBadSetting},
		{"msm-stop-delay", "-1", ErrBadSetting},
		{"msm-complete-backup-follow-symlinks", "yes", ErrBadSetting},
		{"msm-invocation", "java -jar {JAR}; rm -rf /", ErrBadSetting},
		{"msm-screen-name", "bad name", ErrBadSetting},
	} {
		_, err := e.mgr.ConfigSet(ctx, "alpha", tc.key, tc.value)
		if !errors.Is(err, tc.want) {
			t.Errorf("ConfigSet(%q, %q) = %v, want %v", tc.key, tc.value, err, tc.want)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Fatalf("a refused change must not touch the file: %q", data)
	}
	if _, err := e.mgr.ConfigSet(ctx, "missing", "motd", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing server: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.ConfigSet(ctx, "alpha", "motd", "x"); !errors.Is(err, ErrNoProperties) {
		t.Errorf("missing file: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("ConfigSet must not create a properties file")
	}
}

func TestConfigSetOfIdentitySettingsNeedsAStoppedServer(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "alpha")
	path := writeProps(t, dir, "motd=x\n")
	ctx := context.Background()
	for _, kind := range []StateKind{Running, Occupied, Unknown} {
		e.prober.states["alpha"] = State{Kind: kind, Detail: "test"}
		for key, value := range map[string]string{"msm-ram": "2048", "msm-screen-name": "other", "msm-jar-path": "other.jar",
			"msm-invocation": "java -jar {JAR}", "msm-username": "someone"} {
			if _, err := e.mgr.ConfigSet(ctx, "alpha", key, value); err == nil || !strings.Contains(err.Error(), "only while the server is stopped") {
				t.Errorf("%v: setting %s = %v", kind, key, err)
			}
		}
		// A property the server reads at start is allowed while it runs, and
		// the change reports the state so the CLI can say a restart is due.
		c, err := e.mgr.ConfigSet(ctx, "alpha", "motd", "y")
		if err != nil || c.State.Kind != kind || c.Effect != OnRestart {
			t.Errorf("%v: motd = %+v, %v", kind, c, err)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != "motd=y\n" {
		t.Fatalf("file = %q", data)
	}
}

func TestEntry(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "alpha")
	e.prober.states["alpha"] = State{Kind: Running}
	got, err := e.mgr.Entry(context.Background(), "alpha")
	if err != nil || got.Name != "alpha" || got.Dir != dir || got.State.Kind != Running || got.Active {
		t.Fatalf("Entry = %+v, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "active"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.mgr.Entry(context.Background(), "alpha"); !got.Active {
		t.Fatal("an active marker means active")
	}
	if _, err := e.mgr.Entry(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestSettingEffectCoversEveryRegisteredSetting(t *testing.T) {
	for _, name := range legacyconf.ServerSettingNames() {
		_ = SettingEffect(name) // classification must never panic
	}
	for name, want := range map[string]Effect{"RAM": WhileStopped, "LOG_PATH": OnRestart, "STOP_DELAY": Immediate, "SCREEN_NAME": WhileStopped} {
		if SettingEffect(name) != want {
			t.Errorf("%s effect = %v", name, SettingEffect(name))
		}
	}
	if KeyEffect("level-name") != OnRestart || KeyEffect("msm-stop-delay") != Immediate || KeyEffect("msm-bogus") != OnRestart {
		t.Error("KeyEffect classification")
	}
}

func TestConfirmSettingsAreIgnoredAndSaySo(t *testing.T) {
	e := newEnv(t, "DEFAULT_CONFIRM_KICK=\"Kicked .*\"\n")
	dir := e.create(t, "alpha")
	writeProps(t, dir, "msm-confirm-save-on=Saved\n")
	list, warnings, err := e.mgr.ConfigList("alpha")
	if err != nil || len(list) == 0 {
		t.Fatal(err)
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0]+warnings[1], "msm-confirm-save-on is set in server.properties but has no effect") ||
		!strings.Contains(warnings[0]+warnings[1], "msm-confirm-kick is set in msm.conf but has no effect") {
		t.Fatalf("warnings = %q", warnings)
	}
	_, err = e.mgr.ConfigSet(context.Background(), "alpha", "msm-confirm-xp", "x")
	if !errors.Is(err, ErrBadSetting) || !strings.Contains(err.Error(), "DEV-034") {
		t.Fatalf("writing a confirm setting = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "server.properties")); string(data) != "msm-confirm-save-on=Saved\n" {
		t.Fatalf("file = %q", data)
	}
}
