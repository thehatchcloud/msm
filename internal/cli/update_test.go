package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/buildinfo"
	updater "github.com/thehatchcloud/msm/internal/update"
)

const updateCommit = "0123456789abcdef0123456789abcdef01234567"

// updateFixture is a signed release directory and an installed program.
type updateFixture struct {
	*serverFixture
	exe  string
	dir  string
	priv ed25519.PrivateKey
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &updateFixture{serverFixture: newServerFixture(t), dir: t.TempDir(), priv: priv}
	f.exe = filepath.Join(t.TempDir(), "msm")
	if err := os.WriteFile(f.exe, []byte("old program"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.info = buildinfo.Info{Version: "1.0.0", Commit: "old", UpdateKeys: []string{base64.StdEncoding.EncodeToString(pub)}, UpdateURL: "https://example.invalid/releases"}
	f.updateEnv = updateEnv{
		source:     func(buildinfo.Info) (updater.Source, error) { return updater.DirSource(f.dir), nil },
		executable: func() (string, error) { return f.exe, nil },
		goos:       runtime.GOOS, goarch: runtime.GOARCH,
		check: func(string, updater.Release) error { return nil },
	}
	f.publish(t, "1.1.0", "new program")
	return f
}

func (f *updateFixture) publish(t *testing.T, version, body string) {
	t.Helper()
	work := t.TempDir()
	bin, lic := filepath.Join(work, "msm"), filepath.Join(work, "LICENSE.markdown")
	for p, c := range map[string]string{bin: body, lic: "GPL"} {
		if err := os.WriteFile(p, []byte(c), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	art, err := updater.BuildArchive(f.dir, updater.Release{Version: version, Commit: updateCommit, OS: runtime.GOOS, Arch: runtime.GOARCH}, bin, lic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updater.WriteManifest(f.dir, version, updateCommit, []updater.Artifact{art}, f.priv); err != nil {
		t.Fatal(err)
	}
}

func (f *updateFixture) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdateRecognisedFlagsAndArity(t *testing.T) {
	f := newServerFixture(t)
	if code, _, errOut := f.run(t, "", "update", "--unknown"); code != ExitUsage {
		t.Errorf("update --unknown = %d %q", code, errOut)
	}
	if code, _, errOut := f.run(t, "", "update", "now"); code != ExitUsage || !strings.HasPrefix(errOut, "No such command") {
		t.Errorf("update now = %d %q", code, errOut)
	}
	if code, _, _ := f.run(t, "", "update", "--check", "--rollback"); code != ExitUsage {
		t.Errorf("--check --rollback = %d", code)
	}
}

func TestDevelopmentBuildCannotUpdate(t *testing.T) {
	f := newServerFixture(t)
	for _, args := range [][]string{{"update"}, {"update", "--noinput"}, {"update", "--check"}} {
		code, out, errOut := f.run(t, "", args...)
		if code != ExitUnavailable || out != "" || !strings.Contains(errOut, "no trusted release signing key") {
			t.Errorf("msm %v = %d %q %q", args, code, out, errOut)
		}
	}
}

func TestUpdateNeedsConfirmationWithoutATerminal(t *testing.T) {
	f := newUpdateFixture(t)
	code, out, errOut := f.run(t, "", "update")
	if code != ExitRefused || out != "" || !strings.Contains(errOut, "--noinput") {
		t.Errorf("update = %d %q %q", code, out, errOut)
	}
	if f.installed(t) != "old program" {
		t.Error("program changed")
	}
}

func TestUpdateWithNoinputThenRollback(t *testing.T) {
	f := newUpdateFixture(t)
	code, out, errOut := f.run(t, "", "update", "--noinput")
	if code != ExitOK || !strings.Contains(out, "Updated msm from 1.0.0 to 1.1.0") || !strings.Contains(out, "not touched") {
		t.Fatalf("update = %d %q %q", code, out, errOut)
	}
	if f.installed(t) != "new program" {
		t.Errorf("installed = %q", f.installed(t))
	}
	if code, out, _ := f.run(t, "", "update", "--rollback"); code != ExitOK || !strings.Contains(out, "Restored the previous msm") || f.installed(t) != "old program" {
		t.Errorf("rollback = %d %q installed %q", code, out, f.installed(t))
	}
}

func TestUpdatePromptsOnATerminal(t *testing.T) {
	f := newUpdateFixture(t)
	f.terminal = true
	code, out, _ := f.run(t, "n\n", "update")
	if code != ExitOK || !strings.Contains(out, "[y/N]") || !strings.Contains(out, "declined") || f.installed(t) != "old program" {
		t.Errorf("no = %d %q installed %q", code, out, f.installed(t))
	}
	if code, _, _ := f.run(t, "yes\n", "update"); code != ExitOK || f.installed(t) != "new program" {
		t.Errorf("yes = %d installed %q", code, f.installed(t))
	}
	f.info.Version = "1.1.0" // the running program is now the new one
	if code, out, _ := f.run(t, "", "update"); code != ExitOK || !strings.Contains(out, "up to date") {
		t.Errorf("again = %d %q", code, out)
	}
}

func TestUpdateCheck(t *testing.T) {
	f := newUpdateFixture(t)
	code, out, _ := f.run(t, "", "update", "--check")
	if code != ExitOK || !strings.Contains(out, "1.1.0 is available") || f.installed(t) != "old program" {
		t.Errorf("check = %d %q", code, out)
	}
}

func TestUpdateRefusals(t *testing.T) {
	f := newUpdateFixture(t)
	// A release that is not signed by this build's key.
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	f.info.UpdateKeys = []string{base64.StdEncoding.EncodeToString(other)}
	code, _, errOut := f.run(t, "", "update", "--noinput")
	if code != ExitError || !strings.Contains(errOut, "signature") || f.installed(t) != "old program" {
		t.Errorf("bad signature = %d %q", code, errOut)
	}

	// A package manager's executable.
	g := newUpdateFixture(t)
	g.updateEnv.executable = func() (string, error) { return "/usr/bin/true", nil }
	if code, _, errOut := g.run(t, "", "update", "--noinput"); code != ExitRefused || !strings.Contains(errOut, "package manager") {
		t.Errorf("package-managed = %d %q", code, errOut)
	}

	// An older signed release is not an update.
	h := newUpdateFixture(t)
	h.info.Version = "2.0.0"
	if code, _, errOut := h.run(t, "", "update", "--noinput"); code != ExitRefused || !strings.Contains(errOut, "older") {
		t.Errorf("downgrade = %d %q", code, errOut)
	}

	// A version that is not a release version.
	i := newUpdateFixture(t)
	i.info.Version = "go-port-dev"
	if code, _, errOut := i.run(t, "", "update", "--noinput"); code != ExitUnavailable || !strings.Contains(errOut, "development build") {
		t.Errorf("dev version = %d %q", code, errOut)
	}
}
