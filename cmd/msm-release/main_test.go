package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/update"
)

const commit = "89abcdef0123456789abcdef0123456789abcdef"

func goBuild(t *testing.T, out, version, pub string) {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool to build the program with")
	}
	pkg := "github.com/thehatchcloud/msm/internal/buildinfo"
	ld := "-X " + pkg + ".version=" + version + " -X " + pkg + ".commit=" + commit + " -X " + pkg + ".updateKeys=" + pub
	cmd := exec.Command(goTool, "build", "-trimpath", "-ldflags", ld, "-o", out, "../msm")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
}

// TestRealReleaseUpdatesARealProgram runs the whole chain with the real msm:
// keygen, build with the key and version compiled in, package, verify the
// release directory, then update an older build with the default check that
// runs the staged program.
func TestRealReleaseUpdatesARealProgram(t *testing.T) {
	work := t.TempDir()
	keyFile := filepath.Join(work, "release.key")
	var out bytes.Buffer
	if err := run([]string{"keygen", "-out", keyFile}, &out); err != nil {
		t.Fatal(err)
	}
	pub := strings.TrimSpace(out.String())
	out.Reset()
	if err := run([]string{"pubkey", "-key", keyFile}, &out); err != nil || strings.TrimSpace(out.String()) != pub {
		t.Fatalf("pubkey = %q, %v", out.String(), err)
	}
	if err := run([]string{"keygen", "-out", keyFile}, &out); err == nil {
		t.Error("keygen overwrote an existing key")
	}

	oldBin, newBin := filepath.Join(work, "old-msm"), filepath.Join(work, "new-msm")
	goBuild(t, oldBin, "1.0.0", pub)
	goBuild(t, newBin, "1.1.0", pub)
	license := filepath.Join(work, "LICENSE.markdown")
	if err := os.WriteFile(license, []byte("GPL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(work, "release")
	plat := runtime.GOOS + "/" + runtime.GOARCH
	out.Reset()
	err := run([]string{"package", "-key", keyFile, "-version", "1.1.0", "-commit", commit, "-license", license,
		"-out", rel, "-bin", plat + "=" + newBin}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-pubkey", pub, "-dir", rel}, &out); err != nil {
		t.Fatalf("verify: %v", err)
	}

	keys, _ := update.ParseKeys([]string{pub})
	res, err := update.Run(context.Background(), update.Options{
		Keys: keys, Source: update.DirSource(rel), Current: "1.0.0",
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Exe: oldBin,
	})
	if err != nil || res.Status != update.Updated {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	got, err := exec.Command(oldBin, "version").Output()
	if err != nil || !strings.Contains(string(got), "1.1.0 (commit "+commit+")") {
		t.Errorf("updated program says %q, %v", got, err)
	}
	prev, err := exec.Command(oldBin+".previous", "version").Output()
	if err != nil || !strings.Contains(string(prev), "1.0.0") {
		t.Errorf("previous program says %q, %v", prev, err)
	}

	// A program that reports another version than the one signed (the 1.1.0
	// build, signed here as 1.3.0) is refused by the staged-program check,
	// and the installed program is left alone.
	wrong := newBin
	rel2 := filepath.Join(work, "release2")
	if err := run([]string{"package", "-key", keyFile, "-version", "1.3.0", "-commit", commit, "-license", license,
		"-out", rel2, "-bin", plat + "=" + wrong}, &out); err != nil {
		t.Fatal(err)
	}
	_, err = update.Run(context.Background(), update.Options{
		Keys: keys, Source: update.DirSource(rel2), Current: "1.1.0",
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Exe: oldBin,
	})
	if !errors.Is(err, update.ErrArtifact) {
		t.Errorf("mismatched build = %v, want ErrArtifact", err)
	}
	if got, _ := exec.Command(oldBin, "version").Output(); !strings.Contains(string(got), "1.1.0") {
		t.Errorf("program after refused update says %q", got)
	}
}

func TestVerifyRejectsTamperingAndOtherKeys(t *testing.T) {
	work := t.TempDir()
	keyFile := filepath.Join(work, "k")
	var out bytes.Buffer
	if err := run([]string{"keygen", "-out", keyFile}, &out); err != nil {
		t.Fatal(err)
	}
	pub := strings.TrimSpace(out.String())
	bin := filepath.Join(work, "msm")
	lic := filepath.Join(work, "LICENSE.markdown")
	os.WriteFile(bin, []byte("binary"), 0o755)
	os.WriteFile(lic, []byte("GPL"), 0o644)
	rel := filepath.Join(work, "rel")
	args := []string{"package", "-key", keyFile, "-version", "2.0.0", "-commit", commit, "-license", lic, "-out", rel}
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		args = append(args, "-bin", p+"="+bin)
	}
	if err := run(args, &out); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-pubkey", pub, "-dir", rel}, &out); err != nil {
		t.Fatalf("verify: %v", err)
	}
	m, _ := os.ReadFile(filepath.Join(rel, "manifest.json"))
	for _, p := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		if !strings.Contains(string(m), "msm-2.0.0-"+p+".tar.gz") {
			t.Errorf("manifest lacks %s", p)
		}
	}
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if err := run([]string{"verify", "-pubkey", base64Of(otherPub), "-dir", rel}, &out); !errors.Is(err, update.ErrBadSignature) {
		t.Errorf("other key = %v", err)
	}
	archive := filepath.Join(rel, "msm-2.0.0-linux-arm64.tar.gz")
	b, _ := os.ReadFile(archive)
	b[len(b)-12] ^= 1
	os.WriteFile(archive, b, 0o644)
	if err := run([]string{"verify", "-pubkey", pub, "-dir", rel}, &out); err == nil {
		t.Error("a corrupted archive verified")
	}
}

func base64Of(k ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(k) }
