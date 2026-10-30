# Releases and the update channel

This is for maintainers. It describes the files a release consists of and how
`msm update` decides to trust them. **Publishing is not done here**: nothing in
this repository uploads a release, and CI cannot (P17).

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
