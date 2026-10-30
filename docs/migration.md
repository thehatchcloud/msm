# Migrating from the Bash manager, and rolling back

This moves an installation made by the legacy Bash manager (the `installers/`
scripts, `/etc/init.d/msm`, `/etc/cron.d/msm`) to the single Go executable, and
back. It is a deliberate, supervised operation in a stopped maintenance window.
Nothing here deploys to a host by itself, and CI only ever runs it on copies
([Rehearsal](#rehearse-it-first)). Legacy installations exist on Linux only, so
`migrate` is for Linux; macOS is validated for clean installs
([install.md](install.md), [validation.md](validation.md)).

What it does **not** do: convert, move or write any world or server file.
`msm.conf` is read by both managers as it is, so it is not converted either.
The two things that change are the *control plane* (who runs the scheduled jobs
and the boot service, and what `msm` means) and, for the Go side, new files.

## The three commands

```sh
msm migrate preflight                    # read-only inventory and plan
sudo msm migrate apply --backup-servers  # the migration
sudo msm migrate rollback                # undo it, or finish undoing an interrupted one
```

`--root DIR` makes all three read and write below `DIR` instead of `/`, and
prints the `systemctl` commands instead of running them. `--legacy-conf` and
`--state-dir` (default `/var/lib/msm-migration`) override the two locations.

## 1. Preflight (read-only)

`msm migrate preflight` takes no lock, writes nothing and starts nothing
(a test compares the whole tree, with modification times, before and after). It
reads:

| Area | What is inventoried | Reported as a blocker when |
|---|---|---|
| config | `msm.conf` (global settings), unknown keys | it does not exist, or a line is not a literal `NAME=value`: `$(...)`, backticks, `$VAR`, `;`, `&`, `\|`, redirections (the Go port never evaluates shell, DEV-001) |
| users | the manager user and each server's owner (`USERNAME`) | a named user does not exist; a directory owned by another uid is a warning |
| servers | every directory below `SERVER_STORAGE_PATH`, its `SCREEN_NAME`, `VERSION`, JAR link | `msm-invocation`/`INVOCATION` cannot be split into plain words (shell syntax, unbalanced quotes) |
| active flags | which servers carry `FLAG_ACTIVE_PATH`, recorded so the verification can prove the intent survived | |
| screen sessions | whether each server is running, read through `screen -ls` as its owner | a server is running, or cannot be proven stopped |
| worlds | active and inactive worlds, which carry the RAM flag | (warning for RAM worlds: see [ram-worlds.md](ram-worlds.md), DEV-008) |
| JAR groups | each group, its `target.txt` URL, its JARs | (warning for an `http://` target: the Go port refuses insecure downloads) |
| archives | `WORLD_ARCHIVE_PATH`, `BACKUP_ARCHIVE_PATH`, `LOG_ARCHIVE_PATH`, rdiff-backup and rsync modes | |
| schedule | every line of `/etc/cron.d/msm`, and what it becomes | a line that cannot be translated (a script, a pipeline, a command that is not `msm`) |
| customizations | `/etc/profile.d/msm.sh` (sourced by the legacy init script), versioning scripts, the command link, existing Go files | the command link points elsewhere, or a Go file is already in place |

Exit status 5 when anything blocks. Unsupported cron lines are the one blocker
you may accept (`--drop-unsupported`): they are kept as comments in the new
cron file and in the backup, never silently lost.

## 2. The window

Stop the servers first (`msm stop now`, or `msm <server> stop`). Apply and
rollback both refuse unless every server is proven stopped, and the check is
repeated under the migration lock. Nothing is ever started by a migration: when
the window ends, `msm start` (or `systemctl start msm`) starts the servers that
are marked active, exactly the set that was active before.

## 3. Apply

```sh
sudo msm migrate apply --backup-servers     # msm makes a complete backup of every server first
sudo msm migrate apply --external-backup    # you made and verified your own (snapshot, copy)
```

One of the two is required: apply will not proceed without a statement of how
the server data is backed up. `--dry-run` runs the preflight and stops. It asks
for confirmation unless `--yes`. It takes `/var/lib/msm-migration/lock`, so two
migrations cannot run at once, and refuses if a migration is already recorded
there.

The steps, in this order. Each is written to `journal.json` before it starts
and after it ends, so an interruption (a crash, a power loss, Ctrl+C) leaves a
record that `msm migrate rollback` completes:

1. **backup**: copies `msm.conf`, `/etc/cron.d/msm`, `/etc/init.d/msm`,
   `/etc/profile.d/msm.sh`, the completion file, the legacy systemd unit (if
   any) and the `msm` link into `…/backup/` with a manifest of mode, owner,
   SHA-256 and link target, and re-reads every copy.
2. **fingerprint**: a hash of the path, size, mode and time of every entry in
   every world directory (no content is read), taken now and compared at the end.
3. **data-backup**: `--backup-servers` runs the same complete backup as
   `msm <server> backup` for each stopped server (the archive lands in
   `BACKUP_ARCHIVE_PATH` as usual).
4. **disable-scheduler**: removes `/etc/cron.d/msm`. (Renaming would not work:
   some cron implementations read any name.)
5. **disable-service**: `systemctl disable msm.service` when it was enabled,
   removes the legacy unit file if there was one, `systemctl daemon-reload`. On
   a system without `systemctl` the next step is what stops a SysV script.
6. **disable-executable**: clears the execute bits of `/etc/init.d/msm`
   (a runlevel script that is not executable is not run, and neither is
   `/etc/init.d/msm start` typed by hand) and removes the `msm` link.
7. **verify-legacy-disabled**: re-checks all of the above. **Only now is the Go
   executable installed.** This is what guarantees the two managers never write
   at the same time: no cron entry, boot service or command of the old one is
   left when the new one appears.
8. **install-binary**: installs the Go executable at `/usr/local/bin/msm`
   (staged in the same directory and renamed; it refuses a script, and a
   different file already there).
9. **install-schedule**: writes `/etc/cron.d/msm-maintenance`. The four jobs of
   the legacy file keep their schedule and user and run `msm maintenance run
   backup|logroll|todisk|recover`; the four `find | xargs rm` lines become one
   `maintenance run prune --days N [--only …]` job (the largest age wins, so
   nothing is pruned earlier than before); other `msm …` lines with plain words
   keep their arguments. See [maintenance.md](maintenance.md).
10. **install-service**: writes `/etc/systemd/system/msm.service` (the unit
    `msm install service` prints) and enables it. Not started.
11. **verify**: re-reads the configuration the Go manager will use and checks
    the same servers exist, the same ones are active, each world fingerprint is
    unchanged, and every file written is what was written.

If any step fails, everything done so far is undone in reverse (the same code
as rollback) before the error is returned. If the undo itself fails (for
example `systemctl` is broken), the message says so and
`msm migrate rollback` finishes it once the cause is fixed.

## Rollback

```sh
sudo msm migrate rollback
```

Needs the same stopped window and the same lock. It undoes the steps in reverse:
disables the Go service and removes its unit; removes the Go cron file and the
Go executable; restores the legacy executable's mode and the `msm` link; puts
back the legacy unit (and re-enables the service if it had been enabled) and
the legacy cron file, with the recorded mode and owner. If `msm.conf`, the
profile or the completion file differ from the backup they are restored too.

* **Nothing is destroyed.** Anything rollback removes or replaces (for example a
  Go executable you updated with `msm update` meanwhile) is first copied to
  `…/saved/`.
* **Worlds are not touched.** Neither apply nor rollback writes inside a
  server directory, so rollback cannot replace world data with older data and
  cannot downgrade it: the Go manager does not change the world format.
  Anything the Go manager did *while it ran* (a backup, a RAM sync, new chunks)
  stays; the legacy manager reads the same layout. If you ran servers under the
  Go manager and RAM worlds are flagged, run `msm all worlds todisk` **before**
  rolling back.
* The state directory keeps the backup and the journal (`history/` after a
  second apply). Delete it when you are satisfied.

After a rollback the legacy cron file is back, so cron runs the legacy jobs
again, and the legacy service is enabled again: start the servers with
`/etc/init.d/msm start`.

## Rehearse it first

[`compatibility/migration/rehearse.sh`](../compatibility/migration/rehearse.sh)
builds a legacy tree from this repository's shipped `msm.conf`, `cron/msm` and
`init/msm` in a temporary directory and runs preflight, apply, a second apply
(which must be refused) and rollback with `--root`, then compares every path,
mode and checksum with the tree it started from. CI runs it on Linux and
macOS, and `internal/migrate` runs the same sequence with failures injected at
each `systemctl` call and a crash in the middle. To rehearse your own
installation, copy `/etc/msm.conf`, `/etc/cron.d/msm`, `/etc/init.d/msm`, the
link, and `SERVER_STORAGE_PATH` (the servers may be stripped of worlds) below a
scratch directory with the same layout, and run:

```sh
msm migrate preflight --root /scratch
msm migrate apply --root /scratch --external-backup --yes --binary ./msm
msm migrate rollback --root /scratch --yes
```

`--root` never runs `systemctl` and never changes a real service.

## What the migration cannot judge

* **Shell customizations are reported, not converted.** A cron line that calls
  your own script, a `profile.d` file that exports `JAVA_HOME`, versioning
  scripts, a `msm.conf` that uses `$(…)`: the preflight names each one, and you
  rewrite it (an `INVOCATION` with the Java path spelled out, a systemd
  `Environment=`) before the window.
* **A real Minecraft server.** The validation in [validation.md](validation.md)
  runs a JVM stand-in, not Mojang's server. Migrate a staging copy of your
  server first.
* **Production.** This tool and its tests authorize no production deployment.
