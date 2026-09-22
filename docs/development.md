# Go development and build workflow

This is a foundation, not a production manager. The Go executable supports only
`help`, `--help`, `-h`, `version`, and `--version`; all management commands return
a nonzero error. No Go command runs the legacy manager, reads server
configuration, or launches Minecraft.

## Toolchain and dependencies

Use the exact Go patch version in `go.mod`: Go 1.27.1 at this revision.
It was selected from the [official Go downloads](https://go.dev/dl/).
CI sets `GOTOOLCHAIN=local` and installs that version using `actions/setup-go`,
so it cannot silently upgrade the compiler. All CI actions are pinned to commit
SHAs.

The application and tests use only the standard library. There is no `go.sum`
until a module dependency is added. `govulncheck` is a separate, version-pinned
CI tool (`v1.8.0`), not a dependency linked into the binary. Dependabot checks
module and action updates; compiler and scanner updates still require an
explicit reviewed change.

## Local validation

From the repository root:

```sh
gofmt -l .
go vet ./...
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 -timeout=3m ./...
go test ./compatibility
go install golang.org/x/vuln/cmd/govulncheck@v1.8.0
govulncheck ./...
```

The race detector requires CGO and a host C toolchain. That does not change the
shipping build: all distributable binaries are built with `CGO_ENABLED=0`.

The Go tests need neither screen nor Java, use temporary directories, and never
execute the upstream manager. The separate legacy shunit suite must run on a
disposable Linux CI machine: it creates a dedicated test user and uses
`/tmp/msmtest`. Do not run it on a production Minecraft host.

## Build and identify a binary

```sh
CGO_ENABLED=0 go build -trimpath \
  -ldflags="-X github.com/thehatchcloud/msm/internal/buildinfo.version=go-port-dev -X github.com/thehatchcloud/msm/internal/buildinfo.commit=$(git rev-parse HEAD)" \
  -o bin/msm ./cmd/msm
./bin/msm version
./bin/msm help
```

A plain `go build` reports `go-port-dev` and commit `unknown`; the reproducible
CI builds explicitly inject their full source commit. No build timestamp is
injected. Unimplemented commands return exit code 65, and output-write failures
return 1.

Cross-compile by setting `GOOS` and `GOARCH` on the same build command:

| GOOS | GOARCH | Artifact |
|---|---|---|
| linux | amd64 | Linux x86-64 |
| linux | arm64 | Linux ARM64 |
| darwin | amd64 | Intel macOS |
| darwin | arm64 | Apple Silicon macOS |

These are build targets, not a claim that the eventual screen/JVM runtime has
been validated on every machine. P04 and P16 own that validation.

## Small code structure

- `cmd/msm`: executable entry point.
- `internal/cli`: argument handling, separate stdout/stderr, explicit exit codes.
- `internal/buildinfo`: injected version and commit metadata.
- `internal/process`: a small direct-exec boundary for future short-lived
  adapters, with explicit arguments, working directory and environment.
- `internal/clock`: cancelable waits for future countdown operations.
- `internal/testutil`: recording runner, manually advanced clock, and inert
  fixture copying; used by tests only, not imported into `cmd/msm`.
- `compatibility`: Go tests verifying P01 source inventories and safe fixtures.

Do not add a package for every future feature in advance. Add the configuration,
screen and manager packages when their implementing tasks need them.

## GitHub Actions

The `Go CI` workflow runs on pull requests targeting `master`, pushes to
`master`, and manual dispatch. Its token has read-only contents permission;
checkout does not retain credentials.

| Job | Evidence |
|---|---|
| Go quality (ubuntu-24.04) | Formatting, tidy drift, vet, pure-Go unit tests, race tests, native executable smoke |
| Go quality (macos-15) | The same checks on a native macOS runner |
| Go vulnerabilities | Pinned govulncheck scan of application and standard library |
| Go build (OS/architecture) | Four CGO-disabled binaries with source metadata |
| Go checks | Stable aggregate gate; fails if any prerequisite fails, is canceled or is skipped |
| Legacy tests | Separate inherited Bash/shunit suite, unchanged manager behavior |

Each build uploads a development artifact containing `msm`, the GPL license, and
`SHA256SUMS`. Artifacts expire after 14 days and are named with the full commit.
Artifact download may not retain executable mode; run `chmod +x msm` before a
local smoke test. PR artifacts correspond to GitHub's tested merge commit,
not necessarily the head branch SHA.

These files are test artifacts, not approved releases, installers or deployed
applications. Do not overwrite `/usr/local/bin/msm` or the existing init script.
The inherited automatic tag-to-release workflow is removed by this change.
Release packaging, signing/provenance, verified updates and publishing remain
P15/P17 work. There is intentionally no replacement release publishing workflow
yet, so CI cannot publish an unreviewed Go release.

## Review and branch policy

The P02 repository policy is to require a pull request, an up-to-date branch,
passing `Go checks` and `Legacy tests`, conversation resolution, and no force
pushes or deletion of `master`. Administrators are subject to those checks.

Only `thehatchcloud` is currently a repository collaborator, and changes made
through that account cannot receive an independent self-approval in GitHub.
For now, required GitHub approving reviews is set to zero; this is an explicit
enforcement gap, not a claim of automated review enforcement. Human review and
explicit merge authorization remain required by project policy.

To enforce independent review technically, add a second reviewer with suitable
repository access and set required approving reviews to one, with stale reviews
dismissed after new commits. Do not add an automatic approval bot or silently
bypass checks as a substitute for review.

P02 remains open until this single-maintainer policy is accepted or an
independent reviewer is configured. Branch-policy installation/verification is
reported on the P02 pull request rather than inferred from this document.
