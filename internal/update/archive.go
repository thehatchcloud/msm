package update

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Names inside a release archive. The archive holds exactly these files, at
// its top level, and nothing else.
const (
	BinaryName   = "msm"
	LicenseName  = "LICENSE.markdown"
	ChecksumName = "SHA256SUMS"
	ReleaseName  = "RELEASE"
)

// Release is the identity recorded in an archive's RELEASE file; it must
// match the signed manifest and the platform it was selected for.
type Release struct {
	Version string
	Commit  string
	OS      string
	Arch    string
}

func (r Release) text() string {
	return fmt.Sprintf("version=%s\ncommit=%s\nos=%s\narch=%s\n", r.Version, r.Commit, r.OS, r.Arch)
}

func parseRelease(b []byte) (Release, error) {
	var r Release
	fields := map[string]*string{"version": &r.Version, "commit": &r.Commit, "os": &r.OS, "arch": &r.Arch}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		dst := fields[k]
		if !ok || dst == nil || seen[k] || v == "" {
			return Release{}, fmt.Errorf("%w: RELEASE line %q", ErrArtifact, line)
		}
		seen[k] = true
		*dst = v
	}
	if len(seen) != len(fields) {
		return Release{}, fmt.Errorf("%w: RELEASE is incomplete", ErrArtifact)
	}
	return r, nil
}

// Files is the verified content of an archive.
type Files struct {
	Binary  []byte
	License []byte
}

// ReadArchive extracts a release archive in memory, enforcing its exact
// layout, and checks that the binary matches the archive's own SHA256SUMS and
// that RELEASE names want. It never writes to the filesystem, so a hostile
// path inside the archive has nowhere to go.
func ReadArchive(path string, want Release) (*Files, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return nil, fmt.Errorf("%w: not a gzip archive", ErrArtifact)
	}
	defer zr.Close()
	tr := tar.NewReader(io.LimitReader(zr, 3*maxArtifactSize))
	got := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrArtifact, err)
		}
		switch h.Name {
		case BinaryName, LicenseName, ChecksumName, ReleaseName:
		default:
			return nil, fmt.Errorf("%w: unexpected entry %q", ErrArtifact, h.Name)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%w: %q is not a regular file", ErrArtifact, h.Name)
		}
		if _, dup := got[h.Name]; dup {
			return nil, fmt.Errorf("%w: %q appears twice", ErrArtifact, h.Name)
		}
		if h.Size < 0 || h.Size > maxArtifactSize {
			return nil, fmt.Errorf("%w: %q is too large", ErrArtifact, h.Name)
		}
		b, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if err != nil || int64(len(b)) != h.Size {
			return nil, fmt.Errorf("%w: %q is truncated", ErrArtifact, h.Name)
		}
		got[h.Name] = b
	}
	for _, n := range []string{BinaryName, LicenseName, ChecksumName, ReleaseName} {
		if len(got[n]) == 0 {
			return nil, fmt.Errorf("%w: %q is missing or empty", ErrArtifact, n)
		}
	}
	sum := sha256.Sum256(got[BinaryName])
	if line := hex.EncodeToString(sum[:]) + "  " + BinaryName + "\n"; string(got[ChecksumName]) != line {
		return nil, fmt.Errorf("%w: %s does not match the binary", ErrArtifact, ChecksumName)
	}
	rel, err := parseRelease(got[ReleaseName])
	if err != nil {
		return nil, err
	}
	if rel != want {
		return nil, fmt.Errorf("%w: archive is %+v, signed manifest says %+v", ErrArtifact, rel, want)
	}
	return &Files{Binary: got[BinaryName], License: got[LicenseName]}, nil
}
