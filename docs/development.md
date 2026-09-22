# Go development and build workflow

This is a foundation, not a production manager. The Go executable supports only
`help`, `--help`, `-h`, `version`, `--version`, and Cobra-generated `completion`;
all management commands return a nonzero error. Viper reads the small native
configuration schema described below. No Go command runs the legacy manager,
reads legacy server configuration, or launches Minecraft.

## Toolchain and dependencies

Use the exact Go patch version in `go.mod`: Go 1.27.1 at this revision.
It was selected from the [official Go downloads](https://go.dev/dl/).
CI sets `GOTOOLCHAIN=local` and installs that version using `actions/setup-go`,
so it cannot silently upgrade the compiler. All CI actions are pinned to commit
SHAs.

The CLI uses [Cobra](https://github.com/spf13/cobra) v1.10.2 and configuration
uses [Viper](https://github.com/spf13/viper) v1.21.0. Dependencies are pinned in
`go.mod` and verified by `go.sum`. `govulncheck` is a separate, version-pinned
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
injected. Cobra command/flag errors, configuration errors, and errors returned
by `RunE` return exit code 1, printed once to stderr. This replaces the
foundation's interim exit code 65; the full legacy exit taxonomy remains P14
work. Cobra owns help/completion output behavior.

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
- `internal/cli`: Cobra command constructors, persistent flags, separate
  stdout/stderr, and the executable error/exit boundary.
- `internal/config`: per-invocation Viper setup and typed native settings.
- `internal/buildinfo`: injected version and commit metadata.
- `internal/process`: a small direct-exec boundary for future short-lived
  adapters, with explicit arguments, working directory and environment.
- `internal/clock`: cancelable waits for future countdown operations.
- `internal/testutil`: recording runner, manually advanced clock, and inert
  fixture copying; used by tests only, not imported into `cmd/msm`.
- `compatibility`: Go tests verifying P01 source inventories and safe fixtures.

Do not add a package for every future feature in advance. Add screen and
manager packages when their implementing tasks need them.

## Cobra and Viper conventions

Follow [Cobra's flag guidance](https://cobra.dev/docs/how-to-guides/working-with-flags/)
and [Viper's documented configuration patterns](https://github.com/spf13/viper).
Do not introduce a second argument parser or configuration framework.

- Construct commands with `NewRootCommand` and small command constructors;
  register children using `AddCommand`. Use `Use`, `Short`, argument validators
  such as `cobra.NoArgs`, and `RunE` to return errors.
- Keep `os.Exit` in `main`. `SilenceErrors` and `SilenceUsage` prevent duplicate
  error output; the outer `Run` function prints execution errors once.
- Let Cobra generate help and shell completion. Built-in completion covers the
  current command tree, not future dynamic server/world names. PowerShell
  completion generation does not imply Windows runtime support.
- Define persistent flags on the root. Bind configuration flags with
  `BindPFlag` after defining them; do not copy flag defaults using `viper.Set`.
- Create a private `viper.New()` instance for every command tree. No package
  globals, `init()` registration, global `OnInitialize` hooks, or configuration
  watchers are needed. Execute each tree once.
- Load configuration in `PersistentPreRunE`, after flag parsing. Use
  `UnmarshalExact` and `mapstructure` tags for typed settings; register defaults
  for each new key so environment-only settings participate in unmarshalling.
- Pass typed settings to future services, rather than sharing mutable Viper
  instances across goroutines. Keep filesystem/process behavior out of command
  constructors, and test with fresh commands and isolated configuration homes.

Future command work must preserve the P01 server-first syntax
(`msm <server> start`), rather than silently changing it to verb-first syntax.
Design that compatibility adapter around Cobra in P14; server commands are not
implemented by this foundation.

## Native configuration foundation

The only implemented setting is `debug` (default `false`). Viper applies
changed flags > environment > configuration file > defaults, including an
explicit `--debug=false` overriding a true environment/file value.

```yaml
debug: true
```

```sh
msm version --config ./config.yaml
MSM_DEBUG=true msm version
MSM_DEBUG=true msm version --debug=false
msm completion bash
```

Without `--config`, discovery uses `os.UserConfigDir()/msm/config` with Viper's
supported file extensions; YAML (`config.yaml`) is the recommended format.
On Linux this normally uses `$XDG_CONFIG_HOME/msm` or `$HOME/.config/msm`;
on macOS it uses `$HOME/Library/Application Support/msm`. The working directory
and `/etc/msm.conf` are not searched. If no user configuration directory can be
resolved, only an explicitly provided file is read.

An absent optional file is allowed. An explicitly requested missing file,
malformed file, unknown key, or invalid typed value is an error. Configuration
is read-only; no files/directories are created by the loader. The `MSM_`
environment prefix and underscore key mapping are configured centrally;
empty environment values retain Viper's default unset behavior.

Cobra handles help and the built-in `--version` flag before pre-run hooks, so
those informational paths work even with a broken configuration. The `version`
subcommand and runnable completion commands do run configuration initialization.
Debug mode currently emits only a configuration-loaded diagnostic on stderr.

The native Viper schema is not a parser for upstream Bash `msm.conf` or Java
`server.properties`. Legacy discovery, `MSM_CONF`, migration, and per-server
settings remain P03 work and require an explicit compatibility importer.

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
