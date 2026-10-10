package jars

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/servers"
	"github.com/thehatchcloud/msm/internal/testutil"
)

var bg = context.Background()

type stopped struct{}

func (stopped) Probe(context.Context, *legacyconf.ServerSettings, identity.Identity) servers.State {
	return servers.State{Kind: servers.Stopped}
}

// jarBytes is a minimal file that passes the JAR (ZIP) signature check.
func jarBytes(content string) []byte { return []byte("PK\x03\x04" + content) }

type env struct {
	t       *testing.T
	store   string
	servers *servers.Manager
	srv     *httptest.Server
	mux     *http.ServeMux
	clock   *testutil.Clock
	m       *Manager
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{t: t, store: filepath.Join(base, "jars"), mux: http.NewServeMux(),
		clock: testutil.NewClock(time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local))}
	root := filepath.Join(base, "servers")
	for _, d := range []string{root, e.store} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	me := identity.Identity{Username: "tester", UID: os.Geteuid(), GID: os.Getegid()}
	euid := me.UID
	var err error
	e.servers, err = servers.New(servers.Config{Root: root, PropertiesFile: "server.properties",
		Owner: me, DefaultOwner: &me, Prober: stopped{}, EUID: &euid})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewTLSServer(e.mux)
	t.Cleanup(e.srv.Close)
	e.m, err = New(Config{Root: e.store, Target: "target.txt", DownloadDir: "downloads", Owner: me,
		Servers: e.servers, HTTP: e.srv.Client(), ManifestURL: e.srv.URL + "/manifest.json",
		MaxBytes: 1 << 20, Clock: e.clock, EUID: &euid})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) serve(path string, body []byte) {
	e.mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
}

func (e *env) group(name, target string) string {
	e.t.Helper()
	if err := e.m.Create(name, target); err != nil {
		e.t.Fatalf("create %s: %v", name, err)
	}
	return filepath.Join(e.store, name)
}

func (e *env) server(name string) string {
	e.t.Helper()
	c, err := e.servers.Create(bg, name)
	if err != nil {
		e.t.Fatal(err)
	}
	return c.Dir
}

// files lists a directory's entries, ignoring the registry lock.
func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != ".msm-registry.lock" {
			names = append(names, e.Name())
		}
	}
	return names
}

// CT-CMD-013, CT-CMD-014, CT-CMD-017, CT-SURF-012: groups keep their target
// in target.txt; only safe names and https or minecraft targets are taken.
func TestCreateListChangeURL(t *testing.T) {
	e := newEnv(t)
	dir := e.group("minecraft", "minecraft")
	e.group("paper", "https://example.com/paper.jar")
	if data, _ := os.ReadFile(filepath.Join(dir, "target.txt")); string(data) != "minecraft\n" {
		t.Fatalf("target.txt = %q", data)
	}
	if err := e.m.Create("minecraft", "minecraft"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := e.m.Create("MINECRAFT", "minecraft"); !errors.Is(err, ErrExists) {
		t.Fatalf("case duplicate: %v", err)
	}
	for _, bad := range []string{"start", "all", "jargroup", "name with spaces", "--x", "../up", ""} {
		if err := e.m.Create(bad, "minecraft"); !errors.Is(err, ErrInvalidName) {
			t.Errorf("name %q: %v", bad, err)
		}
	}
	for target, want := range map[string]error{
		"http://dl.bukkit.org/latest-rb/craftbukkit.jar": ErrInsecureURL,
		"ftp://example.com/x.jar":                        ErrInvalidTarget,
		"https://":                                       ErrInvalidTarget,
		"https://example.com/a b.jar":                    ErrInvalidTarget,
		"":                                               ErrInvalidTarget,
		"file:///etc/passwd":                             ErrInvalidTarget,
	} {
		if err := e.m.Create("other", target); !errors.Is(err, want) {
			t.Errorf("target %q: %v, want %v", target, err, want)
		}
	}
	os.WriteFile(filepath.Join(dir, "2026-01-02-03-04-05-minecraft_server.1.21.jar"), jarBytes("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "custom.jar"), jarBytes("c"), 0o644)
	groups, err := e.m.List()
	if err != nil || len(groups) != 2 || groups[0].Name != "minecraft" || !slices.Equal(groups[0].JARs, []string{"2026-01-02-03-04-05-minecraft_server.1.21.jar"}) {
		t.Fatalf("list = %+v, %v", groups, err)
	}

	if err := e.m.ChangeURL("paper", "https://example.com/new.jar"); err != nil {
		t.Fatal(err)
	}
	if tg, err := e.m.Target("paper"); err != nil || tg.Raw != "https://example.com/new.jar" {
		t.Fatalf("target = %+v, %v", tg, err)
	}
	if err := e.m.ChangeURL("paper", "http://example.com/x.jar"); !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("changeurl http: %v", err)
	}
	if err := e.m.ChangeURL("missing", "minecraft"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("changeurl missing group: %v", err)
	}
	os.Remove(filepath.Join(e.store, "paper", "target.txt"))
	if err := e.m.ChangeURL("paper", "minecraft"); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("changeurl without target.txt: %v", err)
	}
}

// CT-CMD-018, CT-SURF-011, CT-SURF-013: a URL download is staged in the
// download directory, saved with a timestamp and a safe name, and an
// unchanged download is discarded.
func TestGetLatestURL(t *testing.T) {
	e := newEnv(t)
	body := jarBytes("v1")
	e.mux.HandleFunc("/get", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../etc/evil name;rm.jar"`)
		w.Write(body)
	})
	e.mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/files/paper-1.21.jar", http.StatusFound)
	})
	e.serve("/files/paper-1.21.jar", jarBytes("paper"))
	dir := e.group("g", e.srv.URL+"/get")

	d, err := e.m.GetLatest(bg, "g")
	want := filepath.Join(dir, "2026-10-10-12-00-00-evil_name_rm.jar")
	if err != nil || d.Path != want || d.Previous != "" || d.Unchanged {
		t.Fatalf("first download = %+v, %v", d, err)
	}
	if got := files(t, dir); !slices.Equal(got, []string{"2026-10-10-12-00-00-evil_name_rm.jar", "target.txt"}) {
		t.Fatalf("group = %q (the download directory must be gone)", got)
	}

	e.clock.Advance(time.Hour)
	if d, err := e.m.GetLatest(bg, "g"); err != nil || !d.Unchanged || d.Path != want {
		t.Fatalf("same version = %+v, %v", d, err)
	}
	body = jarBytes("v2")
	d, err = e.m.GetLatest(bg, "g")
	if err != nil || d.Previous != want || d.Path != filepath.Join(dir, "2026-10-10-13-00-00-evil_name_rm.jar") {
		t.Fatalf("new version = %+v, %v", d, err)
	}

	// Redirects are followed; the name comes from the final URL.
	dir = e.group("paper", e.srv.URL+"/redirect")
	if d, err := e.m.GetLatest(bg, "paper"); err != nil || d.Path != filepath.Join(dir, "2026-10-10-13-00-00-paper-1.21.jar") {
		t.Fatalf("redirected = %+v, %v", d, err)
	}
}

// Failed, oversized, interrupted, insecure and invalid downloads leave the
// group exactly as it was.
func TestGetLatestFailures(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(jarBytes("plain")) }))
	defer plain.Close()
	handlers := map[string]http.HandlerFunc{
		"notfound": http.NotFound,
		"html":     func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>not a jar</html>")) },
		"declared-large": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(2<<20))
			w.Write(jarBytes(strings.Repeat("x", 100)))
		},
		"streamed-large": func(w http.ResponseWriter, _ *http.Request) {
			w.(http.Flusher).Flush()
			w.Write(jarBytes(strings.Repeat("x", 2<<20)))
		},
		"interrupted": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			w.Write(jarBytes("partial"))
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		},
		"to-http": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, plain.URL+"/x.jar", http.StatusFound)
		},
		"loop": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, r.URL.Path, http.StatusFound) },
	}
	want := map[string]error{
		"notfound": ErrDownload, "html": ErrInvalidJAR, "declared-large": ErrTooLarge,
		"streamed-large": ErrTooLarge, "interrupted": ErrDownload, "to-http": ErrInsecureURL, "loop": ErrDownload,
	}
	e := newEnv(t)
	for name, h := range handlers {
		e.mux.HandleFunc("/"+name, h)
	}
	for name := range handlers {
		dir := e.group(name, e.srv.URL+"/"+name)
		known := filepath.Join(dir, "2026-01-01-00-00-00-known.jar")
		os.WriteFile(known, jarBytes("known"), 0o644)
		_, err := e.m.GetLatest(bg, name)
		if !errors.Is(err, want[name]) {
			t.Errorf("%s: %v, want %v", name, err, want[name])
		}
		if got := files(t, dir); !slices.Equal(got, []string{filepath.Base(known), "target.txt"}) {
			t.Errorf("%s: group changed to %q", name, got)
		}
	}

	// A target file naming several URLs is refused, as several downloads were.
	dir := e.group("multi", e.srv.URL+"/html")
	os.WriteFile(filepath.Join(dir, "target.txt"), []byte("https://a/x.jar\nhttps://b/y.jar\n"), 0o644)
	if _, err := e.m.GetLatest(bg, "multi"); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("multiple URLs: %v", err)
	}
	// A legacy http target fails at download time.
	os.WriteFile(filepath.Join(dir, "target.txt"), []byte("http://dl.bukkit.org/craftbukkit.jar\n"), 0o644)
	if _, err := e.m.GetLatest(bg, "multi"); !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("legacy http target: %v", err)
	}
	// What an interrupted run left in the download directory is cleaned up.
	os.MkdirAll(filepath.Join(dir, "downloads"), 0o755)
	os.WriteFile(filepath.Join(dir, "downloads", ".msm-download-123"), []byte("partial"), 0o644)
	os.WriteFile(filepath.Join(dir, "target.txt"), []byte(e.srv.URL+"/notfound\n"), 0o644)
	e.m.GetLatest(bg, "multi")
	if _, err := os.Stat(filepath.Join(dir, "downloads")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale staging file was not cleaned up")
	}
}

func sha(b []byte) string { s := sha1.Sum(b); return hex.EncodeToString(s[:]) }

// The minecraft aliases follow Mojang's manifest to the newest release or
// snapshot and verify both the version document and the JAR.
func TestGetLatestMinecraft(t *testing.T) {
	e := newEnv(t)
	jar := jarBytes("server 1.21.4")
	snap := jarBytes("server 25w01a")
	version := func(path string, body []byte, size int) []byte {
		return []byte(fmt.Sprintf(`{"downloads":{"server":{"sha1":%q,"size":%d,"url":%q}}}`, sha(body), size, e.srv.URL+path))
	}
	release := version("/release.jar", jar, len(jar))
	snapshot := version("/snapshot.jar", snap, len(snap))
	var manifest []byte
	setManifest := func(releaseDoc []byte) {
		manifest = []byte(fmt.Sprintf(`{"versions":[
			{"id":"25w01a","type":"snapshot","url":%q,"sha1":%q,"releaseTime":"2026-01-05T00:00:00+00:00"},
			{"id":"1.21.4","type":"release","url":%q,"sha1":%q,"releaseTime":"2025-12-01T00:00:00+00:00"},
			{"id":"1.21.3","type":"release","url":"https://old/x.json","sha1":"00","releaseTime":"2025-10-01T00:00:00+00:00"},
			{"id":"b1.7.3","type":"old_beta","url":"https://old/b.json","sha1":"00","releaseTime":"2011-07-01T00:00:00+00:00"}]}`,
			e.srv.URL+"/snapshot.json", sha(snapshot), e.srv.URL+"/release.json", sha(releaseDoc)))
	}
	setManifest(release)
	e.mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, _ *http.Request) { w.Write(manifest) })
	var releaseDoc = release
	e.mux.HandleFunc("/release.json", func(w http.ResponseWriter, _ *http.Request) { w.Write(releaseDoc) })
	e.serve("/snapshot.json", snapshot)
	var served = jar
	e.mux.HandleFunc("/release.jar", func(w http.ResponseWriter, _ *http.Request) { w.Write(served) })
	e.serve("/snapshot.jar", snap)

	dir := e.group("minecraft", "minecraft")
	if d, err := e.m.GetLatest(bg, "minecraft"); err != nil || d.Path != filepath.Join(dir, "2026-10-10-12-00-00-minecraft_server.1.21.4.jar") {
		t.Fatalf("release = %+v, %v", d, err)
	}
	sdir := e.group("snap", "minecraft-snapshot")
	if d, err := e.m.GetLatest(bg, "snap"); err != nil || d.Path != filepath.Join(sdir, "2026-10-10-12-00-00-minecraft_server.25w01a.jar") {
		t.Fatalf("snapshot = %+v, %v", d, err)
	}

	// A JAR that does not match its published SHA-1, a version document that
	// does not match the manifest, and a wrong size are all refused.
	e.clock.Advance(time.Hour)
	served = jarBytes("tampered")
	if _, err := e.m.GetLatest(bg, "minecraft"); !errors.Is(err, ErrVerify) {
		t.Fatalf("tampered JAR: %v", err)
	}
	served = jar
	releaseDoc = version("/release.jar", jar, len(jar)+1)
	if _, err := e.m.GetLatest(bg, "minecraft"); !errors.Is(err, ErrVerify) {
		t.Fatalf("version document not matching the manifest: %v", err)
	}
	setManifest(releaseDoc)
	if _, err := e.m.GetLatest(bg, "minecraft"); !errors.Is(err, ErrVerify) {
		t.Fatalf("size mismatch: %v", err)
	}
	if got := files(t, dir); len(got) != 2 {
		t.Fatalf("failed verifications changed the group: %q", got)
	}
}

func link(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return target
}

// CT-CMD-036, CT-CMD-037, CT-SURF-014: selection makes JAR_PATH a link to a
// JAR in the group, by name or the newest after downloading.
func TestSelect(t *testing.T) {
	e := newEnv(t)
	body := jarBytes("v1")
	e.mux.HandleFunc("/paper.jar", func(w http.ResponseWriter, _ *http.Request) { w.Write(body) })
	dir := e.group("paper", e.srv.URL+"/paper.jar")
	sdir := e.server("survival")
	custom := filepath.Join(dir, "custom-build.jar")
	os.WriteFile(custom, jarBytes("custom"), 0o644)

	if _, err := e.m.Select(bg, "survival", "paper", "custom-build.jar"); err != nil {
		t.Fatal(err)
	}
	if got := link(t, filepath.Join(sdir, "server.jar")); got != custom {
		t.Fatalf("link = %s", got)
	}
	sel, err := e.m.Select(bg, "survival", "paper", "")
	want := filepath.Join(dir, "2026-10-10-12-00-00-paper.jar")
	if err != nil || sel.JAR != want || sel.Download == nil || link(t, filepath.Join(sdir, "server.jar")) != want {
		t.Fatalf("latest = %+v, %v", sel, err)
	}

	// A failed download keeps the current JAR.
	e.mux.HandleFunc("/gone.jar", http.NotFound)
	e.m.ChangeURL("paper", e.srv.URL+"/gone.jar")
	if _, err := e.m.Select(bg, "survival", "paper", ""); !errors.Is(err, ErrDownload) || link(t, filepath.Join(sdir, "server.jar")) != want {
		t.Fatalf("failed download: %v, link %s", err, link(t, filepath.Join(sdir, "server.jar")))
	}

	os.WriteFile(filepath.Join(dir, "broken.jar"), []byte("nope"), 0o644)
	for file, wantErr := range map[string]error{
		"missing.jar": ErrNoJAR, "../paper/custom-build.jar": ErrNoJAR, "broken.jar": ErrInvalidJAR, "target.txt": ErrInvalidJAR,
	} {
		if _, err := e.m.Select(bg, "survival", "paper", file); !errors.Is(err, wantErr) {
			t.Errorf("file %q: %v, want %v", file, err, wantErr)
		}
	}
	if _, err := e.m.Select(bg, "survival", "nogroup", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing group: %v", err)
	}
	if _, err := e.m.Select(bg, "nobody", "paper", ""); !errors.Is(err, servers.ErrNotFound) {
		t.Fatalf("missing server: %v", err)
	}
	if link(t, filepath.Join(sdir, "server.jar")) != want {
		t.Fatal("a refused selection changed the link")
	}

	// A JAR copied into the server by hand is never overwritten.
	os.Remove(filepath.Join(sdir, "server.jar"))
	os.WriteFile(filepath.Join(sdir, "server.jar"), jarBytes("mine"), 0o644)
	if _, err := e.m.Select(bg, "survival", "paper", "custom-build.jar"); !errors.Is(err, ErrNotLink) {
		t.Fatalf("regular server.jar: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(sdir, "server.jar")); string(data) != string(jarBytes("mine")) {
		t.Fatal("regular server.jar was replaced")
	}
}

// CT-CMD-015, CT-CMD-016, DEV-009: delete refuses a group in use; rename
// re-points every server that uses it, or changes nothing.
func TestRenameAndDeleteKeepServersWorking(t *testing.T) {
	e := newEnv(t)
	dir := e.group("paper", "https://example.com/paper.jar")
	e.group("other", "minecraft")
	jar := filepath.Join(dir, "2026-01-01-00-00-00-paper.jar")
	os.WriteFile(jar, jarBytes("p"), 0o644)
	a, b, c := e.server("a"), e.server("b"), e.server("c")
	for _, s := range []string{"a", "b"} {
		if _, err := e.m.Select(bg, s, "paper", filepath.Base(jar)); err != nil {
			t.Fatal(err)
		}
	}
	os.Symlink("/elsewhere/x.jar", filepath.Join(c, "server.jar"))

	err := e.m.Delete("paper")
	if !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), `servers "a", "b"`) {
		t.Fatalf("delete in use: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("a refused delete removed the group")
	}

	// A failure part way restores the links and the group.
	calls := 0
	symlink = func(old, new string) error {
		if calls++; calls == 2 {
			return errors.New("injected")
		}
		return os.Symlink(old, new)
	}
	_, err = e.m.Rename("paper", "paper2")
	symlink = os.Symlink
	if err == nil || !strings.Contains(err.Error(), "nothing was renamed") {
		t.Fatalf("failed rename: %v", err)
	}
	for _, s := range []string{a, b} {
		if link(t, filepath.Join(s, "server.jar")) != jar {
			t.Fatalf("%s not restored: %s", s, link(t, filepath.Join(s, "server.jar")))
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("group not moved back")
	}

	refs, err := e.m.Rename("paper", "paper2")
	newJAR := filepath.Join(e.store, "paper2", filepath.Base(jar))
	if err != nil || len(refs) != 2 {
		t.Fatalf("rename = %+v, %v", refs, err)
	}
	for _, s := range []string{a, b} {
		if link(t, filepath.Join(s, "server.jar")) != newJAR {
			t.Fatalf("%s not re-pointed", s)
		}
	}
	if link(t, filepath.Join(c, "server.jar")) != "/elsewhere/x.jar" {
		t.Fatal("an unrelated link changed")
	}
	if _, err := e.m.Rename("paper2", "other"); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto an existing group: %v", err)
	}
	if _, err := e.m.Rename("paper2", "PAPER2"); !errors.Is(err, ErrExists) {
		t.Fatalf("case-only rename: %v", err)
	}

	for _, s := range []string{"a", "b"} {
		e.m.Select(bg, s, "other", "")
		os.Remove(filepath.Join(e.store, "..", "servers", s, "server.jar"))
	}
	if err := e.m.Delete("paper2"); err != nil {
		t.Fatal(err)
	}
	if got := files(t, e.store); !slices.Equal(got, []string{"other"}) {
		t.Fatalf("store = %q", got)
	}
	if err := e.m.Delete("paper2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

// Concurrent creates of one group: exactly one succeeds.
func TestCreateIsLocked(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = e.m.Create("g", "minecraft") }()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrExists):
			t.Fatal(err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d creates succeeded", ok)
	}
}

func TestLatestFile(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"2026-01-02-03-04-05-a.jar", "2026-01-02-03-04-06-b.jar", "2025-12-31-23-59-59-z.jar", "custom.jar", "2026-13-01-00-00-00-bad.jar"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	os.Symlink("x", filepath.Join(dir, "2030-01-01-00-00-00-link.jar"))
	if got, err := latestFile(dir); err != nil || filepath.Base(got) != "2026-01-02-03-04-06-b.jar" {
		t.Fatalf("latest = %s, %v", got, err)
	}
}

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"paper-1.21.4-130.jar":            "paper-1.21.4-130.jar",
		"../../etc/passwd":                "passwd",
		`..\..\win.jar`:                   "win.jar",
		".hidden.jar":                     "hidden.jar",
		"a b$(x);.jar":                    "a_b__x__.jar",
		"":                                "server.jar",
		"..":                              "server.jar",
		"/":                               "server.jar",
		"日本.jar":                          "__.jar",
		strings.Repeat("a", 200) + ".jar": strings.Repeat("a", 96) + ".jar",
	} {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
