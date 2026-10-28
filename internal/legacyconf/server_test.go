package legacyconf

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/serverprops"
	"github.com/thehatchcloud/msm/internal/testutil"
)

func TestServerSettingsMatchInventory(t *testing.T) {
	data, err := os.ReadFile("../../compatibility/settings.tsv")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if cols := strings.Split(line, "\t"); cols[0] == "server" {
			want = append(want, cols[1]+"="+cols[2])
		}
	}
	var got []string
	for _, s := range serverSettings {
		got = append(got, s.Name+"="+s.Default)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("server settings differ from compatibility/settings.tsv:\ngot  %q\nwant %q", got, want)
	}
}

// The inert fixture installation proves effective values end to end:
// msm.conf, per-server overrides, and legacy post-processing.
func TestResolveServerFixtures(t *testing.T) {
	root := testutil.CopyFixture(t, "../../compatibility/fixtures/minimal")
	global, err := Load(filepath.Join(root, "msm.conf"))
	if err != nil {
		t.Fatal(err)
	}
	storage := Global(global).ServerStoragePath
	resolve := func(name string) *ServerSettings {
		t.Helper()
		dir := filepath.Join(storage, name)
		props, err := serverprops.Load(filepath.Join(dir, "server.properties"))
		if err != nil {
			t.Fatal(err)
		}
		s, err := ResolveServer(ServerInput{Name: name, Dir: dir, Global: global, Properties: props})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	active := resolve("active-example")
	dir := filepath.Join(storage, "active-example")
	for name, want := range map[string]string{
		"USERNAME":           "minecraft",
		"SCREEN_NAME":        "msm-active-example",
		"VERSION":            "minecraft/1.7.0",
		"RAM":                "512",
		"STOP_DELAY":         "3",
		"MESSAGE_STOP":       "Fixture stopping in 3 seconds",
		"MESSAGE_RESTART":    "SERVER REBOOT IN 10 SECONDS!",
		"JAR_PATH":           filepath.Join(dir, "server.jar"),
		"WORLD_STORAGE_PATH": filepath.Join(dir, "worldstorage"),
		"FLAG_ACTIVE_PATH":   filepath.Join(dir, "active"),
		"INVOCATION":         "java -Xms512M -Xmx512M -jar " + filepath.Join(dir, "server.jar") + " nogui",
	} {
		if got := active.Get(name); got != want {
			t.Errorf("active-example %s = %q, want %q", name, got, want)
		}
	}
	if active.Source("RAM") != FromServer || active.Source("RESTART_DELAY") != FromDefault {
		t.Errorf("sources: RAM from %s, RESTART_DELAY from %s", active.Source("RAM"), active.Source("RESTART_DELAY"))
	}
	if len(active.Warnings) != 0 {
		t.Errorf("warnings: %q", active.Warnings)
	}

	inactive := resolve("inactive-example")
	if got := inactive.Get("RAM"); got != "1024" {
		t.Errorf("inactive-example RAM = %q, want the built-in default", got)
	}
	if got := inactive.Get("MESSAGE_STOP"); got != "SERVER SHUTTING DOWN IN 10 SECONDS!" {
		t.Errorf("inactive-example MESSAGE_STOP = %q", got)
	}
	if got, want := inactive.Get("WORLD_STORAGE_INACTIVE_PATH"), filepath.Join(storage, "inactive-example", "worldstorage_inactive"); got != want {
		t.Errorf("inactive-example WORLD_STORAGE_INACTIVE_PATH = %q, want %q", got, want)
	}
}

func TestResolveServerPrecedence(t *testing.T) {
	global, err := Load(write(t, `DEFAULT_RAM="2048"
default_stop_delay=30
DEFAULT_RESTART_DELAY="45"
DEFAULT_RESTART_DELAY=""
DEFAULT_USERNAME="servers"
DEFAULT_LOG_PATH="/var/log/msm.log"
USERNAME="manager"
`))
	if err != nil {
		t.Fatal(err)
	}
	props := serverprops.Parse([]byte(`msm-ram=4096
MSM-Screen-Name='mc-{SERVER_NAME}'
msm-stop-delay=
msm-jar-path=/srv/jars/custom.jar
msm-world-storage-path=/
msm-unknown-thing=1
`))
	s, err := ResolveServer(ServerInput{Name: "survival", Dir: "/srv/mc/survival/", Global: global, Properties: props,
		Profile: map[string]string{"LOG_PATH": "logs/latest.log", "OPS_PATH": ""}})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]struct {
		value  string
		source Source
	}{
		// The server override beats msm.conf.
		"RAM": {"4096", FromServer},
		// Keys match case-insensitively and quotes are stripped.
		"SCREEN_NAME": {"mc-survival", FromServer},
		// An empty override falls through; msm.conf keys match in any case.
		"STOP_DELAY": {"30", FromGlobal},
		// The last msm.conf line is empty, so the built-in default applies.
		"RESTART_DELAY": {"10", FromDefault},
		// A server's owner comes from DEFAULT_USERNAME, never the manager's
		// USERNAME.
		"USERNAME": {"servers", FromGlobal},
		// The version profile beats msm.conf; an empty one does not count.
		"LOG_PATH": {"/srv/mc/survival/logs/latest.log", FromProfile},
		"OPS_PATH": {"/srv/mc/survival/ops.json", FromDefault},
		// Absolute paths are kept; "/" alone is relative to init/msm.
		"JAR_PATH":           {"/srv/jars/custom.jar", FromServer},
		"WORLD_STORAGE_PATH": {"/srv/mc/survival", FromServer},
		"INVOCATION":         {"java -Xms4096M -Xmx4096M -jar /srv/jars/custom.jar nogui", FromDefault},
		"MESSAGE_STOP":       {"SERVER SHUTTING DOWN IN 30 SECONDS!", FromDefault},
	} {
		if got, src := s.Get(name), s.Source(name); got != want.value || src != want.source {
			t.Errorf("%s = %q from %s, want %q from %s", name, got, src, want.value, want.source)
		}
	}
	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "msm-unknown-thing") {
		t.Errorf("warnings = %q", s.Warnings)
	}
}

func TestResolveServerWithoutLegacyConfiguration(t *testing.T) {
	s, err := ResolveServer(ServerInput{Name: "fresh", Dir: "/data/fresh"})
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range serverSettings {
		if s.Source(setting.Name) != FromDefault {
			t.Errorf("%s from %s", setting.Name, s.Source(setting.Name))
		}
	}
	if s.Username() != "minecraft" || s.Get("SCREEN_NAME") != "msm-fresh" {
		t.Fatalf("username %q, screen name %q", s.Username(), s.Get("SCREEN_NAME"))
	}
}

func TestResolveServerRejectsBadInput(t *testing.T) {
	for _, in := range []ServerInput{
		{Name: "../escape", Dir: "/srv/x"},
		{Name: "start", Dir: "/srv/x"},
		{Name: "ok", Dir: "relative/dir"},
	} {
		if _, err := ResolveServer(in); err == nil {
			t.Errorf("ResolveServer(%+v) accepted bad input", in)
		}
	}
	s, _ := ResolveServer(ServerInput{Name: "ok", Dir: "/srv/ok"})
	defer func() {
		if recover() == nil {
			t.Error("Get of an unregistered setting must panic")
		}
	}()
	s.Get("NOT_A_SETTING")
}

func TestServerOwner(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	props := serverprops.Parse([]byte("msm-username=" + me.Username + "\n"))
	s, err := ResolveServer(ServerInput{Name: "mine", Dir: "/srv/mine", Properties: props})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Owner()
	if err != nil || owner.Username != me.Username || owner.UID != os.Getuid() {
		t.Fatalf("Owner() = %+v, %v; want the current user", owner, err)
	}

	missing := serverprops.Parse([]byte("msm-username=no-such-user-msm-test\n"))
	s, _ = ResolveServer(ServerInput{Name: "orphan", Dir: "/srv/orphan", Properties: missing})
	if _, err := s.Owner(); !errors.Is(err, ErrNoOwner) || !strings.Contains(err.Error(), "server.properties") {
		t.Fatalf("Owner() of a missing user = %v", err)
	}
}

func TestGlobalSettingsMatchInventory(t *testing.T) {
	data, err := os.ReadFile("../../compatibility/settings.tsv")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if cols := strings.Split(line, "\t"); cols[0] == "global" {
			want = append(want, cols[1]+"="+cols[2])
		}
	}
	var got []string
	for _, s := range globalSettings {
		got = append(got, s.Name+"="+s.Default)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("global settings differ from compatibility/settings.tsv:\ngot  %q\nwant %q", got, want)
	}
	for name := range goDefaults {
		found := false
		for _, s := range globalSettings {
			found = found || s.Name == name
		}
		if !found {
			t.Fatalf("goDefaults names %s, which is not a global setting", name)
		}
	}
}

func TestGlobalEntries(t *testing.T) {
	f := &File{Path: "msm.conf", Values: map[string]Assignment{
		"RAM_X":                 {Value: "ignored", Line: 1},
		"JAR_STORAGE_PATH":      {Value: "/srv/jars", Line: 2},
		"DEFAULT_RAM":           {Value: "2048", Line: 3},
		"RDIFF_BACKUP_ROTATION": {Value: "", Line: 4},
	}}
	got := map[string]ConfigEntry{}
	var order []string
	for _, e := range GlobalEntries(f, map[string]string{"SERVER_STORAGE_PATH": "/data/servers"}) {
		got[e.Name] = e
		order = append(order, e.Name)
	}
	if len(order) != len(globalSettings)+len(serverSettings) || order[0] != "DEBUG" || order[len(globalSettings)] != "DEFAULT_USERNAME" {
		t.Fatalf("unexpected order %q", order)
	}
	for name, want := range map[string]ConfigEntry{
		"JAR_STORAGE_PATH":        {"JAR_STORAGE_PATH", "/srv/jars", "msm.conf"},
		"SERVER_STORAGE_PATH":     {"SERVER_STORAGE_PATH", "/data/servers", "detected"},
		"DEFAULT_RAM":             {"DEFAULT_RAM", "2048", "msm.conf"},
		"DEFAULT_STOP_DELAY":      {"DEFAULT_STOP_DELAY", "10", "default"},
		"RDIFF_BACKUP_ROTATION":   {"RDIFF_BACKUP_ROTATION", "7", "default"},
		"RAMDISK_STORAGE_ENABLED": {"RAMDISK_STORAGE_ENABLED", "false", "default"},
	} {
		if got[name] != want {
			t.Errorf("%s = %+v, want %+v", name, got[name], want)
		}
	}
	if nil2 := GlobalEntries(nil, nil); len(nil2) != len(order) || nil2[1].Value != "minecraft" {
		t.Fatalf("no file: %v", nil2[:2])
	}
}

func TestSettingFromKey(t *testing.T) {
	for key, want := range map[string][3]any{
		"msm-stop-delay": {"STOP_DELAY", true, true},
		"MSM-RAM":        {"RAM", true, true},
		"msm-bogus":      {"BOGUS", true, false},
		"motd":           {"", false, false},
	} {
		name, ok, reg := SettingFromKey(key)
		if name != want[0] || ok != want[1] || reg != want[2] {
			t.Errorf("SettingFromKey(%q) = %q %v %v, want %v", key, name, ok, reg, want)
		}
	}
}
