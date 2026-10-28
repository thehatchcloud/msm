package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

type release struct {
	dir  string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

// newRelease builds a signed release directory for version holding an
// artifact for the host platform whose binary is body.
func newRelease(t *testing.T, version, body string) *release {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := &release{dir: t.TempDir(), pub: pub, priv: priv}
	r.publish(t, version, body)
	return r
}

func (r *release) publish(t *testing.T, version, body string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "msm")
	lic := filepath.Join(t.TempDir(), "LICENSE.markdown")
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lic, []byte("GPL"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := Release{Version: version, Commit: testCommit, OS: runtime.GOOS, Arch: runtime.GOARCH}
	art, err := BuildArchive(r.dir, rel, bin, lic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteManifest(r.dir, version, testCommit, []Artifact{art}, r.priv); err != nil {
		t.Fatal(err)
	}
}

func (r *release) options(t *testing.T, exe, current string) Options {
	return Options{
		Keys: []ed25519.PublicKey{r.pub}, Source: DirSource(r.dir), Current: current,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Exe: exe,
		Check: func(string, Release) error { return nil },
	}
}

func install(t *testing.T, body string) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "msm")
	if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func noLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagePrefix) {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestUpdateReplacesAtomicallyAndKeepsPrevious(t *testing.T) {
	r := newRelease(t, "1.1.0", "new program")
	exe := install(t, "old program")
	data := filepath.Join(filepath.Dir(exe), "worlds")
	if err := os.WriteFile(data, []byte("world data"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), r.options(t, exe, "1.0.0"))
	if err != nil || res.Status != Updated {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if got := read(t, exe); got != "new program" {
		t.Errorf("exe = %q", got)
	}
	if got := read(t, exe+".previous"); got != "old program" {
		t.Errorf("previous = %q", got)
	}
	if info, _ := os.Stat(exe); info.Mode().Perm()&0o111 == 0 {
		t.Errorf("replacement is not executable: %v", info.Mode())
	}
	if got := read(t, data); got != "world data" {
		t.Errorf("neighbouring data changed: %q", got)
	}
	noLeftovers(t, filepath.Dir(exe))

	// Rolling back swaps the two, and rolling back again returns.
	target, err := ResolveTarget(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Rollback(); err != nil {
		t.Fatal(err)
	}
	if read(t, exe) != "old program" || read(t, exe+".previous") != "new program" {
		t.Errorf("rollback: exe=%q previous=%q", read(t, exe), read(t, exe+".previous"))
	}
	noLeftovers(t, filepath.Dir(exe))
}

func TestRollbackWithoutPrevious(t *testing.T) {
	target, err := ResolveTarget(install(t, "only"))
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Rollback(); !errors.Is(err, ErrNoRollback) {
		t.Errorf("Rollback = %v", err)
	}
}

func TestUpToDateAndDowngrade(t *testing.T) {
	r := newRelease(t, "1.0.0", "same")
	exe := install(t, "old")
	res, err := Run(context.Background(), r.options(t, exe, "1.0.0"))
	if err != nil || res.Status != UpToDate || read(t, exe) != "old" {
		t.Errorf("same version: %+v, %v", res, err)
	}
	if _, err := Run(context.Background(), r.options(t, exe, "1.2.0")); !errors.Is(err, ErrDowngrade) {
		t.Errorf("older signed release = %v, want ErrDowngrade", err)
	}
	if read(t, exe) != "old" {
		t.Error("program changed")
	}
}

func TestCheckOnlyChangesNothingAndNeedsNoTarget(t *testing.T) {
	r := newRelease(t, "1.1.0", "new")
	exe := install(t, "old")
	o := r.options(t, exe, "1.0.0")
	o.CheckOnly = true
	o.Exe = filepath.Join(t.TempDir(), "missing")
	res, err := Run(context.Background(), o)
	if err != nil || res.Status != Available || res.Plan.To.String() != "1.1.0" {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	noLeftovers(t, filepath.Dir(exe))
}

func TestInvalidSignatureChangesNothing(t *testing.T) {
	r := newRelease(t, "1.1.0", "new")
	exe := install(t, "old")
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	o := r.options(t, exe, "1.0.0")
	o.Keys = []ed25519.PublicKey{other}
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key = %v", err)
	}
	// A tampered manifest fails the same way, even if it is well formed.
	path := filepath.Join(r.dir, ManifestName)
	b := read(t, path)
	if err := os.WriteFile(path, []byte(strings.Replace(b, "1.1.0", "9.9.9", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), r.options(t, exe, "1.0.0")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered manifest = %v", err)
	}
	if read(t, exe) != "old" {
		t.Error("program changed")
	}
	noLeftovers(t, filepath.Dir(exe))
}

func TestNoKeyOrDevelopmentBuild(t *testing.T) {
	r := newRelease(t, "1.1.0", "new")
	exe := install(t, "old")
	o := r.options(t, exe, "1.0.0")
	o.Keys = nil
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrNoTrustedKey) {
		t.Errorf("no key = %v", err)
	}
	if _, err := Run(context.Background(), r.options(t, exe, "go-port-dev")); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("dev build = %v", err)
	}
}

func TestArtifactTamperingIsRejected(t *testing.T) {
	r := newRelease(t, "1.1.0", "new")
	exe := install(t, "old")
	art := filepath.Join(r.dir, ArchiveName("1.1.0", runtime.GOOS, runtime.GOARCH))
	b, _ := os.ReadFile(art)
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(art, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), r.options(t, exe, "1.0.0")); !errors.Is(err, ErrArtifact) {
		t.Errorf("tampered archive = %v", err)
	}
	if read(t, exe) != "old" {
		t.Error("program changed")
	}
	noLeftovers(t, filepath.Dir(exe))
}

func TestExactPlatformOnly(t *testing.T) {
	r := newRelease(t, "1.1.0", "new")
	o := r.options(t, install(t, "old"), "1.0.0")
	o.GOARCH = "riscv64"
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrNoArtifact) {
		t.Errorf("other arch = %v", err)
	}
}

func TestConfirmDeclined(t *testing.T) {
	r := newRelease(t, "1.1.0", "new")
	exe := install(t, "old")
	o := r.options(t, exe, "1.0.0")
	o.Confirm = func(Plan) (bool, error) { return false, nil }
	res, err := Run(context.Background(), o)
	if err != nil || res.Status != Declined || read(t, exe) != "old" {
		t.Errorf("declined: %+v, %v", res, err)
	}
	noLeftovers(t, filepath.Dir(exe))
}

func TestInterruptionAtEveryStepLeavesAWorkingProgram(t *testing.T) {
	for _, step := range []string{"staged", "checked", "previous-kept"} {
		t.Run(step, func(t *testing.T) {
			r := newRelease(t, "1.1.0", "new")
			exe := install(t, "old")
			o := r.options(t, exe, "1.0.0")
			o.hooks = func(s string) error {
				if s == step {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := Run(context.Background(), o); err == nil {
				t.Fatal("interrupted update succeeded")
			}
			if got := read(t, exe); got != "old" {
				t.Errorf("exe = %q after interruption at %s", got, step)
			}
			noLeftovers(t, filepath.Dir(exe))
			// The next run succeeds from whatever the interruption left.
			if res, err := Run(context.Background(), r.options(t, exe, "1.0.0")); err != nil || res.Status != Updated {
				t.Fatalf("retry = %+v, %v", res, err)
			}
			if read(t, exe) != "new" {
				t.Error("retry did not update")
			}
		})
	}
}

func TestKilledUpdateLeavesStagingFilesThatAreCleaned(t *testing.T) {
	r := newRelease(t, "1.1.0", "new")
	exe := install(t, "old")
	dir := filepath.Dir(exe)
	for _, n := range []string{stagePrefix + "bin-123", stagePrefix + "dl-9", stagePrefix + "prev-1"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("half"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Run(context.Background(), r.options(t, exe, "1.0.0")); err != nil {
		t.Fatal(err)
	}
	noLeftovers(t, dir)
}

func TestFailedCheckKeepsOldProgram(t *testing.T) {
	r := newRelease(t, "1.1.0", "new")
	exe := install(t, "old")
	o := r.options(t, exe, "1.0.0")
	o.Check = func(string, Release) error { return fmt.Errorf("%w: will not run", ErrArtifact) }
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrArtifact) {
		t.Fatalf("Run = %v", err)
	}
	if read(t, exe) != "old" {
		t.Error("program changed")
	}
	if _, err := os.Lstat(exe + ".previous"); err == nil {
		t.Error("previous kept for an update that never happened")
	}
	noLeftovers(t, filepath.Dir(exe))
}

func TestSymlinkedExecutableReplacesTheRealFile(t *testing.T) {
	r := newRelease(t, "1.1.0", "new")
	real := install(t, "old")
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "msm")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), r.options(t, link, "1.0.0")); err != nil {
		t.Fatal(err)
	}
	if dest, err := os.Readlink(link); err != nil || dest != real {
		t.Errorf("symlink = %q, %v; want it kept pointing at %s", dest, err, real)
	}
	if read(t, real) != "new" || read(t, real+".previous") != "old" {
		t.Errorf("real=%q previous=%q", read(t, real), read(t, real+".previous"))
	}
	noLeftovers(t, linkDir)
	noLeftovers(t, filepath.Dir(real))
}

func TestPackageManagedExecutablesAreRefused(t *testing.T) {
	for _, p := range []string{"/usr/bin/true", "/bin/sh"} {
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		if _, err := ResolveTarget(p); !errors.Is(err, ErrPackageManaged) && !strings.HasPrefix(resolved, "/usr/local") {
			t.Errorf("ResolveTarget(%s) = %v", p, err)
		}
	}
	// A symlink into a package manager's tree is judged by where it leads.
	base := t.TempDir()
	cellar := "/opt/homebrew/Cellar/msm/1/bin/msm"
	if _, err := os.Stat(cellar); err == nil {
		t.Skip("a real Homebrew msm exists here")
	}
	if err := os.Symlink(cellar, filepath.Join(base, "msm")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTarget(filepath.Join(base, "msm")); err == nil {
		t.Error("a dangling link into the Cellar was accepted")
	}
}

func TestUnwritableDirectoryIsRefusedBeforeAnyDownload(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	r := newRelease(t, "1.1.0", "new")
	exe := install(t, "old")
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	o := r.options(t, exe, "1.0.0")
	o.Source = failingSource{}
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrPermission) {
		t.Fatalf("Run = %v, want ErrPermission", err)
	}
	if read(t, exe) != "old" {
		t.Error("program changed")
	}
}

func TestConcurrentUpdateIsRefused(t *testing.T) {
	exe := install(t, "old")
	target, err := ResolveTarget(exe)
	if err != nil {
		t.Fatal(err)
	}
	release, err := target.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := target.replace([]byte("x"), nil, nil); !errors.Is(err, ErrBusy) {
		t.Errorf("replace while locked = %v", err)
	}
}

type failingSource struct{}

func (failingSource) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("the network must not be used")
}

func TestArchiveLayoutIsStrict(t *testing.T) {
	want := Release{Version: "1.0.0", Commit: testCommit, OS: "linux", Arch: "amd64"}
	cases := map[string]func(*tar.Writer){
		"extra entry": func(tw *tar.Writer) {
			writeEntry(tw, "../escape", "x", tar.TypeReg)
		},
		"symlink": func(tw *tar.Writer) {
			writeEntry(tw, BinaryName, "", tar.TypeSymlink)
		},
		"wrong release": func(tw *tar.Writer) {
			writeEntry(tw, BinaryName, "b", tar.TypeReg)
			writeEntry(tw, LicenseName, "l", tar.TypeReg)
			writeEntry(tw, ChecksumName, "bad\n", tar.TypeReg)
			writeEntry(tw, ReleaseName, want.text(), tar.TypeReg)
		},
	}
	for name, fill := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a.tar.gz")
			f, _ := os.Create(path)
			zw := gzip.NewWriter(f)
			tw := tar.NewWriter(zw)
			fill(tw)
			tw.Close()
			zw.Close()
			f.Close()
			if _, err := ReadArchive(path, want); !errors.Is(err, ErrArtifact) {
				t.Errorf("ReadArchive = %v", err)
			}
		})
	}
}

func writeEntry(tw *tar.Writer, name, body string, typ byte) {
	h := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: typ}
	if typ == tar.TypeSymlink {
		h.Linkname, h.Size = "/etc/passwd", 0
	}
	tw.WriteHeader(h)
	if typ == tar.TypeReg {
		tw.Write([]byte(body))
	}
}

func TestReleaseIdentifiersMustMatchTheManifest(t *testing.T) {
	r := newRelease(t, "1.1.0", "new")
	rel := Release{Version: "1.1.0", Commit: strings.Repeat("a", 40), OS: runtime.GOOS, Arch: runtime.GOARCH}
	_, err := ReadArchive(filepath.Join(r.dir, ArchiveName("1.1.0", runtime.GOOS, runtime.GOARCH)), rel)
	if !errors.Is(err, ErrArtifact) {
		t.Errorf("mismatched commit = %v", err)
	}
}

func TestSmokeCheck(t *testing.T) {
	rel := Release{Version: "1.1.0", Commit: testCommit}
	script := func(out string) string {
		p := filepath.Join(t.TempDir(), "msm")
		os.WriteFile(p, []byte("#!/bin/sh\necho '"+out+"'\n"), 0o755)
		return p
	}
	good := script("Minecraft Server Manager (Go port) 1.1.0 (commit " + testCommit + ")")
	if err := SmokeCheck(good, rel); err != nil {
		t.Errorf("good program: %v", err)
	}
	if err := SmokeCheck(script("Minecraft Server Manager (Go port) 1.0.0 (commit "+testCommit+")"), rel); !errors.Is(err, ErrArtifact) {
		t.Errorf("wrong version: %v", err)
	}
	if err := SmokeCheck(filepath.Join(t.TempDir(), "absent"), rel); !errors.Is(err, ErrArtifact) {
		t.Errorf("cannot run: %v", err)
	}
}

func TestParseManifestIsStrict(t *testing.T) {
	good := `{"schema":1,"project":"thehatchcloud/msm","version":"1.0.0","commit":"` + testCommit + `","artifacts":[{"os":"linux","arch":"amd64","file":"msm-1.0.0-linux-amd64.tar.gz","sha256":"` + strings.Repeat("a", 64) + `","size":10}]}`
	if _, err := ParseManifest([]byte(good)); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"other project":          strings.Replace(good, "thehatchcloud/msm", "msmhq/msm", 1),
		"unknown field":          strings.Replace(good, `"schema":1`, `"schema":1,"x":1`, 1),
		"path in file":           strings.Replace(good, "msm-1.0.0-linux-amd64.tar.gz", "../msm", 1),
		"short commit":           strings.Replace(good, testCommit, "abc", 1),
		"prerelease":             strings.Replace(good, `"1.0.0"`, `"1.0.0-rc1"`, 1),
		"trailing data":          good + `{}`,
		"zero size":              strings.Replace(good, `"size":10`, `"size":0`, 1),
		"schema from the future": strings.Replace(good, `"schema":1`, `"schema":2`, 1),
	}
	for name, doc := range bad {
		if _, err := ParseManifest([]byte(doc)); !errors.Is(err, ErrManifest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestVersions(t *testing.T) {
	a, _ := ParseVersion("v1.2.3")
	b, _ := ParseVersion("1.10.0")
	if a.Compare(b) >= 0 || b.Compare(a) <= 0 || a.Compare(a) != 0 {
		t.Error("comparison")
	}
	for _, s := range []string{"", "go-port-dev", "1.2", "1.2.3-rc1", "01.2.3"} {
		if _, err := ParseVersion(s); !errors.Is(err, ErrUnknownVersion) {
			t.Errorf("ParseVersion(%q) = %v", s, err)
		}
	}
}

func TestHTTPSourceRequiresHTTPSAndRefusesDowngradeRedirects(t *testing.T) {
	if _, err := NewHTTPSource("http://example.com/releases", nil); err == nil {
		t.Error("an http release location was accepted")
	}
	if _, err := NewHTTPSource("https://user:pw@example.com/", nil); err == nil {
		t.Error("credentials in the release location were accepted")
	}
	r := newRelease(t, "1.1.0", "new")
	mux := http.NewServeMux()
	mux.Handle("/rel/", http.StripPrefix("/rel/", http.FileServer(http.Dir(r.dir))))
	mux.HandleFunc("/plain", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "http://example.com/x", http.StatusFound)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	src, err := NewHTTPSource(srv.URL+"/rel", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	exe := install(t, "old")
	o := r.options(t, exe, "1.0.0")
	o.Source = src
	if res, err := Run(context.Background(), o); err != nil || res.Status != Updated || read(t, exe) != "new" {
		t.Fatalf("Run over https = %+v, %v", res, err)
	}
	redir, _ := NewHTTPSource(srv.URL+"/plain", srv.Client())
	if _, err := redir.Open(context.Background(), "x"); err == nil {
		t.Error("an https to http redirect was followed")
	}
	if _, err := src.Open(context.Background(), "missing.json"); err == nil {
		t.Error("a 404 was not an error")
	}
	if _, err := src.Open(context.Background(), "../etc/passwd"); err == nil {
		t.Error("a path was accepted as a file name")
	}
}
