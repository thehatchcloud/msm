package jars

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxMetadata bounds the Mojang manifest and version documents.
const maxMetadata = 16 << 20

// jarMagic starts every JAR, which is a ZIP archive.
var jarMagic = []byte("PK\x03\x04")

func newHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	t.TLSHandshakeTimeout = 30 * time.Second
	t.ResponseHeaderTimeout = time.Minute
	return &http.Client{Transport: t}
}

// checkRedirect follows at most ten redirects, all to https.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("%w: more than 10 redirects", ErrDownload)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("%w: redirected to %s", ErrInsecureURL, req.URL.Redacted())
	}
	return nil
}

// response is one finished download.
type response struct {
	final       *url.URL
	disposition string
	size        int64
}

// fetch GETs u into w, refusing anything but 200 and anything over limit.
// If h is set, the body is also hashed into it.
func (m *Manager) fetch(ctx context.Context, u string, limit int64, w io.Writer) (response, error) {
	client := *m.cfg.HTTP
	client.CheckRedirect = checkRedirect
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return response{}, fmt.Errorf("%w: %v", ErrDownload, err)
	}
	req.Header.Set("User-Agent", "msm-go")
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && errors.Is(urlErr.Err, ErrInsecureURL) {
			return response{}, urlErr.Err
		}
		return response{}, fmt.Errorf("%w: %v", ErrDownload, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return response{}, fmt.Errorf("%w: %s answered %s", ErrDownload, resp.Request.URL.Redacted(), resp.Status)
	}
	if resp.ContentLength > limit {
		return response{}, fmt.Errorf("%w: %s is %d bytes, over the %d byte limit", ErrTooLarge, resp.Request.URL.Redacted(), resp.ContentLength, limit)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return response{}, fmt.Errorf("%w: reading %s: %v", ErrDownload, resp.Request.URL.Redacted(), err)
	}
	if n > limit {
		return response{}, fmt.Errorf("%w: %s is over the %d byte limit", ErrTooLarge, resp.Request.URL.Redacted(), limit)
	}
	return response{final: resp.Request.URL, disposition: resp.Header.Get("Content-Disposition"), size: n}, nil
}

// planned is what to download for a target: a URL, the file name to save
// it under, and the hash and size the publisher states, when it does.
type planned struct {
	url, name string
	sha1      string
	size      int64
}

type mojangManifest struct {
	Versions []struct {
		ID          string    `json:"id"`
		Type        string    `json:"type"`
		URL         string    `json:"url"`
		SHA1        string    `json:"sha1"`
		ReleaseTime time.Time `json:"releaseTime"`
	} `json:"versions"`
}

type mojangVersion struct {
	Downloads struct {
		Server *struct {
			SHA1 string `json:"sha1"`
			Size int64  `json:"size"`
			URL  string `json:"url"`
		} `json:"server"`
	} `json:"downloads"`
}

// plan resolves a target. For the minecraft aliases it reads Mojang's
// version manifest, picks the newest release or snapshot by release time
// (as the legacy jq filter did), checks the version document against the
// manifest's SHA-1, and takes the server download with its SHA-1 and size.
func (m *Manager) plan(ctx context.Context, t Target) (planned, error) {
	if t.Kind == TargetURL {
		return planned{url: t.URL.String()}, nil
	}
	var buf bytes.Buffer
	if _, err := m.fetch(ctx, m.cfg.ManifestURL, maxMetadata, &buf); err != nil {
		return planned{}, fmt.Errorf("version manifest: %w", err)
	}
	var man mojangManifest
	if err := json.Unmarshal(buf.Bytes(), &man); err != nil {
		return planned{}, fmt.Errorf("%w: version manifest: %v", ErrDownload, err)
	}
	kind := "release"
	if t.Kind == TargetSnapshot {
		kind = "snapshot"
	}
	versions := man.Versions[:0:0]
	for _, v := range man.Versions {
		if v.Type == kind {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return planned{}, fmt.Errorf("%w: the version manifest lists no %s", ErrDownload, kind)
	}
	sort.SliceStable(versions, func(i, j int) bool { return versions[i].ReleaseTime.Before(versions[j].ReleaseTime) })
	latest := versions[len(versions)-1]
	if !strings.HasPrefix(latest.URL, "https://") {
		return planned{}, fmt.Errorf("%w: version %s document is not served over https", ErrInsecureURL, latest.ID)
	}
	buf.Reset()
	h := sha1.New()
	if _, err := m.fetch(ctx, latest.URL, maxMetadata, io.MultiWriter(&buf, h)); err != nil {
		return planned{}, fmt.Errorf("version %s: %w", latest.ID, err)
	}
	if err := checkSHA1(h, latest.SHA1, "version "+latest.ID+" document"); err != nil {
		return planned{}, err
	}
	var ver mojangVersion
	if err := json.Unmarshal(buf.Bytes(), &ver); err != nil {
		return planned{}, fmt.Errorf("%w: version %s: %v", ErrDownload, latest.ID, err)
	}
	server := ver.Downloads.Server
	if server == nil || server.URL == "" {
		return planned{}, fmt.Errorf("%w: version %s has no server download", ErrDownload, latest.ID)
	}
	if !strings.HasPrefix(server.URL, "https://") {
		return planned{}, fmt.Errorf("%w: version %s server JAR is not served over https", ErrInsecureURL, latest.ID)
	}
	return planned{url: server.URL, name: "minecraft_server." + latest.ID + ".jar", sha1: server.SHA1, size: server.Size}, nil
}

func checkSHA1(h hash.Hash, want, what string) error {
	if want == "" {
		return nil
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: %s has SHA-1 %s, the manifest says %s", ErrVerify, what, got, want)
	}
	return nil
}

// Download is the result of jargroup getlatest.
type Download struct {
	// Path is the group's newest JAR afterwards.
	Path string
	// Previous is the newest JAR before, if there was one.
	Previous string
	// Unchanged reports that the download matched Previous and was
	// discarded.
	Unchanged bool
}

// GetLatest downloads the newest version for a group's target
// (jargroup getlatest).
func (m *Manager) GetLatest(ctx context.Context, name string) (Download, error) {
	dir, err := m.existing(name)
	if err != nil {
		return Download{}, err
	}
	lock, err := m.lock(false)
	if err != nil {
		return Download{}, err
	}
	defer lock.Release()
	if dir, err = m.existing(name); err != nil {
		return Download{}, err
	}
	if err := m.drop(m.cfg.Owner); err != nil {
		return Download{}, err
	}
	return m.getLatest(ctx, dir)
}

// getLatest does the download with the registry lock held and privileges
// already dropped. The file is written to a staging name in the group's
// download directory, verified, and only then renamed into the group, so
// an interrupted or invalid download never replaces a known-good JAR.
func (m *Manager) getLatest(ctx context.Context, dir string) (Download, error) {
	target, err := m.readTarget(dir)
	if err != nil {
		return Download{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()
	p, err := m.plan(ctx, target)
	if err != nil {
		return Download{}, err
	}

	staging := filepath.Join(dir, m.cfg.DownloadDir)
	if err := os.Mkdir(staging, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return Download{}, fmt.Errorf("jars: create %s: %w", staging, err)
	}
	if info, err := os.Lstat(staging); err != nil || !info.IsDir() {
		return Download{}, fmt.Errorf("jars: %s is not a directory", staging)
	}
	cleanStaging(staging)
	defer os.Remove(staging) // only if empty, as legacy removed it afterwards

	f, err := os.CreateTemp(staging, stagePrefix+"download-*")
	if err != nil {
		return Download{}, fmt.Errorf("jars: %w", err)
	}
	tmp := f.Name()
	published := false
	defer func() {
		if !published {
			os.Remove(tmp)
		}
	}()
	h := sha1.New()
	resp, err := m.fetch(ctx, p.url, m.cfg.MaxBytes, io.MultiWriter(f, h))
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Download{}, err
	}
	if err := checkSHA1(h, p.sha1, p.url); err != nil {
		return Download{}, err
	}
	if p.size > 0 && resp.size != p.size {
		return Download{}, fmt.Errorf("%w: %s is %d bytes, the manifest says %d", ErrVerify, p.url, resp.size, p.size)
	}
	if err := checkJAR(tmp); err != nil {
		return Download{}, fmt.Errorf("%w: what %s served is not a JAR (ZIP) file; nothing was saved", ErrInvalidJAR, resp.final.Redacted())
	}

	previous, err := latestFile(dir)
	if err != nil {
		return Download{}, err
	}
	if previous != "" {
		if same, err := sameContents(previous, tmp); err != nil {
			return Download{}, err
		} else if same {
			return Download{Path: previous, Previous: previous, Unchanged: true}, nil
		}
	}
	name := p.name
	if name == "" {
		name = fileName(resp)
	}
	final := filepath.Join(dir, m.cfg.Clock.Now().Format("2006-01-02-15-04-05")+"-"+name)
	if _, err := os.Lstat(final); err == nil {
		return Download{}, fmt.Errorf("jars: %s already exists; try again in a second", final)
	}
	if err := os.Rename(tmp, final); err != nil {
		return Download{}, fmt.Errorf("jars: publish %s: %w", final, err)
	}
	published = true
	syncDir(dir)
	return Download{Path: final, Previous: previous}, nil
}

// cleanStaging removes downloads an interrupted run left behind. Only
// files with this manager's staging prefix are touched.
func cleanStaging(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagePrefix) && e.Type().IsRegular() {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// fileName chooses the saved name for a URL download: the server's
// Content-Disposition file name, else the last element of the final URL's
// path. Whatever the server says is reduced to one safe file name.
func fileName(resp response) string {
	var name string
	if _, params, err := mime.ParseMediaType(resp.disposition); err == nil {
		name = params["filename"]
	}
	if name == "" && resp.final != nil {
		name = path.Base(resp.final.Path)
	}
	return safeFileName(name)
}

// safeFileName keeps letters, digits, '.', '_', '+' and '-', replaces
// anything else with '_', drops any directory part and leading dots, and
// limits the length. An empty result becomes server.jar.
func safeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = name[strings.LastIndex(name, "/")+1:]
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '+', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name = strings.TrimLeft(b.String(), ".")
	if len(name) > 100 {
		name = name[len(name)-100:]
	}
	if name == "" || strings.Trim(name, "_") == "" {
		return "server.jar"
	}
	return name
}

// latestFile is get_latest_file: the regular file in dir whose name starts
// with the newest YYYY-MM-DD-HH-MM-SS time, or "" if there is none. Ties
// go to the greater name, so the choice is deterministic.
func latestFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("jars: list %s: %w", dir, err)
	}
	var best string
	var bestTime time.Time
	for _, e := range entries {
		if !e.Type().IsRegular() || len(e.Name()) < 19 {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02-15-04-05", e.Name()[:19], time.Local)
		if err != nil {
			continue
		}
		if best == "" || t.After(bestTime) || t.Equal(bestTime) && e.Name() > filepath.Base(best) {
			best, bestTime = filepath.Join(dir, e.Name()), t
		}
	}
	return best, nil
}

func checkJAR(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, len(jarMagic))
	if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head, jarMagic) {
		return ErrInvalidJAR
	}
	return nil
}

func sameContents(a, b string) (bool, error) {
	ia, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if ia.Size() != ib.Size() {
		return false, nil
	}
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	ha, hb := sha1.New(), sha1.New()
	if _, err := io.Copy(ha, fa); err != nil {
		return false, err
	}
	if _, err := io.Copy(hb, fb); err != nil {
		return false, err
	}
	return bytes.Equal(ha.Sum(nil), hb.Sum(nil)), nil
}
