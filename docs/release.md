# Releases and the update channel

This is for maintainers. It describes the files a release consists of and how
`msm update` decides to trust them. Publishing is the manual
[Release workflow](#publishing-a-release-p17) (P17); it stages a draft, and a
person publishes it.

## Files

A release directory, served over HTTPS at one URL, holds:

| File | Content |
|---|---|
| `manifest.json` | schema, project (`thehatchcloud/msm`), version `X.Y.Z`, full 40-hex source commit, and per platform the archive name, SHA-256 and size |
| `manifest.json.sig` | base64 ed25519 signature over `msm-release-manifest-v1\n` + the exact manifest bytes |
| `msm-X.Y.Z-linux-amd64.tar.gz`, `-linux-arm64`, `-darwin-amd64`, `-darwin-arm64` | `msm` (0755), `LICENSE.markdown`, `SHA256SUMS`, `RELEASE` |

`RELEASE` repeats `version`, `commit`, `os` and `arch`; the updater requires
it to equal the signed manifest and the platform it selected, and the
executable itself reports the same version and commit (stamped by the linker),
so the source and release identifiers are checked three ways. Archives are
deterministic (fixed times and owners).

## Trust

The only trust anchor is the list of public keys compiled into the executable
(`internal/buildinfo` `updateKeys`, comma separated, so a key can be rotated by
shipping a release signed by the old key that trusts both). The release
location is compiled in too (`updateURL`, https only). A build without keys
(every `make build`, CI artifact and `go build`) cannot update itself.

The private key is a file made by `msm-release keygen`. It must not be in the
repository, CI secrets for pull requests, or the host that serves the files.

## Making a release

```sh
go run ./cmd/msm-release keygen -out ~/msm-release.key      # once; prints the public key
make release VERSION=1.2.3 RELEASE_KEY=~/msm-release.key \
     RELEASE_URL=https://example.org/msm/releases/1.2.3     # from a clean, committed tree
```

`make release` builds the four platforms with CGO disabled, `-trimpath`, and
the version, commit, key and URL compiled in; packages and signs them into
`dist/release`; and runs `msm-release verify` over the result. Test the
channel against a staging URL before the production one. Installing a release
needs the files at `RELEASE_URL` exactly; `msm update` fetches
`manifest.json`, `manifest.json.sig` and the platform's archive from there.

## Publishing a release (P17)

`.github/workflows/release.yml` is the only publisher. It runs from "Run
workflow" on `master` only, with the version as input, in four jobs:

1. **Plan** refuses a bad version, a non-`master` ref, an existing tag or
   release, and a commit on which `Go checks` and `Native validation checks`
   have not succeeded.
2. **Build** makes the four executables once, with no secret: version, commit,
   `https://github.com/thehatchcloud/msm/releases/download/vX.Y.Z` and the
   public key (repository variable `RELEASE_PUBLIC_KEY`) compiled in.
3. **Validate** runs the native end-to-end suite and the Linux root checks on
   those exact files, for all four platforms and Java 17 and 21. There is no
   skipping: a missing runner fails the job and blocks the release.
4. **Sign** is the only job that holds the key. It waits for a reviewer
   (environment `release`), checks that the key matches the compiled-in public
   key, packages and signs, runs `msm-release verify` and `msm-release
   evidence -release` (so the evidence must come from the signed executables),
   and creates a **draft** release with `manifest.json`, `manifest.json.sig`,
   the four archives and the evidence. Publishing the draft creates the tag at
   the built commit.

One-time setup, in the repository settings: the variable `RELEASE_PUBLIC_KEY`
(the output of `msm-release pubkey`); an environment `release` with required
reviewers, deployment branches limited to `master`, and the secret
`RELEASE_SIGNING_KEY` (the content of the keygen file). Do not store the key as
a repository secret. The workflow has not yet been run: the first release
should use a throwaway key and a fork or a deleted draft to rehearse it.

## Runtime evidence (P16)

A release also needs proof that its executables ran, not only that they built:
native end-to-end evidence for the four platforms and both declared Java
versions ([validation.md](validation.md)). Before signing and publishing:

```sh
go run ./cmd/msm-release evidence -dir ./evidence -commit <release commit> \
    -release dist/release -pubkey "$PUBLIC_KEY"
```

An unavailable runner is a release blocker, or the platform is dropped from the
release on purpose and said so. The migration and rollback have been rehearsed
in CI on copies only ([migration.md](migration.md)); no release is a
deployment.

Version rules the updater enforces: `X.Y.Z` only (no pre-release), never an
older version than installed (a re-signed old release cannot downgrade anyone),
and the manifest must name this project.
