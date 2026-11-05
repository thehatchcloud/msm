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

## Release checklist (P17)

Publishing is a manual maintainer step from a reviewed, CI-passing commit;
nothing in this repository uploads a release and CI holds no signing key. A
release is **not** a deployment: installing it anywhere, or migrating a
production host, needs its own approval ([release-notes.md](release-notes.md)).

1. **Commit.** The release commit is on `master`, reached through a reviewed
   pull request, and every check of that commit is green: `Go checks`
   (formatting, tidy, vet, unit, race, vulnerabilities, four builds, smoke test)
   and `Native validation` including the extended run (`workflow_dispatch`,
   `extended` checked). `git status` is clean; record `git rev-parse HEAD`.
2. **Parity.** `go test ./compatibility` passes: every row of the parity matrix
   is implemented or a reviewed deviation, none deferred, and the no-shell
   audit holds ([audit.md](audit.md)). An unresolved feature omission means no
   claim of full parity.
3. **Security and race.** `make test-race` and `make vuln` are clean on that
   commit (CI ran both; rerun locally if in doubt).
4. **Dependencies and licenses.** `docs/dependencies.md` matches `go.mod`
   (`TestDependenciesAreReviewed`); `LICENSE.markdown` is the file packaged.
5. **Failure drills**, on a copy or a disposable host, none on production:
   `make rehearse` (migration, second apply refused, rollback byte for byte);
   `make native` (crash recovery, intent after global stop, other-user
   refusal, RAM sync); a kill during `msm update` against a staging URL and
   `msm update --rollback`; a full disk during a backup (no partial archive);
   restore a world and a whole server from archives
   ([operations.md](operations.md#restoring)). Note the result of each in the
   release record.
6. **Build and sign** from that commit: `make release VERSION=X.Y.Z
   RELEASE_KEY=... RELEASE_URL=...`. The binaries, archives and the source at
   that exact commit (`git archive --format=tar.gz --prefix=msm-X.Y.Z/ HEAD`)
   are published together; `RELEASE` and `msm version` carry the same commit.
7. **Evidence.** `msm-release evidence ... -release dist/release` passes for
   all four platforms (above). A platform without evidence is dropped from the
   release and named in the notes.
8. **Notes.** Publish [release-notes.md](release-notes.md) with the release:
   required dependencies, optional helpers, OS limits, known deviations,
   future work. Tag the commit `vX.Y.Z`.
9. **Stop.** Do not deploy. Hand the release to the people who approve
   deployments.
