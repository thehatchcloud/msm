# Command line reference

`msm` keeps the original server-first syntax, `msm <server> <command>`, and
the original command list (`msm help` prints it verbatim). This page covers
what the list does not: targets, output formats, exit statuses, settings,
unattended use and tab completion. Behavior that differs from the Bash
manager is listed in [compatibility/deviations.md](compatibility/deviations.md);
[compatibility/parity.md](compatibility/parity.md) maps every legacy command,
setting and behavior to its implementation and tests.

`msm help <topic>` prints the short form of the sections below for the topics
`targets`, `formats`, `exit-codes` and `noninteractive`; `msm help <command>`
prints a command's own help.

## Targets

Anywhere a command takes a server, `all` runs it on every server and reports
each one under the server's name. Start, stop and restart handle up to `--jobs`
servers at once; failures are reported together and make the exit status
non-zero. `console` and `cmdlog` need exactly one server, and `worlds ram`
changes one server's world at a time. `msm all worlds on|off` needs `all`
worlds too, as in the legacy manager.

`msm all stop` marks every server inactive, as `msm <server> stop` does. The
global `msm stop` stops the servers and leaves them marked active, so
`msm start` (for example from cron) brings them back.

The words `start`, `stop`, `restart`, `version`, `server`, `jargroup`,
`config`, `update`, `install`, `migrate`, `help`, `maintenance` and `all` can never be server names (DEV-036, DEV-038).

## Global flags

| Flag | Meaning |
|---|---|
| `--config <file>` | native configuration file (not `msm.conf`; that is `MSM_CONF`) |
| `--debug` | diagnostic output on standard error |
| `--format text\|json` | output format; see [Machine-readable output](#machine-readable-output) |
| `--timeout <duration>` | how long a start, stop or console answer may take |
| `--jobs <n>` | servers handled at once by a command for several servers |
| `--backup-timeout <duration>`, `--allow-external-links` | backups |

A malformed flag, an unknown flag or the wrong number of arguments is a usage
error (exit status 2) and changes nothing.

## Exit statuses

The Bash manager exited 0 after nearly everything, including unknown commands
and failed operations (DEV-004). `msm` uses these statuses, which are stable:

| Code | Name | Meaning |
|---|---|---|
| 0 | `ok` | the command did what was asked |
| 1 | `error` | the command failed, or at least one server of a bulk command failed |
| 2 | `usage` | no such command, wrong number of arguments, or a bad flag; nothing was done |
| 3 | `config` | a configuration file, a setting or its value cannot be used; nothing was done |
| 4 | `not-found` | the server, world, JAR group or property does not exist |
| 5 | `refused` | declined for safety: confirmation missing, server not proven stopped, name taken, another command holds the lock |
| 6 | `unavailable` | a program or feature the command needs is missing (GNU screen, rdiff-backup), or the build cannot do it (`update` in a development build, which has no release signing key) |
| 130 | `interrupted` | stopped by Ctrl+C or a termination request |

Standard output carries results and progress; warnings and errors go to
standard error, one `msm: ...` line per failure. When several things fail at
once the status names the first cause in the order of the table above, so it
does not depend on which server finished first.

## Unattended use

`msm` never waits for input that cannot come. Only three commands ask a
question: `server delete` and `jargroup delete` (destructive), and `update`. On a
terminal they print what will be removed and ask `[y/N]` (only `y`, `Y` or
`yes` confirms). When standard input is not a terminal they print the plan
and refuse with status 5 unless `--yes` is given. Every other command either
runs or refuses without asking.

| Command | Destructive? | Authorization |
|---|---|---|
| `server delete`, `jargroup delete` | removes data | `--yes`, or an answer on a terminal; a running server or a group in use is refused regardless |
| `maintenance prune` | removes old archives | `--dry-run` to preview; the scheduler's explicit `maintenance run prune` |
| `stop`, `restart` (also `all`) | interrupts players | players are warned first; `now` skips the countdown, never the save |
| `<server> config <key> <value>` | rewrites `server.properties` | atomic, keeps comments and unknown keys; identity settings only while stopped |
| `update` | replaces the `msm` executable | a `[y/N]` answer on a terminal, or `--noinput`; refuses with status 5 otherwise. Only a release signed by a key compiled into the build is accepted (see [Updating](#updating)) |

## Updating

`msm update [--noinput]` replaces the `msm` executable with the latest release
published for this fork (DEV-006). It never fetches or runs a script, and it
reads and writes nothing but the executable, a `.previous` copy of it and a
lock and staging files beside it: configuration, servers, worlds and backups
are untouched.

1. The release manifest and its signature are fetched over HTTPS from the
   location compiled into the build. The signature must verify against an
   ed25519 key compiled into the build; the network and the host serving the
   files are not trusted.
2. The artifact for exactly this operating system and architecture is chosen
   (`linux` or `darwin`, `amd64` or `arm64`); there is no fallback.
3. A release older than the installed version is refused; the same version is
   "up to date".
4. After the `[y/N]` answer, the archive is downloaded next to the executable
   (so on the same filesystem), checked against the signed size and SHA-256,
   and read in memory: it must hold exactly `msm`, `LICENSE.markdown`,
   `SHA256SUMS` and `RELEASE`, the binary must match `SHA256SUMS`, and
   `RELEASE` must name the signed version, commit, OS and architecture.
5. The new program is staged beside the old one with its permissions and, as
   root, its owner, and its `version` command is run (no shell) to confirm it
   starts and reports the signed version and commit.
6. The old program is kept as `<msm>.previous` and the new one is renamed over
   it atomically. At every moment the executable is a complete working
   program; an interrupted update leaves the old one, and the next update
   removes its leftovers.

`--check` stops after step 3. `--rollback` swaps the `.previous` program back
(and keeps the program it replaced as the new `.previous`).

It refuses (status 5) when the executable is under a package manager's
directory (`/usr/bin`, a Homebrew `Cellar`, `/nix/store`, ...; update through
that package manager), when the directory is not writable by you (for
example, run it with `sudo` for a root-owned `/usr/local/bin`), or when another
update holds the lock. A symlinked `msm` is followed: the real file is
replaced and the link is kept. A development build has no signing key, so it
exits with status 6; so does a build whose version is not `X.Y.Z`.

## Installation helpers

`msm install check` reports whether GNU screen and Java are on `PATH` and, if
not, the command to install them; it installs nothing. `msm install config`
writes a minimal `msm.conf` and creates the data directories, and `msm install
service` prints (or with `--write` installs, and with `--write --enable`
enables for the next boot, never starting it) a systemd unit or launchd
daemon. See [install.md](install.md).

## Migration

`msm migrate preflight` inventories a legacy Bash installation and reports what
cannot move; it changes nothing. `msm migrate apply` backs the installation up,
disables the legacy cron file, service and executable, and only then installs
and enables the Go executable, schedule and service; `msm migrate rollback`
restores the legacy ones. All need a stopped maintenance window, and apply and
rollback need root (or `--root DIR` to rehearse on a copy). Exit status 5 means
a blocker, a running server, an earlier migration or a missing confirmation;
2, no backup choice. See [migration.md](migration.md).

## Settings

`msm config` prints the global settings as `NAME="value"` lines, followed by
the `DEFAULT_` settings that a server's own `msm-*` properties override. A
value is what `msm.conf` sets (`MSM_CONF`, else `/etc/msm.conf`), the built-in
default, or, for an installation with no `msm.conf`, the data directory `msm`
detected. `RAMDISK_STORAGE_ENABLED` is shown as `false` unless set: RAM worlds
are off until you enable them (DEV-008).

`msm <server> config` lists the server's settings, `msm <server> config <key>`
prints one value and `msm <server> config <key> <value>` writes one.

* `msm-<name>` keys (`msm-ram`, `msm-stop-delay`, ...) are the manager's
  per-server settings. Listing and reading show the effective value, whether
  it comes from the server, `DEFAULT_*` in `msm.conf`, the version profile or
  the built-in default. Any other key is a Minecraft property in
  `server.properties`, matched case-sensitively as Minecraft does.
* A write changes that line in place (every copy of it, since the last one
  wins), appends the key if it is new, and leaves every comment, blank line
  and unknown key byte for byte as it was. The file is replaced atomically
  under the server's lock and keeps its permissions. It must already exist.
* A key or value that could change another line (a line break, an empty key,
  a key containing `=`), an `msm-*` key that no setting answers to, and a
  value the manager cannot use (`msm-ram` that is not a positive number, an
  `msm-invocation` that is not a plain command line) are refused with nothing
  written (DEV-016, DEV-032). An empty `msm-*` value makes the setting fall
  back to its default.
* The write says when it takes effect, because Minecraft reads
  `server.properties` once, at start:

  | Setting | Effect |
  |---|---|
  | `msm-stop-delay`, `msm-restart-delay`, `msm-message-*`, `msm-confirm-*`, `msm-version`, `msm-ops-list`, `msm-whitelist-path`, `msm-banned-players-path`, `msm-banned-ips-path`, `msm-ops-path`, `msm-flag-active-path`, `msm-complete-backup-follow-symlinks` | the next `msm` command |
  | Minecraft properties, `msm-log-path`, `msm-world-storage-path`, `msm-world-storage-inactive-path`, `msm-worlds-flag-inram` | a running server keeps the old value until `msm <server> restart`; the message says so, or says the manager could not tell |
  | `msm-screen-name`, `msm-username`, `msm-ram`, `msm-jar-path`, `msm-invocation` | only while the server is stopped (status 5 otherwise): the manager finds the running server by these, so changing them under it would lose it |

## Machine-readable output

`--format json` prints a JSON document on standard output instead of text,
for `version`, `config`, `<server> config` (the listing), `server list` and
`<server|all> status`. Any other command refuses it with a usage error, so a
program never gets text where it expected JSON. Warnings stay on standard
error and the exit status is the same as for text.

Every document has `schema_version`, currently `1`. A field is never removed,
renamed or changed in meaning without incrementing it; new optional fields may
appear within a version, so parse leniently. The status document is specified
by [schema/status-v1.json](schema/status-v1.json):

```json
{
  "schema_version": 1,
  "servers": [
    {"name": "survival", "active": true, "state": "running", "ok": true},
    {"name": "creative", "active": true, "state": "stopped", "ok": false},
    {"name": "old", "active": false, "state": "occupied", "ok": false,
     "detail": "screen session msm-old is not this server's"}
  ]
}
```

`state` is `running`, `stopped`, `occupied` (a screen session holds the
server's name but is not verifiably the server) or `unknown` (the sessions
could not be inspected). `active` is the server's intent. `ok` is true when an
active server is running or an inactive one is stopped. A monitoring check is
`msm all status --format json | jq -e 'all(.servers[]; .ok)'`.

The other documents: `version` is `{"schema_version", "version", "commit"}`;
`config` is `{"schema_version", "settings": [{"name", "value", "source"}]}`
with `source` one of `msm.conf`, `default`, `detected`; `<server> config` is
`{"schema_version", "server", "settings": [{"key", "value", "source"}]}` with
`source` one of `default`, `msm.conf`, `version profile`, `server.properties`.
Their shapes are fixed by tests.

Ordinary text output stays readable for operators and keeps the legacy
wording where the legacy manager had it; its exact punctuation is not a
contract, but its meaning and its split between standard output and standard
error are.

## Tab completion

`msm completion bash|zsh|fish|powershell` prints a completion script. The
scripts only run the `msm` binary (`msm __complete ...`), which reads the same
places the legacy script listed with `ls`, `cat` and `find`, in Go:

* servers, `all` and the per-server command words, with the aliases accepted
  but not offered;
* `now` after `stop` and `restart`;
* the worlds of a server (active ones for `worlds ram|off`, inactive ones for
  `worlds on`), JAR groups and the JARs in a group (`jar`, `jargroup`);
* player names for `whitelist remove`, `blacklist player|ip remove` and
  `operator remove`, read from the server's JSON lists;
* `gamemode`, `time`, `save` and `config` arguments, including the keys of
  the server's own `server.properties`.

Candidates are filtered to letters, digits and `_ . : @ + = , -` (at most 128
characters), so a world directory, a player name or a property key containing
a newline, space, quote or shell metacharacter is not offered rather than
escaped. Player lists are read up to 4 MiB. Completion prints nothing but
candidates: a configuration it cannot read, or a file it cannot parse, just
offers fewer names. It never sources the init script into your shell.

```sh
# Bash with the bash-completion package (current shell; for every shell put
# the line in ~/.bashrc, or save the output as /etc/bash_completion.d/msm)
source <(msm completion bash)

# zsh (put _msm in a directory of your $fpath, then restart the shell)
msm completion zsh > "${fpath[1]}/_msm"
```

The same scripts are checked in under [completions/](../completions/), where a
test keeps them identical to what the binary prints. The legacy
`bash_completion/msm` stays as the reference until the installers (P15)
replace it.
