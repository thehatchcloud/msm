# Go development and build workflow

This is a foundation, not a production manager. The Go executable supports only
`help`, `--help`, `-h`, `version`, `--version`, and Cobra-generated `completion`;
all management commands return a nonzero error. Viper reads the small native
configuration schema described below. No Go command runs the legacy manager,
reads legacy server configuration, or launches Minecraft; the screen backend
(P04) is a library that later lifecycle and console tasks will call.

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

The Go tests need no Java, use temporary directories, and never execute the
upstream manager. The screen backend's native tests also run GNU screen when it
is installed (in private socket and home directories) and skip otherwise;
`MSM_REQUIRE_SCREEN=1` turns a missing screen into a failure, as CI does. The separate legacy shunit suite must run on a
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
- `internal/process`: a small direct-exec boundary with explicit arguments,
  working directory, environment and optional child credentials, plus an
  attached-terminal variant for consoles.
- `internal/clock`: cancelable waits for future countdown operations.
- `internal/testutil`: recording runner, manually advanced clock, and inert
  fixture copying; used by tests only, not imported into `cmd/msm`.
- `internal/legacyconf`: literal, eval-free parsing of `msm.conf`/`MSM_CONF`,
  with a syntax error naming the file/line for anything that still looks
  like shell, and the P03-owned global settings this task reads from it.
- `internal/serverprops`: reads and writes `server.properties`, preserving
  every Minecraft property this package does not touch, and exposes the
  per-server `msm-<lowercase-dash-name>` overrides other tasks will consume.
- `internal/atomicfile`: the temp-file-plus-rename primitive every mutation
  writes through, so a reader never observes a half-written file.
- `internal/filelock`: cross-process advisory locks for a server directory
  or the shared JAR store, always acquired in one fixed path order so
  overlapping lock requests cannot deadlock.
- `internal/safepath`: the `<name>` grammar validator, root-containment
  checks that reject traversal and symlink escape, configured-root sanity
  checks, and case-insensitive collision detection.
- `internal/identity`: resolves the manager/per-server OS user and drops
  root privilege to it; never shells out to `sudo` and never relies on a
  setuid helper.
- `internal/screen`: the GNU screen session backend (discovery, detached
  launch, console input, terminal attach) and the process-table reader that
  proves which process a session is running.
- `compatibility`: Go tests verifying P01 source inventories and safe fixtures.

Do not add a package for every future feature in advance. Add manager
packages when their implementing tasks need them.

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

The native Viper schema is still not a parser for upstream Bash `msm.conf` or
Java `server.properties`; Viper's own schema is unchanged by P03.
`internal/legacyconf` and `internal/serverprops` are a separate, explicit
compatibility importer, described next.

## Legacy configuration import and filesystem safety

P03 adds the primitives future server-management tasks (P05 onward) build
on to read an existing installation and mutate it safely. Nothing in this
foundation calls them yet: there is still no `server` command tree, and
these packages are exercised only by their own tests and by
`compatibility`.

- `legacyconf.Load` parses `msm.conf`/`MSM_CONF` as literal data, the same
  shape the Bash implementation strips before `eval`: optional matching
  single/double quotes around a value, last assignment wins. It never
  evaluates anything; a value that still contains command substitution,
  variable expansion, chaining, or redirection syntax after unquoting fails
  with the file, line, and migration advice, instead of being silently
  passed through. `legacyconf.Discover` resolves an explicit override, then
  `MSM_CONF`, then a caller-supplied system default, without assuming the
  winning candidate exists or is readable. `legacyconf.Global` extracts the
  settings this task owns (manager `USERNAME`, `SERVER_STORAGE_PATH`,
  `JAR_STORAGE_PATH`, and the Minecraft properties filename) and reports a
  warning, rather than silently choosing one, when both the registered
  `SERVER_PROPERTIES` key and the sample file's unread
  `DEFAULT_PROPERTIES_PATH` key are set to different values (see
  `docs/compatibility/README.md`). Every other legacy key remains available
  on the parsed `*legacyconf.File` for its owning task to read later.
  `File.Lookup` reads a setting as the legacy `manager_property` does: key
  matched case-insensitively, last matching line wins, and an empty value
  means unset so the default applies. `Global` uses it; `File.Get` stays an
  exact match.
- `legacyconf.ResolveServer` computes a server's effective settings, one
  per setting `init/msm` registers with `register_server_setting` (the
  table is checked against `compatibility/settings.tsv`). The first
  non-empty value wins: `msm-<lowercase-dash-name>` in the server's
  properties file, then the version profile P08 will supply, then
  `DEFAULT_<NAME>` in `msm.conf`, then the built-in default. The result is
  post-processed like `server_set_property`: relative `*_PATH` values
  (including `worldstorage`, `worldstorage_inactive` and the JAR link) are
  joined to the server directory, and `{SERVER_NAME}`, `{DELAY}`, `{RAM}`
  and `{JAR}` are expanded. `Source` says which layer each value came from,
  and an `msm-*` key that names no registered setting produces a warning
  rather than being silently ignored.
- Ownership is explicit per server: `ServerSettings.Username` comes from
  `msm-username` or `DEFAULT_USERNAME`, never from the manager's own
  `USERNAME` (the legacy manager keeps them separate), and `Owner` resolves
  it to an `identity.Identity`, failing with `ErrNoOwner` if the user does
  not exist. Callers then act as that user through `identity.DropTo` or a
  child-process credential (`process.Command.Credential`).
- `config.ResolveDataRoots` decides `ServerStoragePath`/`JarStoragePath`
  without forcing an existing installation to move: an imported legacy
  configuration always wins outright. With no legacy configuration to
  import, root keeps the classic `/opt/msm` layout, and an unprivileged
  invocation instead defaults to a rootless per-user data directory
  (`$XDG_DATA_HOME/msm`, or the platform's conventional data directory).
- `serverprops.Document` reads and writes `server.properties` as flat
  `key=value` lines, matched literally rather than through Java's
  Properties escaping rules, mirroring the legacy manager's own sed-based
  reader. `Get` is case-insensitive with quote-stripping and last-match-
  wins; `Set` writes an unquoted `key=value` line in place or appends one,
  and refuses (`ErrUnsafeProperty`) a key or value that would add or break
  a line (DEV-016). Values are never Java-unescaped or escaped; see the
  escaping decision in `docs/compatibility/README.md`.
  Every comment and every key a caller does not touch survives a write
  unchanged. `Overrides` extracts the per-server `msm-<lowercase-dash-name>`
  settings described in `docs/compatibility/README.md`.
- `atomicfile.Write` and `serverprops.Document.Save` replace a file through
  a temporary file in the same directory, fsync, and rename, so a reader
  never observes a partial write; an existing file's permissions are
  preserved rather than overwritten by a caller-supplied default.
- `filelock.Acquire`/`AcquireMany` are cross-process advisory locks (backed
  by `flock` via `golang.org/x/sys/unix`) for a server directory
  (`filelock.ServerLockPath`) or the shared JAR store
  (`filelock.RegistryLockPath`). `AcquireMany` always takes its locks in one
  fixed order (the sorted, resolved absolute paths), regardless of the
  order its caller lists them in, so two callers locking overlapping
  resources can never deadlock against each other.
- `safepath.ValidateName` implements the compatibility contract's `<name>`
  grammar (letters, digits, `_`, `-`; no reserved command tokens; no
  leading `--`). `safepath.Contain` resolves a relative path against a root
  and rejects absolute input, `..` traversal, and an existing intermediate
  symlink that would escape the root, returning the symlink-resolved real
  path. `safepath.ValidateConfiguredRoot` sanity-checks an administrator-
  supplied absolute storage path. `safepath.CaseInsensitiveCollision`
  reports an existing sibling whose name differs from a candidate only in
  case, since a server, world, or JAR group name must stay unique on a
  case-insensitive volume even though Linux's ext4/xfs are not.
- `identity.Lookup`/`identity.Current` resolve an OS user to a UID/GID.
  `identity.DropTo` is a no-op when the process is already running as the
  target user, fails with `identity.ErrPrivilegeRequired` rather than
  silently continuing as the wrong user when it is unprivileged and asked
  for a different one, and — only when already running as root — clears
  supplementary groups and sets the group then the user ID, irrevocably,
  through the standard library's `syscall` package rather than
  `golang.org/x/sys/unix`, because only the former is documented to apply a
  Linux credential change to every OS thread at once instead of just the
  calling one. Nothing in this package shells out to `sudo`/`su`, and
  nothing installs a setuid helper.

## GNU screen backend

P04 adds `internal/screen`, which keeps GNU screen as the console host
instead of introducing a daemon or custom supervisor. Nothing in the CLI calls
it yet; P06 (lifecycle) and P08 (console and game commands) will.

External tools: `screen` is the only program this package runs. It reads the
process table directly (`/proc` on Linux, `sysctl` `kern.proc`/`kern.procargs2`
on macOS) instead of running `ps`, and never uses `sh -c`, `bash -c`, `su`,
`sudo` or `script`.

- **Invocation.** Every call is `screen` with an explicit argument vector,
  working directory and a complete caller-supplied environment (not merged
  with the manager's own). Set `SCREENDIR` in that environment to keep
  sessions somewhere other than screen's default. The owner's `~/.screenrc`
  and the system screenrc still apply, as they did for the legacy manager;
  one that sets `zombie` keeps a finished session open, which `WaitStopped`
  then reports as a timeout.
- **Ownership.** Sessions belong to one OS user. A caller that is that user
  runs screen directly; a root caller runs screen with the owner's UID/GID
  and cleared supplementary groups, for the child only; any other caller is
  refused with `identity.ErrPrivilegeRequired`. `Attach` additionally
  requires the caller to be the owner, since screen will not use a terminal
  another user owns (DEV-015).
- **Discovery.** `screen -ls` is parsed, its exit status ignored (releases
  disagree). Sessions are matched by exact name and addressed by exact
  `<pid>.<name>`, never by screen's name-prefix matching. An unusable socket
  directory is `ErrInaccessible`; two live sessions with the name are
  `ErrDuplicate`, which blocks launch and sending until an administrator
  removes one. Dead or unreachable sockets are listed as `Stale`, ignored for
  liveness, and never wiped.
- **Three separate states.** `Status` reports whether a session exists and,
  separately, `Liveness`: `Running` only when the screen process belongs to
  the owner and exactly one of its window processes is owned by the owner
  and runs the configured invocation (arguments exactly, `argv[0]` by base
  name; extra console windows are noted in `Detail`); `Starting` when no
  window process is visible yet; `Foreign` when anything else holds the
  name, which is reported and never adopted, sent to or terminated. `Launch`
  keeps polling through a transient `Foreign` observation, because screen's
  forked window process looks like screen until it execs the invocation. Minecraft readiness is a third question, answered by a
  caller-supplied `Probe` inside `WaitReady`, which fails early with
  `ErrExited` if the process dies first (DEV-010).
- **Deadlines.** `Launch`, `WaitStopped` and `WaitReady` all take a timeout
  and poll through `clock.Clock`; each returns a specific error
  (`ErrStartTimeout`, `ErrStopTimeout`, `ErrReadyTimeout`, `ErrExited`) naming
  the session and what was last observed. Nothing here kills a process.
- **Console input.** `Send` accepts one line of 1–1000 bytes of printable
  UTF-8; control characters, including CR/LF, are refused. Measured on screen
  4.09, `-X stuff` arguments go through screen's command parser: `\` escapes,
  `^X` makes a control character, and `$NAME`/`${NAME}` expand screen's
  environment, so those three characters are backslash-escaped. Screen 4.09
  also silently drops a `stuff` argument over 756 bytes while exiting 0, so
  lines are sent in chunks of at most 128 bytes, never splitting an escape
  or a UTF-8 sequence, with Enter in the last chunk. A multi-chunk line is
  not atomic against a second concurrent sender; callers serialize on the
  server lock. See DEV-013 and DEV-014.
- **Attach.** `Attach` requires real terminals for stdin and stdout and runs
  `screen -r <pid>.<name>` connected to them. Ctrl-A d detaches and leaves
  the server running.

### Tested screen versions

| Platform | screen | Source | Evidence |
|---|---|---|---|
| Linux amd64 (Ubuntu 24.04) | 4.09.01 | distribution package `screen` 4.9.1-1ubuntu1 | local run and `Go quality (ubuntu-24.04)` |
| macOS arm64 (macos-15 runner) | 5.0.2 | Homebrew `brew install screen` | `Go quality (macos-15)` |
| macOS arm64, OS-bundled `/usr/bin/screen` | 4.00.03 (FAU) | Apple | **Unsupported.** The native tests fail: the session's window process is never observed as a child of its screen process. `Backend.Version` refuses it, and CI asserts that refusal |

`Backend.Version` returns `ErrUnsupportedVersion` for anything older than
4.01, with advice to install a current screen; callers check it once before
using the backend. Releases between 4.01 and 4.09.01 are not yet tested and
are not claimed until they appear in this table. On macOS, install screen
from Homebrew and configure its absolute path.

The native tests in `internal/screen/native_test.go` launch a stand-in
server (the test binary itself, so no Java), exit that launching process,
reconnect and send from fresh processes, round-trip every printable ASCII
character and a maximum-length escapable line, attach through a
pseudo-terminal and detach with Ctrl-A d, and stop within a deadline. They
also cover a stale socket whose orphaned child must not be adopted,
duplicate and foreign sessions, an inaccessible socket directory, and a
launch that fails. They use a private `SCREENDIR` and `HOME`, and skip when
screen is missing unless `MSM_REQUIRE_SCREEN=1`, which CI sets after
installing screen explicitly. `MSM_TEST_SCREEN` selects a specific binary:

```sh
MSM_REQUIRE_SCREEN=1 go test -count=1 -v -run Native ./internal/screen
```

The root-to-owner credential drop is covered by unit tests and was checked by
hand as root against an unprivileged user; hosted CI runners are not root, so
CI does not repeat it.

## GitHub Actions

The `Go CI` workflow runs on pull requests targeting `master`, pushes to
`master`, and manual dispatch. Its token has read-only contents permission;
checkout does not retain credentials.

| Job | Evidence |
|---|---|
| Go quality (ubuntu-24.04) | Formatting, tidy drift, vet, pure-Go unit tests, race tests, native screen backend tests, native executable smoke |
| Go quality (macos-15) | The same checks on a native macOS runner (Homebrew screen), plus a check that the bundled `/usr/bin/screen` 4.00.03 is refused |
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
