package install

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
)

func me(t *testing.T) identity.Identity {
	t.Helper()
	id, err := identity.Current()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPrerequisitesReportAndNeverInstall(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not found") }
	for _, goos := range []string{"linux", "darwin"} {
		checks := Prerequisites(goos, missing)
		if len(checks) != 2 {
			t.Fatalf("%s: %d checks", goos, len(checks))
		}
		for _, c := range checks {
			if c.Path != "" || c.Hint == "" {
				t.Errorf("%s %s: %+v", goos, c.Name, c)
			}
		}
	}
	if h := Prerequisites("darwin", missing)[0].Hint; !strings.Contains(h, "brew install screen") {
		t.Errorf("macOS screen hint = %q", h)
	}
	found := Prerequisites("linux", func(n string) (string, error) { return "/usr/bin/" + n, nil })
	if found[0].Path != "/usr/bin/screen" || found[1].Path != "/usr/bin/java" {
		t.Errorf("found = %+v", found)
	}
}

func TestConfigIsReadBackByTheManager(t *testing.T) {
	dir := t.TempDir()
	o := ConfigOptions{
		Path: filepath.Join(dir, "etc", "msm.conf"), Username: me(t).Username,
		Servers: dir + "/data/servers", Jars: dir + "/data/jars", Archives: dir + "/data/archives",
		Lookup: identity.Lookup,
	}
	if err := os.Mkdir(filepath.Join(dir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	created, err := WriteConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) == 0 || created[len(created)-1] != o.Path {
		t.Errorf("created = %v", created)
	}
	for _, d := range []string{o.Servers, o.Jars, o.Archives + "/worlds", o.Archives + "/backups", o.Archives + "/logs"} {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
	f, err := legacyconf.Load(o.Path)
	if err != nil {
		t.Fatal(err)
	}
	g := legacyconf.Global(f)
	if g.Username != o.Username || g.ServerStoragePath != o.Servers || g.JarStoragePath != o.Jars {
		t.Errorf("read back %+v", g)
	}

	// Running it again is a no-op; a hand-edited file is not replaced.
	if created, err := WriteConfig(o); err != nil || created != nil {
		t.Errorf("repeat = %v, %v", created, err)
	}
	if err := os.WriteFile(o.Path, []byte("USERNAME=\"edited\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteConfig(o); !errors.Is(err, ErrExists) {
		t.Errorf("edited file = %v", err)
	}
	if b, _ := os.ReadFile(o.Path); string(b) != "USERNAME=\"edited\"\n" {
		t.Error("edited file was replaced")
	}
	o.Force = true
	if _, err := WriteConfig(o); err != nil {
		t.Errorf("force = %v", err)
	}
}

func TestConfigRejectsUnsafeValuesAndMissingUser(t *testing.T) {
	base := ConfigOptions{Path: "/tmp/x.conf", Username: "minecraft", Servers: "/s", Jars: "/j", Archives: "/a", Lookup: identity.Lookup}
	for name, mod := range map[string]func(*ConfigOptions){
		"quote":    func(o *ConfigOptions) { o.Servers = `/s"; rm -rf /; "` },
		"dollar":   func(o *ConfigOptions) { o.Jars = "/j/$HOME" },
		"newline":  func(o *ConfigOptions) { o.Archives = "/a\nUSERNAME=root" },
		"dotdot":   func(o *ConfigOptions) { o.Archives = "/a/../etc" },
		"relative": func(o *ConfigOptions) { o.Servers = "servers" },
		"user":     func(o *ConfigOptions) { o.Username = "a b" },
	} {
		o := base
		mod(&o)
		if _, err := ConfigText(o); !errors.Is(err, ErrUnsafe) {
			t.Errorf("%s: %v", name, err)
		}
	}
	o := base
	o.Lookup = func(string) (identity.Identity, error) { return identity.Identity{}, errors.New("no such user") }
	if _, err := WriteConfig(o); err == nil || !strings.Contains(err.Error(), "create it first") {
		t.Errorf("missing user = %v", err)
	}
}

func TestSystemdUnitKeepsActiveStateSemantics(t *testing.T) {
	u, err := SystemdUnit(ServiceOptions{Binary: "/usr/local/bin/msm", Conf: "/etc/msm.conf"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Type=oneshot", "RemainAfterExit=yes",
		"ExecStart=/usr/local/bin/msm start\n", "ExecStop=/usr/local/bin/msm stop\n",
		"Environment=MSM_CONF=/etc/msm.conf", "WantedBy=multi-user.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	// "stop now" and "<server> stop" clear or skip intent; the unit must use
	// the global commands that leave the active state alone.
	if strings.Contains(u, "stop now") || strings.Contains(u, "User=") {
		t.Errorf("unexpected content:\n%s", u)
	}
	u, _ = SystemdUnit(ServiceOptions{Binary: "/opt/msm/bin/msm", RunAs: "minecraft"})
	if !strings.Contains(u, "User=minecraft\n") || strings.Contains(u, "MSM_CONF") {
		t.Errorf("RunAs unit:\n%s", u)
	}
	if _, err := SystemdUnit(ServiceOptions{Binary: "/opt/my msm/msm"}); !errors.Is(err, ErrUnsafe) {
		t.Errorf("space in path = %v", err)
	}
	if _, err := SystemdUnit(ServiceOptions{Binary: "/usr/local/bin/msm", Conf: "/etc/%n.conf"}); !errors.Is(err, ErrUnsafe) {
		t.Errorf("specifier in path = %v", err)
	}
}

func TestLaunchdPlistIsWellFormed(t *testing.T) {
	p, err := LaunchdPlist(ServiceOptions{Binary: "/usr/local/bin/msm", Conf: "/etc/msm.conf"})
	if err != nil {
		t.Fatal(err)
	}
	dec := xml.NewDecoder(strings.NewReader(p))
	for {
		if _, err := dec.Token(); err != nil {
			if err != io.EOF {
				t.Fatalf("not well-formed: %v\n%s", err, p)
			}
			break
		}
	}
	for _, want := range []string{"<string>install</string>", "<string>supervise</string>", "<key>RunAtLoad</key>", "<key>ExitTimeOut</key>", LaunchdLabel, "/var/log/msm-service.log"} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	p, _ = LaunchdPlist(ServiceOptions{Binary: "/usr/local/bin/msm", RunAs: "minecraft"})
	if !strings.Contains(p, "<key>UserName</key>") || strings.Contains(p, "/var/log") {
		t.Errorf("RunAs plist:\n%s", p)
	}
}

func TestWriteServiceEnablesOnlyWhenAsked(t *testing.T) {
	text, path, enable, err := Definition("linux", ServiceOptions{Binary: "/usr/local/bin/msm"})
	if err != nil || path != SystemdUnitPath {
		t.Fatalf("Definition = %q, %v", path, err)
	}
	if _, p, e, _ := Definition("darwin", ServiceOptions{Binary: "/usr/local/bin/msm"}); p != LaunchdPlistPath || e[0][0] != "launchctl" || strings.Contains(strings.Join(e[0], " "), "bootstrap") {
		t.Errorf("darwin definition = %q %v (must enable, not bootstrap, which would start servers now)", p, e)
	}
	if _, _, _, err := Definition("plan9", ServiceOptions{}); err == nil {
		t.Error("plan9 has no service")
	}
	dest := filepath.Join(t.TempDir(), "msm.service")
	var ran [][]string
	run := func(name string, args ...string) error {
		ran = append(ran, append([]string{name}, args...))
		return nil
	}

	if wrote, err := WriteService(text, dest, false, enable, nil); err != nil || !wrote || len(ran) != 0 {
		t.Fatalf("write without enable: %v %v %v", wrote, err, ran)
	}
	if b, _ := os.ReadFile(dest); string(b) != text {
		t.Error("content")
	}
	if err := os.WriteFile(dest, []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteService(text, dest, false, nil, nil); !errors.Is(err, ErrExists) {
		t.Errorf("differing file = %v", err)
	}
	if wrote, err := WriteService(text, dest, true, enable, run); err != nil || !wrote {
		t.Fatalf("force: %v %v", wrote, err)
	}
	if len(ran) != 2 || strings.Join(ran[1], " ") != "systemctl enable msm.service" {
		t.Errorf("enable commands = %v", ran)
	}
}
