# Release notes: the Go port

These notes accompany the first release of the Go port of Minecraft Server
Manager (the version number is chosen when the release is made; see
[release.md](release.md)). They are written for administrators of an existing
Bash installation and for new users. Upstream: `msmhq/msm` at commit
`7a9d120d6b93f2f62178a60416955eea2065c5ce` (0.11.0); this release is GPLv3
and keeps the upstream license and attribution.

## What this is

`msm` is one static executable. Every command of the Bash manager is
implemented in Go (178 inventory rows: 152 preserved, 26 deliberate
deviations, 0 deferred). The Bash manager, its sourced configuration, its
installers and its shunit suite are gone from the source tree (DEV-039). The
release is verified by checksum and ed25519 signature; `msm update` accepts
only such releases.

Start with [install.md](install.md); if you have a Bash installation,
[migration.md](migration.md); then [operations.md](operations.md).

## Required dependencies

* GNU screen (not macOS's bundled 4.00.03) and Java for the Minecraft version
  you run.
* Linux or macOS on amd64 or arm64 ([support-matrix.md](support-matrix.md)).

## Optional helpers

* `rdiff-backup` (and `nice`) for `RDIFF_BACKUP_ENABLED`.
* `systemd` / `launchd` for starting at boot and for the schedules in
  `examples/scheduling` (not installed by `msm`).

## OS-specific limits

* macOS: Homebrew screen; RAM worlds need an `hdiutil` volume you create;
  privilege drop from root is not validated natively; there is no legacy Bash
  installation to migrate (`migrate` is Linux only).
* Linux: RAM worlds need a tmpfs you provide; `migrate` uses `systemctl` for the
  service step when it is present.

## Known deviations

All 26 are in [compatibility/deviations.md](compatibility/deviations.md). The
ones you will notice:

* Unknown commands and failed operations exit non-zero (named exit codes).
* No shell evaluation of `msm.conf` or `INVOCATION` (DEV-001); JAR groups need
  https; downloads verify TLS (DEV-002, DEV-020).
* `server create` makes no JAR link and never downloads (DEV-017); `server
  delete`/`rename` need a stopped server (DEV-018); delete needs a
  confirmation (DEV-003).
* RAM worlds are off for new installations (DEV-008).
* `msm update` updates only the executable, from signed releases (DEV-006).
* `msm install` is a command and installs no schedule or packages on its own
  (DEV-037); the schedule is an opt-in example (DEV-012).
* `bash_completion` is generated: `msm completion bash|zsh` (DEV-033).

## Validation status

Unit, race, security (`govulncheck`) and the audit pass on the release commit
in CI. Native scenarios (real screen, real JVM) pass on the platforms listed
in the evidence the maintainer attaches to the release. **Validated with a
console stand-in, not a real Minecraft jar.** Stage your real server first.

## Future work

Not in this release: a restore command, QuickBackup (local-then-move backups),
validation with real Mojang/Paper/Spigot jars, native macOS root-drop tests,
packaging for distribution package managers, and an installer for the
schedule. None of these is claimed by this release.

## Deployment

Publishing a release does not deploy it. Installing it on any host, and
migrating a production installation, are separate, supervised decisions.
