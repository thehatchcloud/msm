package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const maxManifestSize = 1 << 20

// Status says what Run did.
type Status int

const (
	// UpToDate: the signed release is the installed version.
	UpToDate Status = iota
	// Available: a newer signed release exists (check only; nothing changed).
	Available
	// Updated: the program was replaced.
	Updated
	// Declined: the operator answered no; nothing changed.
	Declined
)

// Plan is what Confirm is asked about: a verified release, not yet applied.
type Plan struct {
	From, To Version
	Commit   string
	Artifact Artifact
	Target   string
}

// Result is the outcome of Run.
type Result struct {
	Status   Status
	Plan     Plan
	Previous string // where the replaced program was kept
}

// Options configures one update.
type Options struct {
	Keys   []ed25519.PublicKey
	Source Source
	// Current is the installed version (buildinfo), GOOS and GOARCH the
	// platform whose artifact is wanted.
	Current      string
	GOOS, GOARCH string
	// Exe is the running executable's path.
	Exe string
	// CheckOnly verifies the release and reports, changing nothing.
	CheckOnly bool
	// Confirm is called with the verified plan before anything is replaced.
	Confirm func(Plan) (bool, error)
	// Check runs on the staged program before it replaces the old one; nil
	// uses SmokeCheck.
	Check func(staged string, rel Release) error

	hooks hooks
}

// Run verifies the latest signed release for this platform and, unless
// CheckOnly, replaces the executable atomically with it. Nothing downloaded
// is executed before its signature, size and hash were verified, no shell is
// involved, and configuration and worlds are never read or written: the only
// files touched are the executable, its .previous copy and a lock and staging
// files beside it.
func Run(ctx context.Context, o Options) (*Result, error) {
	if len(o.Keys) == 0 {
		return nil, ErrNoTrustedKey
	}
	cur, err := ParseVersion(o.Current)
	if err != nil {
		return nil, fmt.Errorf("%w: this is %q, a development build; install a release to use update", ErrUnknownVersion, o.Current)
	}
	var target *Target
	if !o.CheckOnly {
		if target, err = ResolveTarget(o.Exe); err != nil {
			return nil, err
		}
	}
	manifestBytes, err := readLimited(ctx, o.Source, ManifestName, maxManifestSize)
	if err != nil {
		return nil, err
	}
	sig, err := readLimited(ctx, o.Source, SignatureName, 1024)
	if err != nil {
		return nil, err
	}
	m, err := Verify(o.Keys, manifestBytes, sig)
	if err != nil {
		return nil, err
	}
	next, err := ParseVersion(m.Version)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	art, err := m.Select(o.GOOS, o.GOARCH)
	if err != nil {
		return nil, err
	}
	plan := Plan{From: cur, To: next, Commit: m.Commit, Artifact: art}
	if target != nil {
		plan.Target = target.Path
	}
	switch c := next.Compare(cur); {
	case c < 0:
		return nil, fmt.Errorf("%w: installed %s, signed release %s (a downgrade is not an update; use --rollback to return to the previous program)", ErrDowngrade, cur, next)
	case c == 0:
		return &Result{Status: UpToDate, Plan: plan}, nil
	case o.CheckOnly:
		return &Result{Status: Available, Plan: plan}, nil
	}
	if o.Confirm != nil {
		yes, err := o.Confirm(plan)
		if err != nil {
			return nil, err
		}
		if !yes {
			return &Result{Status: Declined, Plan: plan}, nil
		}
	}
	rel := Release{Version: m.Version, Commit: m.Commit, OS: o.GOOS, Arch: o.GOARCH}
	files, err := download(ctx, o.Source, target, art, rel)
	if err != nil {
		return nil, err
	}
	check := o.Check
	if check == nil {
		check = SmokeCheck
	}
	err = target.replace(files.Binary, func(staged string) error { return check(staged, rel) }, o.hooks)
	if err != nil {
		return nil, err
	}
	return &Result{Status: Updated, Plan: plan, Previous: target.PreviousPath()}, nil
}

// download fetches the archive into the target directory, verifies size and
// hash against the signed manifest, then reads it in memory and removes it.
func download(ctx context.Context, src Source, t *Target, art Artifact, rel Release) (*Files, error) {
	rc, err := src.Open(ctx, art.File)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	f, err := os.CreateTemp(t.Dir, stagePrefix+"dl-*")
	if err != nil {
		return nil, permissionOrErr(err, t.Dir)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(rc, art.Size+1))
	if err != nil {
		return nil, fmt.Errorf("update: download %s: %w", art.File, err)
	}
	if n != art.Size {
		return nil, fmt.Errorf("%w: %s is %d bytes, the manifest says %d", ErrArtifact, art.File, n, art.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != art.SHA256 {
		return nil, fmt.Errorf("%w: %s has sha256 %s, the manifest says %s", ErrArtifact, art.File, got, art.SHA256)
	}
	return ReadArchive(f.Name(), rel)
}

// SmokeCheck runs the staged program's version command, a fixed argument
// list without a shell, and requires it to report the signed version and
// commit. It runs only code whose signature and hash were already verified,
// and it catches a binary that cannot execute here (wrong platform, noexec
// directory) before the working program is replaced.
func SmokeCheck(staged string, rel Release) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, staged, "version")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%w: the downloaded program did not run: %v", ErrArtifact, err)
	}
	s := string(out)
	if !strings.Contains(s, " "+rel.Version+" ") || !strings.Contains(s, "commit "+rel.Commit+")") {
		return fmt.Errorf("%w: the downloaded program reports %q, not version %s commit %s", ErrArtifact, strings.TrimSpace(s), rel.Version, rel.Commit)
	}
	return nil
}
