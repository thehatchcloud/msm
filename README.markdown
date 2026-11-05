# Minecraft Server Manager

A single init script which makes running multiple Minecraft/Bukkit servers easier for us admins.

## The Go port

This fork is a behavior-compatible Go port of MSM: one static executable,
`msm`, with the original server-first commands (`msm <server> start`,
`msm survival backup`, ...). The original Bash implementation, its sourced
configuration, installers and shunit suite are no longer in the source tree
(they remain in git tag `legacy-bash-final`, pinned to upstream
[`7a9d120`](https://github.com/msmhq/msm/commit/7a9d120d6b93f2f62178a60416955eea2065c5ce));
`msm` runs no shell and no Bash manager, only GNU screen, Java and the helpers
you choose ([audit](docs/audit.md)).

* [Release notes](docs/release-notes.md) and the [support matrix](docs/support-matrix.md):
  what is required, what is optional, what is validated, what differs.
* [Install and update](docs/install.md); [migrate from the Bash manager](docs/migration.md) and roll back.
* [Operate, back up, restore and recover](docs/operations.md); the [command reference](docs/cli.md);
  [scheduled maintenance](docs/maintenance.md); [RAM worlds](docs/ram-worlds.md).
* The [compatibility contract](docs/compatibility/README.md), the
  [parity matrix](docs/compatibility/parity.md) and the
  [approved deviations](docs/compatibility/deviations.md).
* [Development and CI](docs/development.md), [releases](docs/release.md),
  [dependency and license review](docs/dependencies.md).

Build it with the pinned Go toolchain: `make build` (or
`CGO_ENABLED=0 go build -o bin/msm ./cmd/msm`). Publishing a release is not
deploying it; try it on a test installation first.

## Getting Started

* [Install](docs/install.md) MSM on your box.
* Read the [operations guide](docs/operations.md) to get started.
* Fork and contribute your own modifications.
* Not a coder? [Post an issue][issues] with your idea for MSM instead.
* Read the [changelog][changelog] to get a picture of how MSM has evolved over time.

MSM is released under the GPLv3 licence, which is included in the repository [here](LICENSE.markdown), with the upstream copyright and attribution. I'm open to suggestions where licencing is concerned.

## Features

As well as starting, stopping and restarting MSM has the following features:

* One script handles multiple servers, run two or more servers on one machine.
* Can create and start new servers with a single command, downloads the jars for you.
* Periodically makes [WorldEdit snapshot][we-snapshot] compatible backups of your worlds.
* Backup the entire server directory for complete protection.
* Load world's into RAM for faster access (reduces lag).
* Easily configurable global defaults, with per server overrides if needed.
* Apply server commands to one, multiple, or all servers in one go (useful for whitelisting a player on all servers.)
* Tab completion for all commands (`msm completion bash|zsh`), makes everything faster and getting started a breeze.
* Keep server logs organsied by periodically "rolling" them.
* Organises jar files into groups (such as minecraft and craftbukkit) and links each server to a single jar. Includes automated download of new versions.
* Plethora of in-game commands (whitelist, blacklist, operator, gamemode, kick, say, time, toggledownfall, save)
* Send commands straight to the server via the command line.

## Support

1. If you find a problem with MSM and you think the problem is one that requires changing code [submit an issue][issues] via GitHub.

## Upcoming features

* **QuickBackup:** If you store your backups non-locally (maybe on a NAS), QuickBackup optionally creates a backup locally for speed, and then moves it after your players are building again! My initial testing shows a 54 second network backup confaltes to 23 seconds of in-game time.
* **Restore:** Roll-back to an old world or whole server backup automatically (today restoring is a documented manual copy, see [operations](docs/operations.md#restoring)).


## Versioning

MSM uses semantic version numbers to better describe what code one might have installed, and indicate backwards incompatible changes.

Releases will be numbered in the following format:

`<major>.<minor>.<patch>`

And constructed with the following guidelines:

* Breaking backward compatibility bumps the major (and resets the minor and patch)
* New additions without breaking backward compatibility bumps the minor (and resets the patch)
* Bug fixes and misc changes bumps the patch

For more information on SemVer, visit http://semver.org/.


## Acknowledgements

This code grew out of an old version of [Ahtenus' Minecraft Init Script][ahtenus-minecraft-init].

[we-snapshot]: http://wiki.sk89q.com/wiki/WorldEdit/Snapshots
[ahtenus-minecraft-init]: https://github.com/Ahtenus/minecraft-init
[changelog]: CHANGELOG.markdown
[issues]: https://github.com/thehatchcloud/msm/issues
