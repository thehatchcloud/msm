# Operating, backing up and recovering

A complete walk-through for an administrator who has installed `msm`
([install.md](install.md)) or migrated ([migration.md](migration.md)). Nothing
here needs knowledge of the project's internals. `msm help` lists every
command; [cli.md](cli.md) has targets, exit statuses and unattended use.

## Day to day

```sh
msm server create survival          # makes the directory; select a JAR next
msm jargroup create paper https://example.org/server.jar   # https only
msm jargroup getlatest paper
msm survival jar paper              # link the newest JAR of the group
msm survival config                 # list server.properties; set: config <key> <value>
msm survival start                  # waits for the "Done" line; marks it active
msm survival status
msm survival say "back in five"
msm survival console                # Ctrl-A d detaches, the server keeps running
msm survival stop                   # warns players, save-all, stop; marks it inactive
msm start | msm stop | msm restart  # every server; "stop" keeps the active marks
```

Before the first start you must accept the Minecraft EULA yourself in
`eula.txt` in the server directory (`server create` tells you where); `msm`
never accepts it for you. Run `msm` as the account that owns the data (the `USERNAME` setting), or
as root, which drops to that account for the server processes.

Where things are (defaults of `msm install config`; `msm config` shows yours):

| What | Setting | Default |
|---|---|---|
| Servers (one directory each, `worldstorage/` holds the worlds) | `SERVER_STORAGE_PATH` | `/opt/msm/servers` |
| JAR groups | `JAR_STORAGE_PATH` | `/opt/msm/jars` |
| World archives (`<server>/<world>/<timestamp>.zip`) | `WORLD_ARCHIVE_PATH` | `/opt/msm/archives/worlds` |
| Whole-server archives (`<server>/<timestamp>.zip`) | `BACKUP_ARCHIVE_PATH` | `/opt/msm/archives/backups` |
| Rolled logs | `LOG_ARCHIVE_PATH` | `/opt/msm/archives/logs` |
| rdiff-backup repositories (optional) | `WORLD_RDIFF_PATH` | `/opt/msm/rdiff-backup/worlds` |
| rsync-style snapshots (optional) | `WORLD_RSYNC_PATH` | `/opt/msm/rsync/worlds` |

Without root the same layout lives under `~/.local/share/msm` (Linux) or
`~/Library/Application Support/msm` (macOS).

## Backing up

| Command | Produces |
|---|---|
| `msm <server> worlds backup` | for each active world, `WORLD_ARCHIVE_PATH/<server>/<world>/<YYYY-MM-DD-HH-MM-SS>.zip` holding `<world>/...`; plus rdiff-backup or rsync snapshots if enabled |
| `msm <server> backup` | `BACKUP_ARCHIVE_PATH/<server>/<timestamp>.zip` holding `<server>/...` (the whole server directory) |
| `msm all worlds backup` | the first, for every server |

* On a running server the backup turns saving off, saves, copies a stable image
  and turns saving back on before compressing. Saving is turned back on on an
  error or Ctrl+C; if `msm` itself is killed, the next backup does it. If you
  suspect it is still off: `msm <server> cmd save-on`.
* An archive appears under its final name only when it is complete; a full
  disk or an interrupt leaves no half archive. A backup never overwrites an
  archive.
* Symbolic links are stored as links, not followed. A complete backup follows
  them only if you set `msm-complete-backup-follow-symlinks` (server
  property; default `COMPLETE_BACKUP_FOLLOW_SYMLINKS=false`) and pass
  `--allow-external-links` for targets outside the server.
* A server with a world in RAM is written to disk first ([ram-worlds.md](ram-worlds.md)).
* World archives are WorldEdit-snapshot compatible. Inactive worlds
  (`worlds off`) are not in a world backup.
* Schedule it: `examples/scheduling` has systemd timers, cron and launchd files
  for `backup`, `logroll`, `todisk`, `recover` and `prune`; `msm install`
  never installs a schedule ([maintenance.md](maintenance.md)).
* Keep a copy **off the host**. Retention (`msm maintenance prune`, 30 days by
  default) deletes old archives; a copy that only exists on the same disk is
  not a backup.

## Restoring

There is no restore command (DEV-011): restoring is a deliberate copy you
check. Always stop the server first and keep what you replace.

**A world from a ZIP archive**

```sh
msm survival stop
cd /opt/msm/servers/survival/worldstorage
mv world world.broken-$(date +%F)                    # keep it until the restore is verified
unzip /opt/msm/archives/worlds/survival/world/2026-11-05-05-02-00.zip   # recreates world/
chown -R minecraft: world                            # the USERNAME account
msm survival start
```

If the world was flagged for RAM, keep its `inram` file (it is part of the
world directory; check `msm survival worlds list`).

**A whole server**

```sh
msm survival stop
mv /opt/msm/servers/survival /opt/msm/servers/survival.broken
unzip /opt/msm/archives/backups/survival/2026-11-05-05-02-00.zip -d /opt/msm/servers
chown -R minecraft: /opt/msm/servers/survival
msm survival start
```

**rsync-style snapshot**: `WORLD_RSYNC_PATH/<server>/<world>/<timestamp>/` is a
complete copy (`latest` is a link to the newest). Copy it back with
`cp -a SNAPSHOT/. worldstorage/<world>/` (or `rsync -a --delete`).

**rdiff-backup**: use rdiff-backup itself on the repository
`WORLD_RDIFF_PATH/<server>/<world>`; for 2.x,
`rdiff-backup restore --at 3D REPO TARGET` (1.x: `rdiff-backup -r 3D REPO TARGET`)
to a new directory, then move it in as above.

Check a restore by listing worlds (`msm survival worlds list`), starting the
server, and looking at `logs/latest.log` and `msm survival status`. Practice
this on a copy before you need it.

## Recovering

| Situation | Do |
|---|---|
| A server crashed or the host rebooted | `msm start` (the hourly `recover` job does this) restarts every server still marked active; a server you stopped on purpose stays stopped |
| `status` says running but the console is dead, or a stale session | `msm <server> stop now`; `screen -ls` (as the server account) shows any session left |
| RAM world conflict, power loss with RAM worlds | [ram-worlds.md](ram-worlds.md#recovery) |
| A scheduled job skips every run, a damaged archive, an old log partial | [maintenance.md](maintenance.md#recovering-from-trouble) |
| A bad `msm update` | `sudo msm update --rollback` swaps the previous executable back |
| A bad migration | `sudo msm migrate rollback --yes` restores the Bash manager's files from the backup the migration made ([migration.md](migration.md#rollback)) |
| Lost the host | install `msm` on the new host, restore `msm.conf`, the servers and worlds from the off-host backup (above), `msm start` |

Exit statuses (`0` ok, `1` error, `2` usage, `3` config, `4` not found, `5`
refused, `6` unavailable) are in [cli.md](cli.md#exit-statuses); `5` means
nothing was changed.
