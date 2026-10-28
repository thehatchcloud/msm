package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/identity"
)

func installFixture(t *testing.T, goos string) *serverFixture {
	t.Helper()
	f := newServerFixture(t)
	f.installEnv = installEnv{
		goos: goos, euid: 1000,
		lookPath:   func(string) (string, error) { return "", errors.New("not found") },
		lookup:     identity.Lookup,
		run:        func(string, ...string) error { t.Error("an external command ran"); return nil },
		executable: func() (string, error) { return "/usr/local/bin/msm", nil },
	}
	return f
}

func TestInstallCheckReportsAndNeverInstalls(t *testing.T) {
	f := installFixture(t, "linux")
	code, out, errOut := f.run(t, "", "install", "check")
	if code != ExitUnavailable || !strings.Contains(out, "missing: screen") || !strings.Contains(out, "sudo apt install screen") || !strings.Contains(errOut, "nothing was installed") {
		t.Errorf("check = %d %q %q", code, out, errOut)
	}
	f.installEnv.lookPath = func(n string) (string, error) { return "/usr/bin/" + n, nil }
	if code, out, _ := f.run(t, "", "install", "check"); code != ExitOK || !strings.Contains(out, "ok: java found at /usr/bin/java") {
		t.Errorf("check with tools = %d %q", code, out)
	}
	m := installFixture(t, "darwin")
	if _, out, _ := m.run(t, "", "install", "check"); !strings.Contains(out, "brew install screen") {
		t.Errorf("macOS check = %q", out)
	}
}

func TestInstallConfigWritesAFileTheManagerReads(t *testing.T) {
	f := installFixture(t, "linux")
	dir := t.TempDir()
	conf := filepath.Join(dir, "msm.conf")
	code, out, errOut := f.run(t, "", "install", "config", "--conf", conf,
		"--servers", dir+"/s", "--jars", dir+"/j", "--archives", dir+"/a")
	if code != ExitOK || !strings.Contains(out, "created "+conf) {
		t.Fatalf("config = %d %q %q", code, out, errOut)
	}
	t.Setenv("MSM_CONF", conf)
	if code, out, errOut := f.run(t, "", "config"); code != ExitOK || !strings.Contains(out, `SERVER_STORAGE_PATH="`+dir+`/s"`) {
		t.Errorf("msm config = %d %q %q", code, out, errOut)
	}
	if code, out, _ := f.run(t, "", "install", "config", "--conf", conf, "--servers", dir+"/s", "--jars", dir+"/j", "--archives", dir+"/a"); code != ExitOK || !strings.Contains(out, "nothing was changed") {
		t.Errorf("repeat = %d %q", code, out)
	}
	if err := os.WriteFile(conf, []byte("USERNAME=\"edited\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := f.run(t, "", "install", "config", "--conf", conf, "--servers", dir+"/s", "--jars", dir+"/j", "--archives", dir+"/a"); code != ExitRefused || !strings.Contains(errOut, "--force") {
		t.Errorf("edited = %d %q", code, errOut)
	}
	if code, _, errOut := f.run(t, "", "install", "config", "--conf", conf, "--servers", `/x"; touch /tmp/y; "`); code != ExitConfig {
		t.Errorf("unsafe path = %d %q", code, errOut)
	}
}

func TestInstallConfigWithoutRootNeedsAnExplicitFile(t *testing.T) {
	f := installFixture(t, "linux")
	t.Setenv("MSM_CONF", "")
	if code, _, errOut := f.run(t, "", "install", "config"); code != ExitUsage || !strings.Contains(errOut, "--conf") {
		t.Errorf("config as non-root = %d %q", code, errOut)
	}
}

func TestInstallServicePrintsByDefault(t *testing.T) {
	f := installFixture(t, "linux")
	code, out, errOut := f.run(t, "", "install", "service", "--conf", "/etc/msm.conf")
	if code != ExitOK || !strings.Contains(out, "ExecStart=/usr/local/bin/msm start") || !strings.Contains(out, "Environment=MSM_CONF=/etc/msm.conf") ||
		!strings.Contains(errOut, "/etc/systemd/system/msm.service") {
		t.Errorf("service = %d %q %q", code, out, errOut)
	}
	m := installFixture(t, "darwin")
	if code, out, _ := m.run(t, "", "install", "service"); code != ExitOK || !strings.Contains(out, "<string>supervise</string>") {
		t.Errorf("macOS service = %d %q", code, out)
	}
	if code, _, errOut := f.run(t, "", "install", "service", "--enable"); code != ExitUsage || !strings.Contains(errOut, "--write") {
		t.Errorf("--enable alone = %d %q", code, errOut)
	}
	if code, _, _ := f.run(t, "", "install", "service", "--binary", "/opt/my msm/msm"); code != ExitConfig {
		t.Errorf("unsafe binary = %d", code)
	}
}

func TestInstallServiceWriteAndExplicitEnable(t *testing.T) {
	f := installFixture(t, "linux")
	dest := filepath.Join(t.TempDir(), "msm.service")
	code, out, errOut := f.run(t, "", "install", "service", "--write", "--path", dest)
	if code != ExitOK || !strings.Contains(out, "Not enabled") || !strings.Contains(out, "systemctl enable msm.service") {
		t.Fatalf("write = %d %q %q", code, out, errOut)
	}
	if b, err := os.ReadFile(dest); err != nil || !strings.Contains(string(b), "ExecStop=/usr/local/bin/msm stop") {
		t.Errorf("file = %q, %v", b, err)
	}
	var ran []string
	f.installEnv.run = func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}
	if code, out, _ := f.run(t, "", "install", "service", "--write", "--enable", "--path", dest); code != ExitOK || !strings.Contains(out, "Nothing was started") {
		t.Errorf("enable = %d %q", code, out)
	}
	if len(ran) != 2 || ran[1] != "systemctl enable msm.service" {
		t.Errorf("commands = %v", ran)
	}
	if err := os.WriteFile(dest, []byte("hand edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := f.run(t, "", "install", "service", "--write", "--path", dest); code != ExitRefused || !strings.Contains(errOut, "--force") {
		t.Errorf("differing file = %d %q", code, errOut)
	}
}

func TestInstallServiceUserMustMatchTheConfiguration(t *testing.T) {
	f := installFixture(t, "linux")
	me, _ := identity.Current()
	if code, out, errOut := f.run(t, "", "install", "service", "--user", me.Username+"x"); code != ExitConfig {
		t.Errorf("mismatched user = %d %q %q", code, out, errOut)
	}
	if code, out, _ := f.run(t, "", "install", "service", "--user", me.Username); code != ExitOK || !strings.Contains(out, "User="+me.Username) {
		t.Errorf("matching user = %d %q", code, out)
	}
}

func TestInstallIsAReservedServerName(t *testing.T) {
	f := newServerFixture(t)
	if code, _, _ := f.run(t, "", "server", "create", "install"); code == ExitOK {
		t.Error("a server named install was created")
	}
}

func TestSuperviseStaysUntilTerminatedThenStopsServers(t *testing.T) {
	f := installFixture(t, "darwin")
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(300 * time.Millisecond)
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
	start := time.Now()
	code, _, errOut := f.run(t, "", "install", "supervise")
	<-done
	if code != ExitOK || time.Since(start) < 250*time.Millisecond {
		t.Errorf("supervise = %d after %v: %q", code, time.Since(start), errOut)
	}
}
