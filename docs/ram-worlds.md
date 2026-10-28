# RAM-backed worlds (P12)

A world can be served from memory while its server runs, which makes chunk
reads and writes much faster, and written back to disk. The directory on
disk always stays the durable copy. This page says what the manager does,
what it needs from you, what a power loss costs, and how to recover.

## What you must provide

The manager **never creates, mounts or sizes a RAM disk**; that needs
privileges it does not take. You prepare one, then enable RAM worlds:

* **Linux:** a `tmpfs`. `/dev/shm` is one on every systemd distribution.
  For a size limit of your own, mount one, for example
  `mount -t tmpfs -o size=4g tmpfs /mnt/msm-ram` (or an `/etc/fstab` line),
  and make its top directory writable by the user that runs the manager.
  `ramfs` is refused: it has no size limit, so a
  full one exhausts the machine's memory instead of failing.
* **macOS:** a RAM disk made from memory, for example
  `diskutil erasevolume HFS+ MSMRAM $(hdiutil attach -nomount ram://4194304)`
  (4194304 sectors of 512 bytes is 2 GiB). The check recognizes volumes whose
  image path is `ram://` in the output of `hdiutil info`. It runs `hdiutil`,
  an external program that ships with macOS; no other external program is
  used for RAM worlds.

Then, in `msm.conf`:

```sh
RAMDISK_STORAGE_ENABLED="true"
RAMDISK_STORAGE_PATH="/dev/shm/msm"   # inside the RAM volume
```

Each world flagged for RAM lives in `RAMDISK_STORAGE_PATH/<server>/<world>`
while a server runs. The nearest existing part of that path must be on the
RAM volume; directories below it are made as needed. If the path is on an
ordinary disk, or the volume is not mounted, every RAM command fails with an
unsupported/configuration error that says so. Nothing is created, and no disk
directory is ever treated as RAM.

### Defaults and imported settings

A new installation ships `RAMDISK_STORAGE_ENABLED="false"`, and a missing
setting means off (the legacy manager defaulted to on; see DEV-008). An
imported `msm.conf` that says `true` and worlds already flagged with the
`inram` file keep that intent: the flag stays, `worlds list` still shows
`RAM`, and nothing is lost. A flagged world is only served from RAM when the
path is validated as above and the volume has room for the world; until then
the server is started from the world's copy on disk and a warning says why.

## Commands

```text
msm <server> worlds ram <world>   switch a world's RAM flag (server stopped)
msm <server> worlds todisk        write the RAM worlds of a server to disk
msm all worlds todisk             the same for every server (cron runs this)
```

* **Switching on** checks the volume and the free space, copies the world to
  RAM (staged, then renamed into place) and only then sets the flag. If any
  step fails the world is unchanged and unflagged.
* **Switching off** first writes the RAM copy to disk, and only after that
  is durable removes the flag and the RAM copy. If the write fails the world
  stays flagged with its RAM copy.
* **Start** (and `worlds load`) makes or checks the RAM copy of each flagged
  world and links the server to it. A flagged world whose copy cannot be used
  is linked to its disk copy and a warning is printed; the server still starts.
* **Shutdown** (`stop`, `restart`, global `stop`) writes the RAM worlds to
  disk after the server has stopped. If that fails the server is stopped, the
  command fails and says the worlds were not made durable; the RAM copies are
  untouched.
* **Backup** writes the RAM worlds to disk after `save-all` (and, for a
  stopped server, before copying), so the backup holds the newest data. If the
  write fails the backup fails.
* **Periodic sync** is `msm all worlds todisk`; `cron/msm` runs it every 30
  minutes (see [maintenance.md](maintenance.md) for the systemd, cron and
  launchd examples). For a running server it turns saving off, saves, writes, and turns
  saving on again, also on an error or Ctrl+C.

All of them hold the server's lock, so shutdown, a backup and a cron sync
that coincide run one after another. `worlds off` refuses a world that has a
RAM copy.

## How the disk copy is protected

* The disk copy is only replaced by a complete, flushed, verified copy,
  built beside the world and swapped in with one atomic exchange (two renames
  where the filesystem cannot exchange, restoring the old copy if the second
  fails). A full disk (`ENOSPC`), an error or an interruption leaves the disk
  copy as it was and removes the partial copy. Free space is checked before
  copying.
* Each RAM copy records, in `.msm-ram.json` inside it, what both sides looked
  like when they last matched (name, type, size, permission and modification
  time of every entry; no contents are read). Each side is compared only with
  its own earlier value, at full timestamp precision; a copy is verified with
  whole-second times, because a macOS RAM disk (HFS+) keeps no fractions of a
  second. **RAM is written to disk only if
  the disk copy has not changed since**, and **the disk copy replaces the RAM
  copy only if the RAM copy has not changed since**. So stale RAM data never
  overwrites a newer world on disk (for example after you restored a backup),
  and a newer disk copy is never lost to an old RAM copy.
* If both changed, neither is touched. The server is started from the disk
  copy, `worlds todisk` and `worlds ram` (off) refuse that world with an
  error naming both paths, and the RAM copy is kept for you to inspect.
* A RAM copy without a manifest (not made by this manager) is not used,
  synchronized or replaced.
* Nothing outside the world's own RAM directory is ever removed, and files
  are copied through descriptors that follow no symbolic links, so a link
  planted in a world cannot redirect a copy that runs as root.

## What a power loss costs

**Everything that changed since the last successful write to disk is lost**
if the machine loses power, panics, or the RAM volume is unmounted, because
that data existed only in RAM. The write happens at each shutdown, backup and
periodic `worlds todisk` (every 30 minutes with the shipped cron), so the
worst case is about the cron interval of play. A crash of the manager or of
Minecraft alone does not lose RAM data (memory survives); the next start keeps
a RAM copy that is ahead of disk and the next sync writes it. Shorten the
interval in `cron/msm` if half an hour is too much, or leave a world off RAM.

## Recovery

First look at what you have: `msm <server> worlds list` shows `RAM` worlds,
`ls -la RAMDISK_STORAGE_PATH/<server>/` shows the RAM copies, and a
`.msm-world-*` directory next to a world on disk is a leftover that is safe
to delete once you have checked the world beside it is whole.

1. **After a reboot or power loss.** The RAM copies are gone. The disk copy
   from the last successful sync is complete and intact. Start the server:
   the world is copied to RAM again from disk, and you have lost only what
   changed after the last sync. If you restore an older world instead, see
   below.
2. **`todisk` or a shutdown reports "could not be written to disk".** The RAM
   copy is intact and the disk copy is unchanged. Fix the cause (usually free
   space on the disk with the world: `df -h`), then run
   `msm <server> worlds todisk` with the server stopped or running.
3. **A conflict ("changed both in ... and in ...").** The disk copy and the
   RAM copy each hold changes the other lacks. Decide which to keep with the
   server stopped:
   * keep **disk**: remove the RAM copy (`rm -r RAMDISK_STORAGE_PATH/<server>/<world>`);
     the next start copies the disk world to RAM again;
   * keep **RAM**: copy it over the disk world by hand, for example
     `rsync -a --exclude .msm-ram.json RAM/<server>/<world>/ <worldstorage>/<world>/`
     (keep the `inram` flag file), then remove the RAM copy so the next start
     rebuilds it;
   * keep **both**: move one aside under another name first.
4. **A copy without a manifest.** `RAMDISK_STORAGE_PATH/<server>/<world>`
   exists but was not made by this manager, so it is not used. Inspect it, copy
   anything you need from it, and move it aside.
5. **The server died or you killed `todisk` while saving was off.** Saving is
   turned back on on every error and on Ctrl+C, but not if the manager is
   killed outright. Send it by hand: `msm <server> cmd save-on`.
6. **Switching a world off RAM by hand.** If the manager cannot do it, stop
   the server, copy the RAM copy to the disk world as in (3), delete the
   `inram` file in the disk world, and remove the RAM copy.

## Tests

`internal/worlds` has RAM tests for: default off and the shipped
configuration, imported intent, unsupported and missing volumes, `ENOSPC`
(free-space check and failure while copying, to RAM and to disk), interrupted
copies and killed-process leftovers, stale and conflicting copies, publish
without atomic exchange, a running server's links, containment of removals,
simultaneous shutdown, backup and periodic sync, and the save-off/save-on
order around a running server. `TestRealRAMVolume` and
`TestDiskDirectoryIsNotRAM` run on a real RAM volume: tmpfs on Linux and an
`hdiutil` RAM disk on macOS, both required in CI (`MSM_REQUIRE_RAM=1`).
`internal/lifecycle` tests that a stop writes the RAM worlds to disk and
reports a failure to, and `internal/backup` that a backup synchronizes first.
