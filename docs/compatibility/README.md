# Go port compatibility contract

This directory defines the behavior that the Go port must account for before it
can claim parity with Minecraft Server Manager (MSM).

## Baseline

- Repository: `msmhq/msm`
- Commit: `7a9d120d6b93f2f62178a60416955eea2065c5ce`
- MSM version declared by that commit: `0.11.0`
- Contract issue: [#1](https://github.com/thehatchcloud/msm/issues/1)

The checked-in Bash implementation remains the executable reference during the
port. It is not automatically correct: `deviations.md` records behavior that the
Go implementation must intentionally change.

## Machine-readable inventories

| Fixture | Contract |
|---|---|
| `compatibility/commands.tsv` | Every one of the 69 registered command signatures, aliases, handler, owning task, and future contract-test ID |
| `compatibility/settings.tsv` | Every registered global setting and per-server setting, including defaults and empty profile-derived values |
| `compatibility/surfaces.tsv` | Filesystem conventions, state flags, version profiles, maintenance jobs, service integration, completion, prompts, and exit behavior |
| `compatibility/fixtures/minimal/` | Inert example installation copied into a temporary directory by the verifier |

Run `./scripts/verify-compatibility-baseline.sh` after changing a legacy source
surface. The script reads files as text; it never sources or invokes `init/msm`.
It fails when command or setting registrations drift without an accompanying
contract update.

P02 will move these checks into the Go test harness. This small shell verifier is
repository tooling, not manager runtime.

## Compatibility rules

### Command grammar and targeting

- Preserve the server-first syntax: `msm <server> <command>`.
- `<string>` accepts one shell argument. `<strings>` consumes one or more
  remaining arguments and the handler joins them with spaces.
- `<flags>` consumes a contiguous run of dash-prefixed arguments. The baseline
  updater recognizes only `--noinput`.
- A `|` inside a fixed signature token exposes aliases, such as
  `whitelist|wl`.
- `<name>` permits ASCII letters, digits, `_`, and `-`. It rejects `start`,
  `stop`, `restart`, `version`, `server`, `jargroup`, `all`, `config`,
  `update`, `help`, and names beginning with `--`.
- `<name:server>` accepts the special target `all`. `<name:world>` accepts
  `all` only when the server target is also `all`; a single server may also
  target all of its worlds through the dispatcher.
- Quoted values are argument-boundary behavior supplied by the invoking shell.
  The Go CLI must consume the already-separated `argv` values and must not
  re-evaluate quotes or metacharacters.

### Active state and lifecycle

The server's `active` marker records intent and is separate from screen/JVM
liveness:

| Operation | Running targets | Intent after operation |
|---|---|---|
| Global `start` | Starts every active server that is stopped | Unchanged |
| Global `stop [now]` | Stops every running server, including an unexpectedly running inactive server | Unchanged |
| Global `restart [now]` | Stops every running server, then starts the active set | Unchanged |
| `<server> start` | Starts the selected server | Active |
| `<server> stop [now]` | Stops the selected server | Inactive |
| `<server> restart [now]` | Restarts or starts the selected server | Active |

`now` removes the warning and delay. It is not permission to skip `save-all`,
normal Minecraft shutdown, wait-for-stop, or RAM-world synchronization.

The source accepts `all` for any registered `<name:server>` command, including
commands not advertised by `help`. The Go port will preserve useful bulk
targeting, but P01 treats the hidden breadth as a source/documentation
disagreement that P14 must expose and test consistently.

### Output, errors, and prompts

- Normal command results and progress go to stdout.
- Warnings and errors go to stderr; color is presentation, not contract.
- The legacy reference returns numeric codes `64` through `73` in its tests,
  but some source call sites pass symbolic or otherwise undefined names. P14
  will define stable named Go exit codes and map compatibility tests to them.
- Unknown commands print `No such command. See <program> help`; the reference
  commonly exits zero because `main` ends with `exit 0`. The Go port will
  return a nonzero usage error.
- `server delete` and `jargroup delete` prompt on stdin and default to no.
  `update` prompts by default; `--noinput` answers yes.
- Noninteractive execution must never wait indefinitely. Destructive
  operations will require an explicit flag or confirmed terminal prompt.
- Exact progress punctuation, color escapes, countdown cursor control, and
  historical misspellings are not compatibility requirements. Meaning,
  stdout/stderr destination, machine-readable mode, and success/failure status
  are.

### Configuration

The Bash implementation reads the last matching `NAME=value` assignment from
`MSM_CONF` or `/etc/msm.conf`, strips optional matching single/double quotes,
and then uses `eval`. Per-server overrides are `msm-<lowercase-dash-name>` keys
inside `server.properties`.

The Go port will accept literal assignments and the `{SERVER_NAME}`, `{RAM}`,
`{JAR}`, and `{DELAY}` substitutions used by MSM. It will not execute shell
expressions, command substitutions, redirections, pipelines, or environment
expansion embedded in configuration.

Empty `CONFIRM_*` defaults are populated by the selected version profile. The
five shipped profiles form an inheritance graph:

```text
minecraft/1.2.0
├── minecraft/1.3.0
│   ├── minecraft/1.7.0
│   └── craftbukkit/1.3.0
└── craftbukkit/1.2.0
```

### Files and backups

- Servers live directly below `SERVER_STORAGE_PATH`.
- Each server owns `server.properties`, its active `worldstorage`, inactive
  `worldstorage_inactive`, the active marker, and the in-world `inram` flags.
- Active worlds are linked into the server root. A RAM-enabled active world is
  linked to its RAM copy instead.
- Shared JAR groups live below `JAR_STORAGE_PATH`. `target.txt` stores the
  download target and `downloads/` is temporary staging.
- World ZIPs are stored by server and world. Full backups are stored by server.
  Logs are archived by server.
- World-only backup excludes inactive worlds. Full backup includes the server
  directory, including inactive world storage.
- ZIP world archives, optional rdiff-backup repositories, and optional
  hard-linked rsync snapshots are distinct capabilities. None may be silently
  dropped.
- Live world backup performs save-off, save-all, RAM-to-disk sync, backup, then
  save-on. The reference lacks crash-safe cleanup; P10 must add it.

### Reference tests

The upstream `test.sh` contains 30 test functions. They cover a useful subset of
names, instance CRUD, stopped-server behavior, basic backup/JAR behavior, and
JAR-group CRUD. They do not cover global lifecycle, running-server behavior,
player commands, version-profile parsing, RAM failure, restore, concurrency,
or most error paths.

The sample `msm.conf` contains 42 assignment keys. One,
`DEFAULT_PROPERTIES_PATH`, is not registered or read by `init/msm`; the source
instead uses an internal `SERVER_PROPERTIES` setting. It remains in
`settings.tsv` as a `legacy-config` input so migration cannot silently ignore
an administrator's configured filename.

The Go contract tests named in the TSV files are planned identifiers. They
become executable in their owning implementation tasks. P01's executable check
only verifies that the source inventory and inert fixtures have not drifted.

## Source/documentation disagreements

| Topic | Source behavior | Public/help behavior | Go-port decision |
|---|---|---|---|
| `all` targeting | Dispatcher permits `all` in every `<name:server>` signature | Cron uses `all`; help emphasizes global lifecycle and individual server syntax | Preserve safe bulk operations and document them consistently in P14 |
| Global stop intent | Does not remove active marker | Public global docs say intent is unchanged | Preserve |
| Per-server stop intent | Removes active marker before stopping | Public server docs describe inactive state | Preserve |
| `server delete` while running | Prompts, then stops and deletes | Help only says delete | Keep explicit prompt/flag; preview target and fail safely |
| `jargroup rename` | Does not update server JAR symlinks; source contains TODO | Help implies rename succeeds | P07 must update/refuse affected references atomically |
| Unknown command status | Prints a message and normally exits zero | No status documented | Return nonzero usage status |
| Update transport | Downloads unverified files with TLS checks disabled | Described simply as update | Replace with verified release artifacts |
| Current game syntax | Profiles stop at Minecraft 1.7-era parsing | Project description implies general Minecraft support | Retain legacy profiles and add an explicitly tested modern profile |
| Properties filename | `init/msm` registers `SERVER_PROPERTIES` | `msm.conf` exposes `DEFAULT_PROPERTIES_PATH`, which the source never reads | Import either spelling with a warning; write one canonical Go setting in P03 |

## Ownership map

- P03 owns settings, filesystem safety, literal configuration, and state
  compatibility.
- P04 owns screen behavior and process identity.
- P05 owns instance CRUD.
- P06 owns lifecycle, active state, `now`, and bulk behavior.
- P07 owns JAR groups and downloads.
- P08 owns profiles, console, connected players, and game commands.
- P09 owns world discovery, links, and active/inactive state.
- P10 and P11 own backup modes and recovery.
- P12 owns RAM worlds.
- P13 owns log rolling and scheduled maintenance.
- P14 owns complete CLI grammar, help, completions, output, and exit codes.
- P15 owns installation, services, releases, and updates.
- P16 owns migration, rollback, and native platform validation.
