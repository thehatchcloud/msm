# Support matrix

What `msm` runs on, what it needs, and what has actually been validated. This
is the page to read before you depend on a combination; anything not marked
validated is "expected to work, not tested".

## Platforms

| OS | amd64 | arm64 | Notes |
|---|---|---|---|
| Linux (static, CGO-free executable) | native validation on Ubuntu 24.04 | native validation on Ubuntu 24.04 ARM (extended run) | systemd units; root drops to the data owner; RAM worlds on tmpfs. Other distributions are expected to work, not tested |
| macOS | native validation on macOS 15 (extended run) | native validation on macOS 15 | launchd daemon; RAM worlds on an `hdiutil` RAM volume; **root privilege drop not validated natively**; Homebrew screen needed |

Windows and the BSDs are not supported. 32-bit systems are not supported. Pull requests run linux/amd64 and macOS/arm64; linux/arm64 and macOS/amd64
run only from the manually started extended run, which a release requires
([validation.md](validation.md)). "Native validation" above means that suite,
not a claim about hardware this page has not seen evidence for.

## Required and optional programs

| Program | Status | Notes |
|---|---|---|
| GNU screen | **required** to run servers | validated with Ubuntu's 4.9.1 and current Homebrew; the macOS-bundled 4.00.03 is refused. `msm install check` reports it |
| Java | **required** to run servers | 17 and 21 validated; use the version your Minecraft server needs |
| `rdiff-backup`, `nice` | optional | only with `RDIFF_BACKUP_ENABLED=true` |
| `systemctl` / `launchctl` | optional | only to enable the boot service; you can enable it yourself |
| `unzip`, `cp`, `rsync` | your tools | not used by `msm`; used by you when [restoring](operations.md#restoring) |

No `bash`, `wget`, `jq`, `zip`, `find`, `sed` or `ps` is needed by `msm`
([audit.md](audit.md)).

## Minecraft

| | Status |
|---|---|
| Server console protocol of 1.13 and later (`minecraft/1.13.0` profile) | validated end to end with a console stand-in; **not** with a real Mojang, Paper or Spigot jar |
| Legacy profiles 1.2 to 1.12 | checked against recorded log fixtures, not live servers |
| Plugins during a backup | `save-off`/`save-all` cover vanilla worlds; plugin data outside the world is not made consistent |

## Feature parity with the Bash manager

All 178 rows of the [parity matrix](compatibility/parity.md) are implemented
or covered by a reviewed deviation; none is deferred. 152 are preserved and
tested; 26 differ on purpose ([deviations](compatibility/deviations.md)), for
safety (no shell evaluation, verified TLS, no unbounded waits, no data loss)
or because the upstream behavior is obsolete. Features the Bash manager
advertised but never had (QuickBackup, a restore command) are not in this
release ([release-notes.md](release-notes.md#future-work)).
