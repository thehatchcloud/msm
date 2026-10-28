# Go development and build workflow

This is an alpha, not a production manager. The Go executable supports
`help`, `--help`, `-h`, `version`, `--version`, Cobra-generated `completion`,
and the P05 instance commands `server list`, `server create`, `server rename`
and `server delete` (see [Server instances](#server-instances-p05)), the P06
lifecycle commands, the P07 JAR commands, the P08 console and player
commands (see [Console and player commands](#console-and-player-commands-p08))
the P09 world commands `worlds list|load|on|off` (see [Worlds](#worlds-p09)) and
the P10 backup commands `worlds backup` and `backup`, which P11 extends with
hard-link snapshots and rdiff-backup (see [Backups](#backups-p10)), and the
P12 commands `worlds ram` and `worlds todisk` (see [RAM-backed
worlds](ram-worlds.md)).
Every other management command returns a nonzero error. Viper reads the small native
configuration schema described below. No Go command runs the legacy manager or
launches Minecraft; the screen backend (P04) is a library that later lifecycle
and console tasks will call, and that `server` commands use only to check
whether a server is running.

## Toolchain and dependencies

Use the exact Go patch version in `go.mod`: Go 1.27.3 at this revision.
It was selected from the [official Go downloads](https://go.dev/dl/).
CI sets `GOTOOLCHAIN=local` and installs that version using `actions/setup-go`,
so it cannot silently upgrade the compiler. All CI actions are pinned to commit
SHAs.

Locally, `make build`, `test`, `test-race` and `cover` first run `make
check-go`, which fails with the fix if the Go in use is not that version. Either
install it, or opt in to letting Go fetch the pinned toolchain
(`GOTOOLCHAIN=auto make test`; `auto` is Go's default from 1.21, so this only
matters if your environment sets `local` or `off`). The Makefile does not set
`GOTOOLCHAIN` itself, so your choice and CI's `local` both stand.

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

The `Makefile` wraps these commands; run `make` to list its targets. `make
check` runs the formatting, vet, module, unit and race checks, `make build`
writes `bin/msm` stamped with the current commit, `make cross` writes one
binary per platform below to `dist/`, `make vuln` runs the pinned
`govulncheck`, and `make clean` removes `bin/`, `dist/` and `coverage.out`
(`make clean-cache` also clears the Go build and test caches). CI runs the
commands directly and does not use make.

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
- `internal/servers`: server instance management (list, create, rename,
  delete) over one storage root, and the screen-backed check of whether a
  server is running.
- `internal/profiles`: the parts of the legacy version profiles that
  lifecycle needs (log path, log-line prefix, start event, save-all
  confirmation), as inert data; P08 extends it.
- `internal/jars`: JAR groups (list, create, delete, rename, changeurl,
  getlatest), verified downloads and a server's JAR selection.
- `internal/lifecycle`: start, stop, restart and status for one server,
  every server (`all`) or the global set, with countdowns, deadlines and
  bounded bulk concurrency.
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
- `msm help`, `msm --help` and `msm -h` print the legacy `command_help`
  list verbatim (`internal/cli/help.go`; `TestLegacyHelpAndNoSuchCommand`
  compares it with `init/msm`). It lists every legacy command, including
  those not yet ported. Cobra still generates help for subcommands
  (`msm help server`, `msm server delete --help`) and shell completion.
  Built-in completion covers the current command tree, not future dynamic
  server/world names. PowerShell completion generation does not imply
  Windows runtime support.
- Anything that matches no command, including a bare `msm`, prints the
  legacy `No such command. See <program> help`, on stderr and with exit
  status 1 rather than the legacy stdout and 0 (see the compatibility
  contract).
- `<program>` in that line and in the help's `Usage:` line is the
  executable's path as bash's `$0` would give it: as typed when it contains
  a `/` (`./bin/msm`), otherwise the full path found on `PATH`
  (`/usr/local/bin/msm`).
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

Command work must preserve the P01 server-first syntax (`msm <server>
start`), rather than silently changing it to verb-first syntax. P06 adds the
first part of that adapter: the root command accepts arbitrary arguments and
dispatches `<server>|all <verb>` itself (`runServerCommand`), while real
subcommands such as `server` and the global `start` keep priority. P14
extends it to every server command.

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

P03 adds the primitives server-management tasks (P05 onward) build on to
read an existing installation and mutate it safely. `internal/servers` (P05)
is their first caller.

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
instead of introducing a daemon or custom supervisor. P06 (lifecycle) uses it;
P08 (console and game commands) will.

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

## Server instances (P05)

`internal/servers` and the `msm server` commands manage the directories below
`SERVER_STORAGE_PATH`, with the upstream layout and defaults
(`compatibility/commands.tsv` CMD-009 to CMD-012, SURF-038).

### Configuration and ownership

The CLI imports the legacy `msm.conf` from `MSM_CONF`, else `/etc/msm.conf`
(`legacyconf.Discover`). It supplies `SERVER_STORAGE_PATH`,
`SERVER_PROPERTIES`, the manager user `USERNAME`, and the `DEFAULT_*` values a
new server starts from. Without one, root uses the classic `/opt/msm/servers`
owned by `minecraft`, and an unprivileged user gets a rootless data directory
(`config.ResolveDataRoots`) that is created on the first `server create`.
Servers whose owner is not configured belong to that user. A configured
`SERVER_STORAGE_PATH` must already exist; the manager never creates it.

Files are created only as the manager user. An unprivileged caller must be
that user (else `identity.ErrPrivilegeRequired`). A root caller takes the
locks and checks the server's state as root, then permanently drops to the
manager user (`identity.DropTo`) before its first filesystem change, so a
privileged process never modifies paths inside a tree an unprivileged user
can write to. Lock files a root caller creates are made with `O_EXCL` (never
through a symbolic link) and handed to their owner through the open
descriptor.

### Behavior

- **Names.** `safepath.ValidateName` applies to every new and existing name.
  A name that already exists, even as a file or symbolic link, or that differs
  from an existing entry only in letter case, is refused. A case-only rename
  (`Foo` to `foo`) is refused; rename through another name.
- **List.** Servers are sorted by name and keep the legacy wording, for
  example `[ ACTIVE ] "survival" is running. Everything is OK.` Active intent
  is the `FLAG_ACTIVE_PATH` marker. Liveness comes from GNU screen: a live
  session running the configured invocation is running; any other live
  session with the server's `SCREEN_NAME` is reported as not verifiably this
  server; a session the manager cannot inspect (for example another user's,
  without root) is reported as unknown. If `screen` is not on `PATH`, no
  server can be running in it. Leftover staged operations, symbolic links and
  directories with invalid names are listed as warnings on stderr, never as
  servers. The expected invocation is the argument vector P06 launches
  (`legacyconf.ServerSettings.Invocation`); a mismatch can only make a server
  look occupied, never stopped.
- **Create.** Writes `[]` into the whitelist, banned-IPs, banned-players and
  ops JSON files, `ops.txt` from `DEFAULT_OPS_LIST` (comma separated, spaces
  removed), an empty properties file, and the world storage directory with
  its `readme.txt`, exactly as `server_create` does. It never writes
  `eula.txt` or the `active` marker. A `DEFAULT_*_PATH` that resolves outside
  the new directory is not created and produces a warning. The instance is
  assembled in `.msm-create-<name>-<random>` in the storage root and
  published with one `rename(2)`. It does not select a server JAR
  (DEV-017).
- **Rename.** Refuses unless the server is proven stopped (running, occupied
  and unknown all refuse). The directory moves with one `rename(2)`; then
  each top-level symbolic link whose absolute target lies inside the old
  directory (the world links `server_ensure_links` makes) is atomically
  retargeted into the new one. Links elsewhere, such as `server.jar` into the
  JAR store, are untouched. If retargeting fails, the changed links are
  restored and the directory moved back, and the error says nothing was
  renamed; if even that fails, the error names where the server now is.
  Archives made under the old name keep it, as before, and the command says
  so.
- **Delete.** Shows the resolved directory, file count and size, top-level
  entries and every symbolic link that points outside the server, then asks
  the legacy question. Only `y`, `Y` or `yes` deletes; anything else prints
  `Server was NOT deleted.` and exits 0. When stdin is not a terminal it
  never reads it: it fails unless `--yes` (`-y`) is given. A server that is
  not proven stopped is refused, instead of being stopped as the legacy
  command did (DEV-018). Under the locks, the directory must still be the
  one previewed; it is renamed to `.msm-delete-<name>-<random>` (leaving
  service in one step) and then removed without following symbolic links,
  so link targets outside the server always survive. If removal stops part
  way, the command fails with the leftover path, which `server list` keeps
  reporting.
- **Locking.** Create, rename and delete hold the storage-root lock
  (`.msm-servers.lock`); rename and delete also hold the server's
  `.msm.lock`, acquired together through `filelock.AcquireMany`.
  `Manager.LockServer`, for P06 onward, takes only a server's lock and fails
  with `ErrNotFound` if the server was renamed or deleted while it waited
  (`filelock.Lock.Stat` proves the locked file is still the one at the path).

### Tests

`internal/servers` tests interrupt every filesystem step of create and
rename, and both steps of delete, through an injected failing `fsOps`, and
check that the original instance is intact (or the staged directory is
reported). The contract tests carry their IDs in comments: CT-CMD-009
(`TestList`), CT-CMD-010 (`TestCreateLayout`), CT-CMD-011 (`TestDelete`),
CT-CMD-012 (`TestRename`) and CT-SURF-038 (`TestServerDeleteConfirmation`).
Privilege dropping is tested with a recorded `DropTo`; the real drop was
checked by hand by running the binary as root with an unprivileged
`USERNAME`.

## Server lifecycle (P06)

`internal/lifecycle` and the server-first commands start, stop, restart and
report on servers (`compatibility/commands.tsv` CMD-001 to CMD-005 and CMD-020
to CMD-025, SURF-005):

```text
msm start                   start every active server that is stopped
msm stop [now]              stop every running server (active or not)
msm restart [now]           stop every running server, then start the active ones
msm <server> start          mark the server active and start it
msm <server> stop [now]     stop it and mark it inactive
msm <server> restart [now]  restart (or start) it and mark it active
msm <server> status         say whether it is running
msm all <command> [now]     run a <server> command on every server
```

The root command accepts the legacy server-first form directly; any first
word that is not a Cobra subcommand is a server name (or `all`). Only these
verbs are implemented; anything else gets `No such command. See msm help`
and exits nonzero.
P14 extends the dispatcher to the remaining commands. `--timeout` and
`--jobs` (below) apply to every lifecycle command.

### Intent and bulk behavior

- **Intent.** The per-server commands change the `FLAG_ACTIVE_PATH` marker;
  the global commands never do. `msm all stop` therefore marks every server
  inactive, whereas `msm stop` leaves every marker alone (tested by
  `TestGlobalStopIsNotAllStop`). Start and restart mark the server active
  first, even if the start then fails, as `server_start` does. Stop marks it
  inactive once the stop is committed; an aborted countdown changes nothing
  (DEV-019).
- **Global start** starts active stopped servers and reports inactive running
  ones with the legacy wording. **Global restart** stops every running
  server, then starts the active set; a server that failed to stop is not
  started, and an interrupted stop phase starts nothing.
- **Concurrency.** Each server operation holds that server's lock
  (`servers.Manager.LockServer`), so two managers never act on one server at
  once: a second `msm <server> start` waits and then finds it running. Bulk
  commands work on up to `--jobs` servers at once (default 4); each output
  line is prefixed with its server's name and never interleaves with
  another. `msm all status` takes no locks.
- **Aggregate status.** A bulk command attempts every server and fails if
  any failed, with one error listing every failure in name order, so the
  message and exit status do not depend on scheduling
  (`TestBulkPartialFailure`).

### Starting

1. The invocation is `INVOCATION` split into an argument vector without a
   shell (`legacyconf.ServerSettings.Invocation`, DEV-001). Spaces and tabs
   separate words; single quotes are literal; double quotes are literal but
   may not contain `$`, a backtick or `\`. Any other unquoted character a
   shell would treat specially (`;&|<>()$\*?[]{}~#!` and the backtick) is
   refused, not guessed at; quote an argument that needs one. The
   placeholders `{RAM}` (a positive whole number) and `{JAR}` are then
   filled in each word, so a JAR path with spaces stays one argument.
2. Preflight, before anything is launched: the program must be an
   executable file (searched on `PATH` when it has no `/`), `JAR_PATH` must
   be a regular file starting with the ZIP signature, and screen must be
   installed and at least 4.01.
3. The log is marked, the server is launched in a detached screen session
   as its owner, and the command waits for a fresh start line: the profile's
   line prefix followed by `Done` (DEV-010). Lines already in the log never
   count: a log the server appends to is read from where it ended, and one
   it replaces (Minecraft 1.7+ moves `logs/latest.log` aside) or rewrites is
   read from the start, even if the file system reuses the inode number.
4. If the process ends first, the command fails and says why when it can:
   the EULA is not accepted (`eula.txt` without `eula=true`, or the EULA
   notice in the log), or the port is in use (`FAILED TO BIND TO PORT` in
   the log). The manager never writes `eula.txt`.

### Stopping

Unless `now` is given, the players are warned with `MESSAGE_STOP` (or
`MESSAGE_RESTART`) and the command waits `STOP_DELAY` (or `RESTART_DELAY`)
seconds. Ctrl+C or SIGTERM during that countdown sends the abort message
and leaves the server running. Then, with or without `now`: `save-all` is
sent and its profile confirmation awaited for the profile's timeout (an
unconfirmed save is reported and the stop continues, since `stop` saves
too), `stop` is sent, and the command waits for the process to end.
Synchronizing RAM worlds to disk joins this sequence in P12.

### Deadlines

Readiness and stopping each wait at most 5 minutes (readiness at least the
profile's start timeout), or `--timeout`. At the deadline the command fails
and names what is still running; nothing is ever killed or quit
(DEV-019). The window process must appear within 15 seconds of launch.

### Version profiles

`internal/profiles` encodes, for the five shipped profiles, only what
lifecycle needs: `LOG_PATH`, the log-line prefix (`console_event REGEX`),
the `START` event and the `SAVE_ALL` confirmation, with inheritance
resolved. `TestProfilesMatchVersioningFiles` reads `versioning/*.sh` as
text and requires the Go data to match. `VERSION` selects the newest
profile of the same type that is not newer (numeric comparison); an unset
or unmatched `VERSION` uses `minecraft/1.7.0`, as the legacy manager assumed
the newest profile. The profile's `LOG_PATH` sits between a per-server
`msm-log-path` and `DEFAULT_LOG_PATH`, as in `server_property`. Modern
servers use the 1.7 log format; their `save-all` confirmation differs, so it
is reported as unconfirmed until P08 adds a modern profile.

### Privileges

As root, the manager takes the server lock as root and runs screen, and
therefore the server, with the owner's credentials for that child only
(P04). The active marker is changed through a descriptor for the server
directory, opened without following a symbolic link, and a new marker is
handed to the owner; as root, a `FLAG_ACTIVE_PATH` outside the server
directory is refused. The log, JAR and `eula.txt` are only read; the log is
opened non-blocking and must be a regular file. An unprivileged caller must
be the server's owner.

External tools: only `screen`, which runs the configured invocation
(normally `java`). Nothing goes through a shell.

### Tests

`internal/lifecycle` runs the real screen backend over a simulated screen
and Minecraft (`fake_test.go`) with a clock that advances virtually, so
countdowns and deadlines are instant and deterministic. It covers intent for
every command, `now`, the countdown abort, stale start lines, EULA
rejection, missing Java, missing and invalid JARs, port conflicts, crashes,
a server that ignores `stop`, simultaneous managers, unproven sessions,
bounded concurrency and partial bulk failure. `native_test.go` repeats the
main flows against real GNU screen with this test binary as the server
(`TestFakeMinecraft`), under the same `MSM_REQUIRE_SCREEN` rules as P04.
Contract IDs are in the test comments: CT-CMD-001 to CT-CMD-005 and
CT-CMD-020 to CT-CMD-025.

## JAR groups (P07)

`internal/jars` and `msm jargroup list|create|delete|rename|changeurl|getlatest`
and `msm <server> jar <jargroup> [<file>]` manage the JAR store below
`JAR_STORAGE_PATH` (`compatibility/commands.tsv` CMD-013 to CMD-018, CMD-036,
CMD-037; SURF-011 to SURF-014, SURF-039). The store comes from the same
`msm.conf` as the servers; rootless, it is the data directory's `jars`, made
on demand. `JARGROUP_TARGET` (`target.txt`) and `JARGROUP_DOWNLOAD_DIR`
(`downloads`) are read from `msm.conf`.

### Layout and commands

- A group is a directory holding its target file and JARs named
  `YYYY-MM-DD-HH-MM-SS-<name>` (local time of the download). `list` prints
  each group and, indented, those timestamped JARs, as `jargroup_list` does.
  A JAR placed in a group by hand under any other name is not listed but can
  be selected by name.
- `create` validates the name like a server name and the target, and
  publishes the group with its target file in one rename. `changeurl`
  replaces the target file atomically and requires it to exist.
- **Targets** (DEV-020): an https URL; `minecraft-snapshot`; or any other
  word starting with `minecraft` (normally `minecraft`), meaning the newest
  release. A plain http URL is refused when set, and an http target left by
  the legacy manager fails at download time. A target file naming several
  URLs is refused, as the legacy download of several files failed.
- `delete` refuses a group that any server's `JAR_PATH` points into, naming
  the servers; otherwise it asks the legacy question (`y`, `Y` or `yes`
  deletes) or takes `--yes` when standard input is not a terminal. The group
  leaves service in one rename and is then removed without following links.
- `rename` moves the group and re-points every server whose `JAR_PATH`
  points into it at the same JAR under the new name (DEV-009). If any step
  fails, the links already changed are restored and the group moved back.

### Downloads

`getlatest`, and `<server> jar <group>` without a file, download with Go's
HTTP client: verified TLS (1.2 or later), https redirects only (at most
10), a 30 s TLS handshake and 1 minute response-header timeout, 10 minutes
for the whole download, and at most 512 MiB. `minecraft` targets read
Mojang's version manifest (`version_manifest_v2.json`), pick the newest
release or snapshot by release time (as the legacy `jq` filter did), check
the version document's SHA-1 against the manifest, and check the server
JAR's SHA-1 and size against the version document; the file is saved as
`minecraft_server.<version>.jar`. URL targets are saved under the server's
`Content-Disposition` file name, else the last element of the final URL,
reduced to letters, digits, `.`, `_`, `+` and `-`, without directories or
leading dots.

Each download is written to a staging file in the group's download
directory, synced, checked (hash and size when published, and the ZIP
signature every JAR starts with), and only then renamed into the group. A
failed, interrupted, oversized, unverified or non-JAR download therefore
never adds or replaces a JAR, and staging files an interrupted run left are
removed on the next download. A download identical to the group's newest
JAR is discarded (`was already up to date`).

### Selecting a server's JAR

`msm <server> jar <group> <file>` points `JAR_PATH` at that file in the
group; it must be a regular file there and a JAR. Without a file, the
group's newest version is downloaded first and the newest timestamped JAR
is used; if the download fails, the server keeps its current JAR. The link
is replaced atomically, and only when `JAR_PATH` is missing or already a
link: a JAR copied into the server by hand is never overwritten. The change
takes effect at the server's next start. `msm all jar …` selects for every
server in turn and reports each failure.

### Locking and privileges

Every store mutation and every selection holds the registry lock
(`.msm-registry.lock` in the store); selection also holds the server's lock,
always after the registry lock, so rename and delete never race a selection.
As root, the manager locks and checks as root, then drops permanently to the
store's user before changing anything (as for servers). Because that drop
is to one user, root refuses to re-point, on rename, a server owned by
another user, and to download-and-select for one; selecting an existing
file drops to the server's owner instead. An unprivileged caller must be the
store's user.

External tools: none. Downloads no longer need `wget` or `jq`.

### Tests

`internal/jars` tests run against local `httptest` TLS servers: URL and
`minecraft`/`minecraft-snapshot` downloads, redirects, a redirect to http,
redirect loops, 404s, declared and streamed oversized bodies, a connection
dropped mid-body, non-JAR content, a hostile `Content-Disposition` name, a
tampered JAR, a version document that does not match the manifest, a size
mismatch, rename rollback after an injected failure, deletes of groups in
use, and concurrent creates. Contract IDs are in the test comments.

## Console and player commands (P08)

`internal/console` and `msm <server> connected|console|whitelist|wl|blacklist|bl|
operator|op|gamemode|gm|kick|say|time|toggledownfall|tdf|give|xp|save|cmd|cmdlog`
send commands to a running server's console and report its answer
(`compatibility/commands.tsv` CMD-026, CMD-038, CMD-042 to CMD-069). `msm all
<command>` reaches every running server in name order; `console` and `cmdlog`
need one server. External tools: GNU screen only.

### Profiles

`internal/profiles` holds, as inert data, what each command types and what
answers it, for the five shipped `versioning/*.sh` profiles (a test parses
those files and requires the Go data to equal them, apart from DEV-022) and
the `minecraft/1.13.0` profile for current servers (DEV-023). `VERSION` picks
the newest profile not newer than it; an unset or unknown `VERSION` is
`minecraft/1.7.0`, as before, and a `VERSION` that names no profile warns on
standard error. Each command lists its answers in the profile's order; the
first *n* confirm it and the rest refuse it. `CONFIRM_*` settings are
registered by the legacy manager but were never read, and are ignored here.

Support is as follows. Every legacy profile is covered by recorded-line tests
of the profile's own patterns. The modern profile is covered by vanilla
language-file wording and Paper's `[12:00:00 INFO]:` prefix, not by a live
server yet; P16 adds native Java runs. `craftbukkit/1.13+`, Forge and other
servers with different log formats need their own profile or `cmd`.

### Sending and answering

A command is typed as one line (DEV-014), under the server's lock. Its answer
is read from the server log (`internal/logwatch`, shared with lifecycle): only
lines written after the send count, in a log that is replaced or truncated
mid-wait too. A matching refusal is an error; an unanswered command, after
5 s, is an error saying it may still have taken effect; a refusal-only
profile entry (such as `gamemode` in 1.3) waits one second and then reports
"not confirmed". Several players are handled one after another: a refusal does
not stop the rest, an unanswered command does. Nothing is sent unless every
step is valid for the server's version. Minecraft assigns no request IDs, so a
player triggering the same message at the same moment is indistinguishable;
holding the lock prevents this manager from causing it.

`whitelist on|off` on a stopped server sets `white-list` in `server.properties`
instead (the file's other lines are untouched), as the legacy manager did.
The other commands, and `save`, refuse a stopped server. `connected` on a
stopped server says no one is connected; with screen missing it fails, since
that cannot be known.

### Lists

`whitelist list`, `operator list` and `blacklist list` read the server's files
(`WHITELIST_PATH`, `OPS_PATH`, `BANNED_PLAYERS_PATH`, `BANNED_IPS_PATH`)
without writing them: JSON arrays (Minecraft 1.7.6 and later) and the older
one-name-per-line text are both read; a half-written or oversized file is an
error, never an empty list.

### cmd, cmdlog and console

`cmd` types the words joined with spaces. `cmdlog` does that and prints the
last five log lines and then new ones until Ctrl+C, which stops only the
follower: the lock is released after sending, and the server keeps running.
`console` attaches the terminal with screen as the session's owner and first
explains that Ctrl+A then D detaches, while Ctrl+C is typed into the server
console and can stop it.

### Tests

`internal/console` tests use a fake session layer for log correlation (stale
lines, forged chat, refusals, rotation, `all`, list formats, cmdlog) and, with
GNU screen, run the commands against a real session, including the byte-exact
`cmd` round trip of screen metacharacters.

## Worlds (P09)

`internal/worlds` and `msm <server> worlds list|load|on <world>|off <world>`
manage a server's worlds (`compatibility/commands.tsv` CMD-027, CMD-028,
CMD-032, CMD-033; SURF-006 to SURF-008). `worlds ram` and `todisk` are P12
and described in [RAM-backed worlds](ram-worlds.md); `worlds backup` is
[P10](#backups-p10).

```text
msm <server> worlds list        list the worlds, active and inactive
msm <server> worlds load        link the active worlds into the server directory
msm <server> worlds on <world>  move an inactive world into active storage
msm <server> worlds off <world> move an active world into inactive storage
msm all worlds list|load        every server, in name order
msm all worlds on|off all       every world of every server
```

### Layout

A server keeps worlds in `WORLD_STORAGE_PATH` (default `worldstorage`,
active) and `WORLD_STORAGE_INACTIVE_PATH` (default `worldstorage_inactive`),
each possibly overridden per server and possibly outside the server
directory. They must differ, must not contain one another and must not be the
server directory. A world is a real directory in one of them; a file, a
symbolic link, a hidden directory or a name with control characters is
ignored and reported on stderr, so a link planted in storage never makes the
manager touch its target. A storage directory that is itself a symbolic link
(for example to another disk) is followed. Active worlds are linked into the
server directory under their own names, the links Minecraft finds; inactive
worlds are not linked, and are the ones world-only backups skip (P10).

### Commands

- **List** prints one line per world, sorted, `RAM  name` when the world holds
  the `WORLDS_FLAG_INRAM` file and five spaces otherwise, as the legacy
  command does, or `There are no worlds in worldstorage.` It reads only, and
  works whatever the server is doing.
- **Load** and start. `worlds load` and `msm <server> start` run the same
  reconciliation (the legacy `server_ensure_links`): an active world's link is
  created, or replaced if it points elsewhere, and the link of an inactive
  world is removed; a name that is already a real file or directory is
  reported as `Could not link world` and left alone, the other worlds are
  still linked, and `worlds load` then exits nonzero (start continues, as it
  did, with the report). If the properties name a `level-name`,
  `active-storage/<level-name>` is created so a new world is generated in
  storage, unless that world already exists in the server directory or in
  inactive storage. Links are changed through a descriptor for the server
  directory opened without following a symbolic link, each new link is made
  under a temporary name, handed to the owner and renamed into place, so
  root cannot be redirected by the server's owner and a failure leaves the old
  link.
- **RAM.** A world flagged for RAM links to `RAMDISK_STORAGE_PATH/<server>/<world>`
  only when `msm.conf` sets `RAMDISK_STORAGE_ENABLED=true` and the copy
  exists; otherwise to disk, with a warning. Creating and synchronizing the
  copy is P12.
- **On and off** move the whole world directory (with its `DIM-1`/`DIM1`
  dimensions) between the storage directories. Nothing is replaced: if the
  name is taken in the destination, the command fails with the world still
  where it was. Within one filesystem it is a single `rename(2)`. Across
  filesystems the world is copied (links kept as links, modes and times
  kept, special files refused) to a hidden `.msm-world-<name>-<random>`
  directory beside the destination, published with one rename, and the
  original is then renamed aside and removed, so an interruption leaves the
  world intact at its source. If the original cannot be retired the copy is
  removed again; if even that fails the error names both places. A leftover
  that could not be removed is reported by `worlds list`. Deactivating also
  removes the world's own link from the server directory, but not a link
  that points anywhere else, and is refused while the world has a copy in
  RAM that may hold newer data. A world that exists in both directories is
  ambiguous and is refused.
- **All.** `msm all worlds list|load` runs every server, one at a time in
  name order, with each line prefixed by the server's name; every server is
  attempted and a failure is reported once at the end. `msm all worlds
  on|off` needs `all` as the world (`when specifying "all" servers, "all"
  worlds must be specified also`); it moves every world that is not already
  there. `msm <server> worlds on all` is "there is no world with the name
  \"all\"", as before.

### Safety policy (DEV-024)

Every change first takes the server's lock and proves the server is stopped
(running, occupied or unknown refuse), so a link or world never changes under
a live server. As root, `on` and `off` drop permanently to the server's owner
before moving anything, and a root process that has dropped to one user refuses
a server owned by another; `load` and start do not drop (start must keep
running screen) and use the descriptor-relative calls above instead. An
unprivileged caller must be the server's owner.

External tools: none.

### Tests

`internal/worlds` tests cover multiple worlds with dimension directories,
per-server path overrides, planted links, RAM resolution, conflicts, the
stopped-server policy, `all`, privileges, and an injected failure at every
filesystem step of a move and of a link change. Contract IDs are in the test
comments: CT-CMD-027, CT-CMD-028, CT-CMD-032, CT-CMD-033, CT-SURF-006 to
CT-SURF-008. `TestStartLinksWorlds` (`internal/lifecycle`) checks the start
integration and `TestWorldsCommands` (`internal/cli`) the grammar.

## Backups (P10)

`internal/backup`, `msm <server> worlds backup` and `msm <server> backup`
(CMD-031, CMD-035; SURF-015 to SURF-017) make ZIP backups with Go's
`archive/zip`. ZIP backups run no external tool; `zip` is not needed. The
optional P11 methods are described in [Other backup methods
(P11)](#other-backup-methods-p11).

```text
msm <server> worlds backup     one archive per active world
msm <server> backup            the whole server directory
msm all worlds backup|backup   every server, in name order
    --backup-timeout 2h        limit on compressing (default 2h)
    --timeout 5s               how long each console answer is awaited
    --allow-external-links     see "Following links" below
```

### Layout

```text
WORLD_ARCHIVE_PATH/<server>/<world>/<YYYY-MM-DD-HH-MM-SS>.zip   holds <world>/...
BACKUP_ARCHIVE_PATH/<server>/<YYYY-MM-DD-HH-MM-SS>.zip          holds <server>/...
```

The timestamp is local time, as `date "+%F-%H-%M-%S"` gave. Entries keep
permission bits and modification times, directories have their own entries
and are listed in name order. A world backup covers the worlds in
`WORLD_STORAGE_PATH` only; worlds in the inactive directory are skipped (a
world present in both directories is skipped with a warning). A complete
backup is the server directory, which includes inactive storage when it lives
there; if world storage is configured outside the server directory a warning
says those worlds are not in it. Without an `msm.conf` the archive directories
are `archives/worlds` and `archives/backups` beside the servers directory.
Archive files are `0600` and their directories `0700` (DEV-026).

The world layout is the one WorldEdit's snapshot database reads: point its
snapshot directory at `WORLD_ARCHIVE_PATH/<server>`. This was checked against
WorldEdit's own source, not assumed: see [Tests](#tests-5).

### A stopped server

The backup holds the server's lock and proves the server stopped, then
archives the files in place. This is the only mode that claims a consistent
copy of everything, plugins and configuration included.

### A running server

The sequence, with the server's lock held throughout:

1. A marker, `.msm-backup-pending`, is written in the server directory.
2. `say` (`MESSAGE_WORLD_BACKUP_STARTED` or `MESSAGE_COMPLETE_BACKUP_STARTED`),
   then `save-off` and `save-all` as the server's version profile spells them.
   Each must be confirmed by a log line written after it was sent (DEV-010,
   DEV-021); an unanswered or refused command stops the backup. `save-all` is
   followed by `sync(2)`. A server that refuses `save-off` (it is already off)
   is left as it was.
3. The worlds (or the server tree) are copied to a hidden staging directory
   next to the archives, within `SaveWindow` (15 minutes).
4. `save-on`, confirmed, and the marker is rewritten without `saving_off`.
5. Only then is the copy compressed, within `--backup-timeout`, and the
   staging directory removed.

The prior state of automatic saving cannot be read from the server; the
manager assumes it was on, as the legacy one did, except when `save-off` is
refused. A world flagged for RAM whose RAM copy exists makes the backup of a
running server refuse until P12 provides the RAM-to-disk sync (a hook,
`Config.SyncRAM`, is called after `save-all`).

Limits of a live backup, printed each time: worlds are saved and copied
together, but plugin data, configuration and logs outside the worlds are not
flushed by `save-all` and are copied while the server runs (a complete backup)
or not at all (a world backup). For a consistent copy of those, stop the
server.

### Failure and recovery

* Saving is turned back on after any error, on Ctrl+C or `SIGTERM` (the
  context is canceled; the re-enable runs on a fresh two-minute context) and
  when the save window passes. If it cannot be turned back on the error says
  so, and the marker stays.
* The marker also covers a manager killed with `SIGKILL`, a power loss or an
  OOM kill. The next `worlds backup` or `backup` of that server takes the
  lock (which proves the old process is gone), reads the marker, sends
  `save-on` first if the server runs and saving may be off, and removes
  `.msm-backup-staging-*` directories, `.msm-backup-*.partial` files and
  `.msm-backup-snapshot-*.partial` directories below the server's archive,
  rsync and rdiff-backup directories. The marker supplies only the fact that
  saving may be off; what is deleted is found from the configured archive
  directories, never from paths in the marker, which the server's owner can
  edit. There is no separate recovery command, and no restore command: restore
  stays a manual `unzip`.
* An archive is written to `.msm-backup-<random>.partial`, flushed, re-read to
  check it holds every planned entry, and published by `link(2)` to its final
  name, which cannot replace an existing archive. A full disk (`ENOSPC`), a
  permission error, a timeout, cancellation or a killed process therefore
  never leave a file named `*.zip` that is incomplete, and the partial file is
  removed or swept by the next backup. If one of several world archives (or
  methods) fails, the ones already published are complete and the error lists
  them. The sweep also removes partial snapshots below the rsync and rdiff-backup
  directories, matching only the exact names the manager creates.
* The archive directory, the staging directory, the marker and the server lock
  file are never archived, even when the archive directory is inside what is
  being archived (including through a followed link).

### Following links (DEV-011, DEV-025)

World archives never follow links. A complete backup stores links as links
unless the server's `COMPLETE_BACKUP_FOLLOW_SYMLINKS` is `true`. When it is,
the tree is walked before the server is touched and the backup is refused,
listing each `link -> target`, if a link leads outside the server directory,
its two world storage directories and `JAR_STORAGE_PATH` (where a server JAR
normally points); `--allow-external-links` accepts the list after review. A
link that leads back to a directory already being archived is a cycle and is
refused. Sockets, pipes and devices are skipped with a warning.

### Other backup methods (P11)

`worlds backup` makes every method that is enabled, for every active world, in
the legacy order (ZIP, rdiff-backup, hard-link snapshot), with the same save,
staging, marker and recovery steps as a ZIP backup. A running server is saved
and copied once, saving is turned back on, and then every method reads the
staged copy, so they all hold the same state. `backup` (the complete backup) is
ZIP only, as it was. At least one method must be enabled: `WORLD_ARCHIVE_ENABLED`,
`RDIFF_BACKUP_ENABLED` and `RSYNC_BACKUP_ENABLED` must each be `true` or
`false` (anything else is an invalid setting, not "off"), and all three `false`
is an error (DEV-026, DEV-029). Each method has its own directory, and
directories that are the same or inside one another are refused.

External programs (the dependency matrix for backups):

| Method | Settings | Runs | Needed when |
| --- | --- | --- | --- |
| ZIP | `WORLD_ARCHIVE_ENABLED`, `WORLD_ARCHIVE_PATH` | nothing | never |
| Hard-link snapshots | `RSYNC_BACKUP_ENABLED`, `WORLD_RSYNC_PATH` | nothing (Go; the `rsync` program is not used) | never |
| rdiff-backup | `RDIFF_BACKUP_ENABLED`, `WORLD_RDIFF_PATH`, `RDIFF_BACKUP_ROTATION`, `RDIFF_BACKUP_NICE` | `rdiff-backup`, and `nice` unless `RDIFF_BACKUP_NICE=0` | only when enabled |

The manager's source is Go, but `rdiff-backup` is not: it is a separate Python
program with librsync that this binary does not embed, reimplement or replace.
A missing or unusable program fails only a backup that enables the method, before
the server is touched, with the setting to change and how to install it
(`apt install rdiff-backup`, `brew install rdiff-backup`). If a deployment must
have no non-Go helpers, it cannot enable rdiff-backup; no other archive format
stands in for it, and none is described as rdiff compatible (DEV-028).

#### Hard-link snapshots (`RSYNC_BACKUP_ENABLED`)

This is the Go form of `rsync -aH --link-dest=<dir>/latest <world> <dir>/<timestamp>`.
Whether exact rsync semantics need the external utility was decided, not
assumed: the legacy command only copies a tree and hard-links unchanged files
to the previous copy, and a world holds regular files, directories and (rarely)
links, which Go does fully; the differences are in DEV-027.

```text
WORLD_RSYNC_PATH/<server>/<world>/<YYYY-MM-DD-HH-MM-SS>/<world>/...   a complete copy
WORLD_RSYNC_PATH/<server>/<world>/latest -> <YYYY-MM-DD-HH-MM-SS>     the newest snapshot
```

A file is hard-linked to the snapshot `latest` names when it has the same size,
permission bits and modification time (to the nanosecond), and is otherwise
copied to a new file. Permission bits and times of files and directories, and
links (stored as links, never followed), are kept. An older snapshot is never
changed through a shared inode: a linked file is never opened for writing,
chmod-ed or touched, and a changed file is a new inode, so editing or deleting
a snapshot never alters another. If the filesystem refuses links (`EXDEV`, as
when the previous snapshot is on another filesystem, `EPERM`, `ENOTSUP`) the
rest of the snapshot is copied and a warning says so; a file with too many
links (`EMLINK`) is copied alone. `latest` is believed only when it is a link to
a plain name that is a directory holding this world; otherwise everything is
copied and a warning says why. On a filesystem that keeps whole seconds the
copies never compare equal, so files are copied instead of linked, which costs
space but is never wrong.

A snapshot is built in `.msm-backup-snapshot-<random>.partial`, checked against
the plan (every entry present, with the right kind and size), and published by
a rename that cannot replace an existing snapshot (`renameat2`
`RENAME_NOREPLACE` on Linux, `RENAME_EXCL` on macOS; a second snapshot in the
same second gets `-1`, `-2`...). `latest` is then replaced atomically, so
there is never a moment without it. Failure, cancellation or a kill leaves no
published snapshot and does not move `latest`; the next backup removes the
partial directory. The macOS rename (`RENAME_EXCL`) compiles but has not been
run; P16 owns native macOS evidence.

#### rdiff-backup (`RDIFF_BACKUP_ENABLED`)

```text
WORLD_RDIFF_PATH/<server>/<world>     an rdiff-backup repository
RDIFF_BACKUP_ROTATION                 days of increments to keep (1 to 36500, default 7)
RDIFF_BACKUP_NICE                     -20 to 19, default 19; negative values need root
```

The adapter runs the program directly with an argument vector, never through a
shell: `nice -n <N> rdiff-backup ...`, in the repository directory, with
absolute paths. It asks `rdiff-backup --version` first and uses the command line
that version has: the legacy one (`rdiff-backup <src> <repo>`, then
`--remove-older-than <N>D --force <repo>`) for 1.x to 2.1, and the actions
(`backup`, `--force remove increments --older-than <N>D`, `list increments`)
from 2.2, which deprecates the legacy line, and for 3.x, which drops it. Versions
older than 1.0 and output that is not a version are refused. Only 2.2.6
was run against. The legacy line is the one the legacy manager used, and the
choice of 2.2 as the first version to use actions, and the 3.x line, are
inferred from 2.2.6's behavior and not run; they are tested only with a
stand-in (P16 owns running other versions).

* The repository is the unchanged rdiff-backup format. One made by the legacy
  manager is read and extended: before the server is touched, a non-empty
  directory with no `rdiff-backup-data` is refused (rdiff-backup would refuse
  it), and an existing repository is listed with the installed version, which
  must succeed. This catches an unreadable or incompatible repository, not
  corruption inside it; run `rdiff-backup verify` for that, as it reads every
  file.
* A stopped server's world is backed up in place; a running server's from the
  staged copy, whose permission bits and times are restored first because
  rdiff-backup records them. A repository inside the world is refused.
* An interrupted rdiff-backup is regressed by the program itself on its next
  run; the manager adds no recovery for it. Run time counts against
  `--backup-timeout`, which stops the program.
* Retention runs after a successful backup. `remove increments` exits with
  status 2 when no increment is old enough yet (a young repository, for the
  first `RDIFF_BACKUP_ROTATION` days); that one case, recognized by the status
  and its message, is not a failure. Any other failure of either step is an
  error naming the step; a backup made but not pruned says so.
* rdiff-backup refuses to run twice in one second.

### Privileges

As for worlds: root drops to the server's owner before writing anything, and a
non-root caller must be that owner.

### Tests

`internal/backup` uses a fake console that records every command in order and
lets a test act as the server. They cover the layouts, modes and times, the
inactive-world scope, link policies (stored, followed, external, cycle, the
archive directory through a link), the live sequence (the archive holds the
saved state, not what the server wrote after `save-on`), saving re-enabled
after a refused or unanswered command, cancellation, a failed copy and a
timeout, recovery after a killed manager and with a corrupt or hostile
marker, `ENOSPC` and permission errors (stopped and live) leaving no archive,
no overwrite, refusals, RAM worlds and a restore into a fresh directory
compared with the original (a test helper, not a command).

The P11 tests cover the snapshots (first copy with modes, times, empty and
read-only directories and stored links; unchanged files shared by inode while
changed, re-moded, added and removed files are not, and an older snapshot
untouched in content, mode, time and name; link failures from another
filesystem, no hard-link support and too many links; a world on another
filesystem; an untrusted `latest`; failure, cancellation and a file that
changes while copied leaving nothing and not moving `latest`; same-second
names; a directory called `latest`; sweeping), the settings (flags,
retention, priority, overlapping directories, nothing enabled), the adapter
(missing program and `nice`, each version's command line, an unreadable
version, a repository that is not one or cannot be read, a failing backup or
retention, the exit-2 case), all methods together and in order, and a running
server whose rsync and rdiff-backup runs read the staged copy after saving
resumed. `TestRealRdiffBackup` runs the real rdiff-backup (skipped when none
works, as on CI runners without it) against a repository made with the legacy
command line, then a second and a running-server backup, `list increments` and
`verify`. rsync is never run by any test.

`TestWorldEditSnapshotAcceptance` runs WorldEdit's own
`FileSystemSnapshotDatabase` and archive/date helpers (unmodified source,
pinned to a commit of its `version/7.3.x` branch, with stubs for the code that
reads chunks) over a real backup and checks it lists the snapshot, with its
timestamp and `level.dat`; a control archive without the world folder is
rejected. It needs a JDK and network access to build, so it is skipped unless
`MSM_WORLDEDIT_HARNESS` names a directory built with
`compatibility/worldedit/build.sh`; Linux CI builds it and sets the variable.
`TestWorldEditSnapshotLayout` is a Go mirror of the same rules that always
runs. No live Minecraft server with the WorldEdit plugin and a `//snapshot`
command was run: that is P16 work.

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
