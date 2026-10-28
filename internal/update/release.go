package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ArchiveName is the file name of the archive for a platform.
func ArchiveName(version, goos, goarch string) string {
	return fmt.Sprintf("msm-%s-%s-%s.tar.gz", version, goos, goarch)
}

// BuildArchive writes the release archive for rel into dir from a built
// binary and the license file, and returns its manifest entry. The archive is
// deterministic: fixed times, owners and modes, fixed entry order.
func BuildArchive(dir string, rel Release, binaryPath, licensePath string) (Artifact, error) {
	bin, err := os.ReadFile(binaryPath)
	if err != nil {
		return Artifact{}, err
	}
	lic, err := os.ReadFile(licensePath)
	if err != nil {
		return Artifact{}, err
	}
	sum := sha256.Sum256(bin)
	entries := []struct {
		name string
		mode int64
		data []byte
	}{
		{BinaryName, 0o755, bin},
		{LicenseName, 0o644, lic},
		{ChecksumName, 0o644, []byte(hex.EncodeToString(sum[:]) + "  " + BinaryName + "\n")},
		{ReleaseName, 0o644, []byte(rel.text())},
	}
	name := ArchiveName(rel.Version, rel.OS, rel.Arch)
	path := filepath.Join(dir, name)
	out, err := os.Create(path)
	if err != nil {
		return Artifact{}, err
	}
	defer out.Close()
	h := sha256.New()
	zw, err := gzip.NewWriterLevel(multi{out, h}, gzip.BestCompression)
	if err != nil {
		return Artifact{}, err
	}
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.data)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
		if err := tw.WriteHeader(hdr); err != nil {
			return Artifact{}, err
		}
		if _, err := tw.Write(e.data); err != nil {
			return Artifact{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return Artifact{}, err
	}
	if err := zw.Close(); err != nil {
		return Artifact{}, err
	}
	if err := out.Sync(); err != nil {
		return Artifact{}, err
	}
	info, err := out.Stat()
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{OS: rel.OS, Arch: rel.Arch, File: name, SHA256: hex.EncodeToString(h.Sum(nil)), Size: info.Size()}, nil
}

type multi struct {
	f *os.File
	h interface{ Write([]byte) (int, error) }
}

func (m multi) Write(p []byte) (int, error) {
	if _, err := m.h.Write(p); err != nil {
		return 0, err
	}
	return m.f.Write(p)
}

// WriteManifest writes manifest.json and its signature into dir after the
// artifacts were built, and returns the manifest.
func WriteManifest(dir, version, commit string, artifacts []Artifact, key ed25519.PrivateKey) (*Manifest, error) {
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].File < artifacts[j].File })
	m := &Manifest{Schema: manifestSchema, Project: Project, Version: version, Commit: commit, Artifacts: artifacts}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if _, err := ParseManifest(data); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), data, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, SignatureName), Sign(key, data), 0o644); err != nil {
		return nil, err
	}
	return m, nil
}

// VerifyDir checks a release directory the way the updater would, for every
// platform: the signature, each archive's size and hash against the manifest,
// and each archive's layout, checksums and identifiers.
func VerifyDir(keys []ed25519.PublicKey, dir string) (*Manifest, error) {
	read := func(name string) ([]byte, error) {
		return readLimited(context.Background(), DirSource(dir), name, maxManifestSize)
	}
	manifest, err := read(ManifestName)
	if err != nil {
		return nil, err
	}
	sig, err := read(SignatureName)
	if err != nil {
		return nil, err
	}
	m, err := Verify(keys, manifest, sig)
	if err != nil {
		return nil, err
	}
	for _, a := range m.Artifacts {
		path := filepath.Join(dir, a.File)
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		if int64(len(b)) != a.Size || hex.EncodeToString(sum[:]) != a.SHA256 {
			return nil, fmt.Errorf("%w: %s", ErrArtifact, a.File)
		}
		if _, err := ReadArchive(path, Release{Version: m.Version, Commit: m.Commit, OS: a.OS, Arch: a.Arch}); err != nil {
			return nil, fmt.Errorf("%s: %w", a.File, err)
		}
	}
	return m, nil
}
