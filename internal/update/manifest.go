// Package update is the verified self-updater and the release packaging that
// feeds it. A release is a signed manifest plus one archive per platform. The
// updater trusts only an ed25519 signature made by a key compiled into the
// running binary, never the transport, and it never runs a shell, a downloaded
// script or any code before the replacement is verified and staged.
package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Project is the repository the manifest must name, so a signature made for
// another project that shares a key is not accepted here.
const Project = "thehatchcloud/msm"

// Names of the files a release directory holds.
const (
	ManifestName  = "manifest.json"
	SignatureName = "manifest.json.sig"
)

// signingContext is prepended to the manifest before signing, so a signature
// over this manifest cannot be replayed as a signature of anything else.
const signingContext = "msm-release-manifest-v1\n"

const manifestSchema = 1

// Errors the command line maps to named exit statuses.
var (
	// ErrNoTrustedKey: this build carries no release signing key.
	ErrNoTrustedKey = errors.New("this build has no trusted release signing key")
	// ErrBadSignature: the manifest is not signed by a trusted key.
	ErrBadSignature = errors.New("manifest signature is not valid for any trusted key")
	// ErrManifest: the signed manifest is malformed or not for this project.
	ErrManifest = errors.New("release manifest is invalid")
	// ErrNoArtifact: the release has no artifact for this OS and architecture.
	ErrNoArtifact = errors.New("release has no artifact for this platform")
	// ErrArtifact: a downloaded artifact does not match what was signed.
	ErrArtifact = errors.New("release artifact does not match the signed manifest")
	// ErrDowngrade: the signed release is older than the installed program.
	ErrDowngrade = errors.New("signed release is older than the installed version")
	// ErrUnknownVersion: the installed version cannot be compared.
	ErrUnknownVersion = errors.New("installed version is not a release version")
)

// Artifact is one platform's archive in a release.
type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is the signed description of a release.
type Manifest struct {
	Schema    int        `json:"schema"`
	Project   string     `json:"project"`
	Version   string     `json:"version"`
	Commit    string     `json:"commit"`
	Artifacts []Artifact `json:"artifacts"`
}

var (
	versionRE = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	commitRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256RE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fileRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	platRE    = regexp.MustCompile(`^[a-z0-9]+$`)
)

// maxArtifactSize bounds any archive and any file extracted from one.
const maxArtifactSize = 256 << 20

// Version is a release version, MAJOR.MINOR.PATCH.
type Version [3]int

// ParseVersion reads "1.2.3" or "v1.2.3". Pre-release and build suffixes are
// not release versions and are rejected.
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("%w: %q", ErrUnknownVersion, s)
	}
	var v Version
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Version{}, fmt.Errorf("%w: %q", ErrUnknownVersion, s)
		}
		v[i] = n
	}
	return v, nil
}

// Compare returns -1, 0 or 1.
func (v Version) Compare(o Version) int {
	for i := range v {
		switch {
		case v[i] < o[i]:
			return -1
		case v[i] > o[i]:
			return 1
		}
	}
	return 0
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// ParseKeys decodes base64 ed25519 public keys.
func ParseKeys(encoded []string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, e := range encoded {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(e))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("update: trusted signing key %q is not a base64 ed25519 public key", e)
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys, nil
}

// Sign returns the base64 detached signature of manifest bytes.
func Sign(key ed25519.PrivateKey, manifest []byte) []byte {
	sig := ed25519.Sign(key, append([]byte(signingContext), manifest...))
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// Verify checks sig over manifest against the trusted keys and, only after
// the signature holds, parses and validates the manifest.
func Verify(keys []ed25519.PublicKey, manifest, sig []byte) (*Manifest, error) {
	if len(keys) == 0 {
		return nil, ErrNoTrustedKey
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: unreadable signature", ErrBadSignature)
	}
	msg := append([]byte(signingContext), manifest...)
	ok := false
	for _, k := range keys {
		if ed25519.Verify(k, msg, raw) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, ErrBadSignature
	}
	return ParseManifest(manifest)
}

// ParseManifest decodes and validates a manifest. It is strict: unknown
// fields, trailing data, repeated platforms and file names that are not plain
// names are errors.
func ParseManifest(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data", ErrManifest)
	}
	switch {
	case m.Schema != manifestSchema:
		return nil, fmt.Errorf("%w: schema %d is not supported", ErrManifest, m.Schema)
	case m.Project != Project:
		return nil, fmt.Errorf("%w: it is for project %q", ErrManifest, m.Project)
	case !versionRE.MatchString(m.Version) || strings.HasPrefix(m.Version, "v"):
		return nil, fmt.Errorf("%w: version %q", ErrManifest, m.Version)
	case !commitRE.MatchString(m.Commit):
		return nil, fmt.Errorf("%w: commit %q is not a full lowercase commit hash", ErrManifest, m.Commit)
	case len(m.Artifacts) == 0:
		return nil, fmt.Errorf("%w: no artifacts", ErrManifest)
	}
	seen := map[string]bool{}
	for _, a := range m.Artifacts {
		key := a.OS + "/" + a.Arch
		switch {
		case !platRE.MatchString(a.OS) || !platRE.MatchString(a.Arch):
			return nil, fmt.Errorf("%w: platform %q", ErrManifest, key)
		case seen[key]:
			return nil, fmt.Errorf("%w: platform %s is listed twice", ErrManifest, key)
		case !fileRE.MatchString(a.File) || strings.Contains(a.File, ".."):
			return nil, fmt.Errorf("%w: file name %q", ErrManifest, a.File)
		case !sha256RE.MatchString(a.SHA256):
			return nil, fmt.Errorf("%w: sha256 of %s", ErrManifest, a.File)
		case a.Size <= 0 || a.Size > maxArtifactSize:
			return nil, fmt.Errorf("%w: size of %s", ErrManifest, a.File)
		}
		seen[key] = true
	}
	return &m, nil
}

// Select returns the artifact for exactly goos/goarch; there is no fallback
// to another architecture.
func (m *Manifest) Select(goos, goarch string) (Artifact, error) {
	for _, a := range m.Artifacts {
		if a.OS == goos && a.Arch == goarch {
			return a, nil
		}
	}
	return Artifact{}, fmt.Errorf("%w: %s/%s", ErrNoArtifact, goos, goarch)
}
