# Scheduled maintenance, log rolling and retention

The legacy `cron/msm` file ran eight jobs from cron as the `minecraft` user.
The Go port keeps scheduling in the host OS (systemd, cron or launchd): there
is no resident scheduler. The OS starts the same `msm` binary you run by
hand, and each job is an `msm` command. **Nothing here is installed by MSM**;
you opt in by copying the example files in
[`examples/scheduling`](../examples/scheduling) (steps below). The
installers (P15) may offer to do the same later.

## The jobs

Every line of the upstream `cron/msm` (commit
`7a9d120d6b93f2f62178a60416955eea2065c5ce`), and what replaces it:

| Legacy cadence | Legacy command | Go job | Cadence in the examples |
|---|---|---|---|
| `02 05 * * *` | `/etc/init.d/msm all worlds backup` | `maintenance run backup` (= `msm all worlds backup`) | daily 05:02 |
| `55 04 * * *` | `/etc/init.d/msm all logroll` | `maintenance run logroll` (= `msm all logroll`) | daily 04:55 |
| `*/30 * * * *` | `/etc/init.d/msm all worlds todisk` | `maintenance run todisk` (= `msm all worlds todisk`) | every 30 minutes |
| `@hourly` | `/etc/init.d/msm start` | `maintenance run recover` (= `msm start`) | hourly |
| `10 05 * * *` | `find /opt/msm/servers/*/logs -maxdepth 1 -mtime +30 -type f -name "*.log.gz" \| xargs rm --force` | `maintenance run prune` (= `msm maintenance prune`), category `server-logs` | daily 05:10, 30 days |
| `12 05 * * *` | `find /opt/msm/archives/backups -maxdepth 2 -mtime +30 -type f -name "*.zip" \| xargs rm --force` | `maintenance run prune`, category `backups` | daily 05:10, 30 days |
| `14 05 * * *` | `find /opt/msm/archives/logs -maxdepth 2 -mtime +30 -type f -name "*.log.gz" \| xargs rm --force` | `maintenance run prune`, category `log-archives` | daily 05:10, 30 days |
| `16 05 * * *` | `find /opt/msm/archives/worlds -maxdepth 3 -mtime +30 -type f -name "*.zip" \| xargs rm --force` | `maintenance run prune`, category `world-backups` | daily 05:10, 30 days |

* **Backup** and **todisk** act on active worlds; a world in RAM is written to
  disk first ([ram-worlds.md](ram-worlds.md)). With `todisk` every 30
  minutes, a power loss costs at most about that much play.
* **recover** is "intended-active crash recovery": `msm start` starts every
  server whose `active` marker is set and that is stopped. A server you
  stopped with `msm <server> stop` has no marker and stays stopped.
* The four retention lines became one job. The cadence and the 30 days are
  the legacy ones; they are not settings in `msm.conf`. Change them with
  `--days`, `--keep` and `--only` on the command line of your unit.
* The retention roots are the settings the commands that write the files use:
  `SERVER_STORAGE_PATH`, `BACKUP_ARCHIVE_PATH`, `LOG_ARCHIVE_PATH` and
  `WORLD_ARCHIVE_PATH`. The legacy cron hard-coded `/opt/msm/...`, so a
  changed setting left its files unpruned; the Go job follows the settings.
* `rdiff-backup` repositories and rsync-style snapshots keep their own
  retention (`RDIFF_BACKUP_ROTATION`, `RSYNC_BACKUP_ROTATION`) and are not
  touched by `prune`.

## Log rolling

`msm <server> logroll` (and `msm all logroll`) archives a server log to
`<LOG_ARCHIVE_PATH>/<server>/<server>-<YYYY-MM-DD-HH-MM-SS>.log.gz` and
replaces the log with the legacy one-line note naming that directory.

**The policy is: a log is rolled only while its server is stopped.** The
command holds the server's lock and proves the server is stopped (the same
proof `worlds on` and server deletion use). A running server is *skipped*,
successfully, with a message, and its log is not touched.

Why not copy-and-truncate while running, as the legacy command did? It
cannot be lossless:

* Every line written between the copy and the truncate is lost.
* If the writer does not open the file in append mode (the log4j appender
  Minecraft 1.7 and later uses keeps its own file offset), truncating leaves a
  hole of NUL bytes at the old offset when it writes next.
* Renaming the file does not help: the server keeps writing to the renamed
  file.

By server version:

| Server | Log | What happens |
|---|---|---|
| Minecraft 1.2 to 1.6 and CraftBukkit (profiles `minecraft/1.2.0`, `1.3.0`, `craftbukkit/*`) | `server.log` | Rolled when the server is stopped. A server that runs for weeks keeps appending to `server.log` until it is stopped or restarted; roll it as part of a planned restart (`msm <server> stop`, `msm <server> logroll`, `msm <server> start`). |
| Minecraft 1.7 and later, Paper, Spigot (profiles `minecraft/1.7.0` and `minecraft/1.13.0`) | `logs/latest.log` | The server itself rolls `latest.log` into `logs/*.log.gz` at start and by day. `msm logroll` archives the current `latest.log` when stopped; `prune` category `server-logs` removes the server's own rolled files. |

The nightly `logroll` job therefore does useful work for servers that were
stopped and is a quiet no-op ("Skipped: the server is running") for servers
that were up.

How the archive is made, for a stopped server:

1. The log must be a regular file inside the server directory (`LOG_PATH`
   outside it, a link, or a link in its directory is refused).
2. It is compressed to a temporary file, flushed, and re-read: the archive
   must decompress to exactly the log's bytes (size and SHA-256).
3. It is published with one atomic link that never replaces an existing
   archive. Only then is the log replaced, atomically, with the note. A
   failure at any step leaves the log unchanged. If the log cannot be
   replaced after the archive was published, the message says so; a rerun
   archives it again.
4. A log of one line or none is left alone ("No new log entries to roll"), as
   before.

## Retention: `msm maintenance prune`

```
msm maintenance prune [--dry-run] [--days 30] [--keep 1] [--only server-logs,backups,log-archives,world-backups]
```

Replaces the four `find | xargs rm` pipelines. Always start with
`--dry-run`, which lists every file that would be removed and removes
nothing.

* **Age** is the legacy `-mtime +30`: a file is removed once its
  modification time is more than 30 whole days old (31 days). A time in the
  future is never old.
* **What** is removed: regular files directly inside the directories in the
  table above, named `*.log.gz` (logs) or `YYYY-MM-DD-HH-MM-SS.zip`
  (backups, the names `msm` publishes). A ZIP with another name, a link, a
  directory or a file deeper down is never removed.
* **The newest recoverable backup is never removed.** In each backup
  directory (a server's complete backups; each world's backups) the `--keep`
  newest archives (default 1) that verify as finished ZIPs are kept, however
  old: a server that was not backed up for 60 days keeps its last backup. An
  archive that does not verify (truncated, damaged afterwards) is not
  recoverable, is reported, and does not count; if *no* archive in a directory
  verifies, nothing in it is removed. The same holds for the archives of a
  server that was deleted: its last backup stays until you remove it.
* **Roots are checked** before anything is removed. A root that is not an
  absolute clean path, is `/` or one directory below it, is a symbolic link,
  is not a directory, or overlaps another root (the servers, backups, logs
  and world archives must be four separate directories, none inside another)
  is refused with nothing removed. A root that does not exist has nothing to
  prune.
* **Containment**: files are listed and removed through a directory handle
  opened on the root (`os.Root`), which cannot be led outside it by `..` or by
  a symbolic link swapped in while the job runs. A linked server directory in
  an archive root is not followed.
* **Locking**: each server is pruned under its own lock, the one a backup,
  log roll or start holds, so no archive is published into a directory that is
  being read. When started as root it acts as the server's owner, as the
  other commands do.
* A file that cannot be removed is reported; the others are still pruned and
  the command exits nonzero.

## Locks, environment, owner and timeouts

A scheduled job is not special-cased: `msm maintenance run <job>` runs the
same code as the command in the table, so it takes the same per-server
locks, drops to the same owner and makes the same checks. Specifically:

* **Locks.** Each job takes the lock of every server it touches, one at a
  time, exactly like the interactive command. A backup and a log roll of the
  same server wait for each other, in either order.
* **Overlap.** `maintenance run` also takes a lock for the job itself
  (`.msm-job-<job>.lock` in `SERVER_STORAGE_PATH`). If the previous run of
  the same job is still going, the new run is *skipped*, prints
  `skipped, the previous run is still going`, and exits 0. So a RAM sync
  scheduled every 30 minutes does not pile up behind a two-hour backup.
  Different jobs are not excluded by it: they queue on the server locks they
  need, and each is stopped by its timeout. systemd and launchd also refuse to
  start a job that is still running.
* **Owner.** Run the job as the manager user (`USERNAME` in `msm.conf`) or
  as root. Root acts as each server's owner as the other commands do. Any
  other user is refused with the same error as at the command line. The
  examples name `User=minecraft` (systemd), the user column (cron) and
  `UserName` (launchd).
* **Environment.** The units set `MSM_CONF`, `PATH`, `LANG` and a umask
  explicitly; nothing comes from a login shell. `msm` prints one line to
  standard error when a job starts, with the effective user ID and the
  timeout, so a log shows what ran.
* **Timeouts.** `maintenance run` stops a job at `--job-timeout`: backup 4h,
  prune 1h, recover 30m, todisk 20m, logroll 15m. A job that does not stop
  within 30 seconds of that (for example waiting for a server's lock) is
  abandoned with an error so the process exits and releases everything. The
  systemd example adds `TimeoutStartSec=6h` as a backstop above all of them.
  The per-operation `--timeout` (start/stop/console, default 5m/5s) and
  `--backup-timeout` (default 2h) still apply inside the job.

External programs: scheduling needs the host's own scheduler (systemd, cron
or launchd), and servers need GNU `screen` as for any `msm` command. Nothing
else (`find`, `xargs`, `rm`, `gzip`, `flock`) is used.

## Opting in

Pick one scheduler. Edit the user, the path of the `msm` binary
(`/usr/local/bin/msm` below) and `MSM_CONF` first. Disable any old
`/etc/cron.d/msm` from the Bash installation, or jobs run twice
(`msm migrate apply` does this, and writes the cron file below from the legacy
one; see [migration.md](migration.md)).

### systemd (Linux)

```sh
sudo cp examples/scheduling/systemd/msm-maintenance@.service examples/scheduling/systemd/msm-*.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl start msm-maintenance@prune.service     # try one job first; use --dry-run in ExecStart while testing
sudo systemctl enable --now msm-logroll.timer msm-backup.timer msm-prune.timer msm-todisk.timer msm-recover.timer
systemctl list-timers 'msm-*'
journalctl -u 'msm-maintenance@*'
```

Remove with `systemctl disable --now msm-*.timer` and delete the files.

### cron (Linux)

```sh
sudo cp examples/scheduling/cron/msm-maintenance /etc/cron.d/msm-maintenance
sudo chmod 644 /etc/cron.d/msm-maintenance
```

`/etc/cron.d` files must be owned by root and not writable by others.
Remove the file to opt out.

### launchd (macOS)

```sh
for job in logroll backup prune todisk recover; do
  sudo cp examples/scheduling/launchd/com.example.msm.$job.plist /Library/LaunchDaemons/
  sudo chown root:wheel /Library/LaunchDaemons/com.example.msm.$job.plist
  sudo launchctl bootstrap system /Library/LaunchDaemons/com.example.msm.$job.plist
done
```

Rename the label (`com.example.msm.*`) to one of your own first. Output goes
to `/var/log/msm-maintenance-<job>.log`. Remove with
`sudo launchctl bootout system/<label>` and delete the file.

## Recovering from trouble

* **A job reports `skipped` every time.** A run that holds the job lock is
  still going, or a process was killed while holding it; the lock is released
  when its process exits, so look for a stuck `msm maintenance run`.
* **`prune` warns an archive "is not recoverable".** The archive was damaged
  after it was published. Keep the older archive it names as the recovery
  point and make a new backup; remove the damaged file yourself when you are
  sure.
* **`logroll` says "Skipped: the server is running".** Stop the server, roll,
  start it again; see the table above.
* **A `.msm-logroll-*.partial` file in a log archive directory** is from a
  roll that was killed; it is not an archive and can be removed.
