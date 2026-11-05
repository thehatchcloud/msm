# Subprocess and shell audit (P17)

The release contract is that **all manager logic is Go**. The only programs
`msm` may start are the Java/screen server pair and the system adapters an
administrator selects. This page is the audit; `go test ./compatibility`
(`audit_test.go`) fails when the tree or the code no longer matches it.

## No Bash manager remains

The inherited Bash implementation (`init/msm`, the sourced `msm.conf`,
`versioning/*.sh` profiles, `cron/msm`, `bash_completion/msm`, `installers/*.sh`,
the service files and the shunit suite `test.sh`, with its `test.yml`
workflow) is not in the release source tree (DEV-039).

* History and attribution are kept: the last commit that holds those files is
  git tag `legacy-bash-final`, which pins upstream `msmhq/msm`
  `7a9d120d6b93f2f62178a60416955eea2065c5ce`. The upstream copyright and the
  GPLv3 text in `LICENSE.markdown` ship with every release archive.
* Inert data the Go tests compare against is kept in `compatibility/legacy/`:
  `msm.conf` and `cron-msm.txt` (read as text), and the version profiles
  renamed `*.profile` so no runtime `.sh` profile exists. Go reads none of
  them at run time.
* The test fails if any removed path returns, if a `.sh` file or a `#!` file
  appears other than the two harnesses below, or if a workflow mentions the
  old suite.

Test harnesses (run by CI on disposable machines; never installed, never run
by `msm`, not in a release archive):

| Script | Purpose |
|---|---|
| `compatibility/migration/rehearse.sh` | rehearses a migration and its rollback on a temporary copy ([migration.md](migration.md)) |
| `compatibility/worldedit/build.sh` | builds the WorldEdit snapshot-lookup harness for the backup acceptance test |

## Where processes start

No Go file calls a shell, `sh -c`, `eval`, `source`, `su` or `sudo`; the test
rejects those words as strings in non-test code. Every process is started with
an explicit argument vector. Files that start or look up a process:

| File | What it does |
|---|---|
| `internal/process/process.go` | the one runner (`exec.CommandContext`, optional uid/gid drop) used for `screen`, `java` (inside the screen session, from `INVOCATION`/`msm-invocation`), `rdiff-backup` and `nice` |
| `internal/update/update.go` | runs the freshly staged, signature-verified executable with `version` before replacing the old one |
| `internal/install/install.go` | `systemctl` / `launchctl` after `msm install service --write --enable` |
| `internal/worlds/worlds.go` | `hdiutil info` on macOS, to prove a RAM volume |
| `internal/cli/migrate.go` | `systemctl` when migrating a host that uses systemd |
| `internal/cli/server.go`, `internal/cli/install.go`, `internal/cli/help.go`, `internal/backup/backup.go` | `exec.LookPath` only: find a program, never run it |

## External programs

| Program | Needed for | Selected by |
|---|---|---|
| `screen` | running servers, console | always (required) |
| `java` | the Minecraft server itself | always (required) |
| `rdiff-backup`, `nice` | rdiff-backup history | `RDIFF_BACKUP_ENABLED=true` |
| `systemctl` | service enable/disable on Linux | `install service --write --enable`, `migrate` |
| `launchctl` | service enable on macOS | `install service --write --enable` |
| `hdiutil` | verifying a macOS RAM volume | `RAMDISK_STORAGE_ENABLED=true` |

`rsync`-style snapshots, ZIP archives, log rolling, retention, downloads, JAR
handling, links and completion are Go code; no `rsync`, `zip`, `wget`, `jq`,
`find`, `sed` or `ps` is needed. The installed executable also runs itself
(`msm maintenance run ...`) from the schedule you opt into
([maintenance.md](maintenance.md)).

## Changing this list

Adding a subprocess means updating `execFiles` or `programs` in
`compatibility/audit_test.go` **and** this page, in a reviewed pull request.
