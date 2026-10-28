// Command msm-release builds the signed release files msm update verifies:
// an archive per platform (the msm executable, the license, SHA256SUMS and a
// RELEASE file naming version, commit and platform), a manifest listing them
// and a detached signature of the manifest. It is a maintainer tool; it
// publishes nothing and the private key never belongs in the repository.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/thehatchcloud/msm/internal/update"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "msm-release: %v\n", err)
		os.Exit(1)
	}
}

const usage = `usage:
  msm-release keygen  -out KEYFILE            write a new signing key (0600) and print its public key
  msm-release pubkey  -key KEYFILE            print the public key to compile into builds
  msm-release package -key KEYFILE -version X.Y.Z -commit HASH -license FILE -out DIR
                      -bin os/arch=PATH ...   build the archives, manifest and signature
  msm-release verify  -pubkey BASE64 -dir DIR check a release directory as msm update would
`

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "keygen":
		return keygen(args[1:], out)
	case "pubkey":
		return pubkey(args[1:], out)
	case "package":
		return pack(args[1:], out)
	case "verify":
		return verify(args[1:], out)
	}
	return errors.New(usage)
}

func keygen(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	keyFile := fs.String("out", "", "file to create")
	if err := fs.Parse(args); err != nil || *keyFile == "" {
		return errors.New("keygen needs -out KEYFILE")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, base64.StdEncoding.EncodeToString(pub))
	return err
}

func readKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a key file made by keygen", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func pubkey(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("pubkey", flag.ContinueOnError)
	keyFile := fs.String("key", "", "key file")
	if err := fs.Parse(args); err != nil || *keyFile == "" {
		return errors.New("pubkey needs -key KEYFILE")
	}
	priv, err := readKey(*keyFile)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
	return err
}

type binFlags map[string]string

func (b binFlags) String() string { return fmt.Sprint(map[string]string(b)) }
func (b binFlags) Set(v string) error {
	plat, path, ok := strings.Cut(v, "=")
	if !ok || path == "" || strings.Count(plat, "/") != 1 {
		return fmt.Errorf("-bin wants os/arch=PATH, not %q", v)
	}
	b[plat] = path
	return nil
}

func pack(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("package", flag.ContinueOnError)
	keyFile := fs.String("key", "", "signing key file")
	version := fs.String("version", "", "release version X.Y.Z")
	commit := fs.String("commit", "", "full source commit hash")
	license := fs.String("license", "", "license file to include")
	dir := fs.String("out", "", "output directory")
	bins := binFlags{}
	fs.Var(bins, "bin", "os/arch=PATH of a built executable (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyFile == "" || *version == "" || *commit == "" || *license == "" || *dir == "" || len(bins) == 0 {
		return errors.New(usage)
	}
	key, err := readKey(*keyFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	var artifacts []update.Artifact
	for plat, path := range bins {
		goos, goarch, _ := strings.Cut(plat, "/")
		a, err := update.BuildArchive(*dir, update.Release{Version: *version, Commit: *commit, OS: goos, Arch: goarch}, path, *license)
		if err != nil {
			return fmt.Errorf("%s: %w", plat, err)
		}
		artifacts = append(artifacts, a)
		fmt.Fprintf(out, "%s  %s\n", a.SHA256, a.File)
	}
	if _, err := update.WriteManifest(*dir, *version, *commit, artifacts, key); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s and %s in %s\n", update.ManifestName, update.SignatureName, *dir)
	return nil
}

func verify(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	pub := fs.String("pubkey", "", "base64 public key")
	dir := fs.String("dir", "", "release directory")
	if err := fs.Parse(args); err != nil || *pub == "" || *dir == "" {
		return errors.New("verify needs -pubkey and -dir")
	}
	keys, err := update.ParseKeys([]string{*pub})
	if err != nil {
		return err
	}
	m, err := update.VerifyDir(keys, *dir)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "ok: version %s commit %s, %d artifacts verified\n", m.Version, m.Commit, len(m.Artifacts))
	return err
}
