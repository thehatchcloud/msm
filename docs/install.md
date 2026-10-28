# Installing and updating the Go port

This is the manual installation of the single `msm` executable. There is no
installer script: nothing here downloads or runs code, installs a package,
creates a user, or enables or starts a service unless you run the command that
says so. The inherited `installers/*.sh` and `init/` files belong to the
original Bash manager (DEV-037); do not use them for this executable.

Supported: Linux and macOS, on amd64 and arm64.

## 1. Get the executable

Build it (`make build`, or `CGO_ENABLED=0 go build -o bin/msm ./cmd/msm` with
the Go version in `go.mod`), or take the archive for your platform from a
release (see [release.md](release.md)), and check it before using it:

```sh
# a release directory: verifies the signature, every archive and its contents
# (build msm-release from this repository: go run ./cmd/msm-release ...)
go run ./cmd/msm-release verify -pubkey "$PUBLIC_KEY" -dir ./release
```

Each archive `msm-X.Y.Z-<os>-<arch>.tar.gz` holds exactly `msm`,
`LICENSE.markdown`, `SHA256SUMS` (the executable's checksum) and `RELEASE`
(version, commit, OS, architecture). Copy `msm` to a directory that you or root
own and no package manager does, for example `/usr/local/bin` (or
`~/.local/bin` without root), mode 0755:

```sh
sudo install -m 0755 msm /usr/local/bin/msm
```

Keep it out of `/usr/bin`, a Homebrew `Cellar` and similar: `msm update`
refuses to replace files a package manager owns.

## 2. Prerequisites

```sh
msm install check
```

reports GNU screen and Java, and for each missing one the command that
installs it on common systems; it never runs it. Exit status 6 means
something is missing. On macOS the screen in `/usr/bin` (4.00.03) is too old
and is refused; install a current one (`brew install screen`).

## 3. Configuration and data directories

Without root, `msm` needs no configuration file: it keeps servers and JARs in
per-user directories (`~/.local/share/msm` on Linux, `~/Library/Application
Support/msm` on macOS). To use a system-wide installation:

```sh
sudo useradd --system --create-home --shell /usr/sbin/nologin minecraft   # Linux; macOS: create the user in System Settings
sudo msm install config                    # /etc/msm.conf, /opt/msm/{servers,jars,archives}
```

`install config` writes a minimal `msm.conf` (the other settings keep their
defaults; `msm config` lists them), creates the directories it names owned by
that user, and refuses to replace a different existing file without `--force`.
Options: `--conf`, `--user`, `--servers`, `--jars`, `--archives`. Without root
you must pass `--conf PATH` and set `MSM_CONF` to it.

Then create a server and select a JAR:

```sh
msm server create survival
msm jargroup list                  # JAR groups; see docs/cli.md
```

## 4. Start at boot (optional)

```sh
msm install service                  # print the definition
sudo msm install service --write     # install it, not enabled
sudo msm install service --write --enable
```

Linux gets `/etc/systemd/system/msm.service`; macOS gets
`/Library/LaunchDaemons/com.thehatchcloud.msm.plist`. `--conf` and `--user`
choose the `msm.conf` and the account the service runs as (default root; `msm`
then drops to each server's owner, as before). `--enable` enables it for the
next boot and does not start it now; start it with `sudo systemctl start msm`
or `msm start`. On macOS, enabling marks the daemon enabled but does not load
it; it loads at the next boot, or load it yourself with `sudo launchctl
bootstrap system /Library/LaunchDaemons/com.thehatchcloud.msm.plist` (which
starts it).

### What starts and stops

At boot the service runs `msm start`: it starts the servers marked **active**.
At shutdown (or `systemctl stop msm`) it runs `msm stop`, which warns players,
saves and stops every running server and **leaves them marked active**, so the
same servers return at the next boot. Marking a server inactive is a separate
act: `msm <server> stop`. No per-server unit is generated, because running
`msm <server> stop` at shutdown would mark that server inactive and it would
not come back.

launchd only sends SIGTERM and has no stop command, so the macOS daemon runs
`msm install supervise`, which starts the active servers, stays in the
foreground, and on SIGTERM stops them exactly as `msm stop` does.
`ExitTimeOut` (900 s) is how long launchd waits before killing it; systemd's
`TimeoutStopSec` is the same.

The scheduled jobs (backups, log rolling, `todisk`) are not installed; see
[`examples/scheduling`](../examples/scheduling) and
[maintenance.md](maintenance.md).

## 5. Shell completion

```sh
msm completion bash | sudo tee /etc/bash_completion.d/msm >/dev/null
msm completion zsh  | sudo tee /usr/local/share/zsh/site-functions/_msm >/dev/null
```

(The generated scripts are also in [`completions/`](../completions/).)

## Updating

```sh
msm update --check      # is a newer signed release available?
sudo msm update         # asks [y/N]; --noinput answers yes
sudo msm update --rollback
```

See [Updating in the command reference](cli.md#updating) for exactly what is
verified. Only releases built with `make release` carry the signing key and
release location, so a development build says it cannot update. Updates
replace the executable only: configuration, servers, worlds and backups are
untouched. A running server is not interrupted; the new program is used the
next time you run `msm`.

## macOS: what is and is not promised

* The executables are **not** signed with an Apple Developer ID and **not**
  notarized. The Go linker adds the ad-hoc signature that Apple silicon
  requires to run a binary at all, which says nothing about who built it.
* The authority for a release is the ed25519 signature checked by `msm update`
  and `msm-release verify`, not Gatekeeper.
* A file downloaded with a browser carries the `com.apple.quarantine`
  attribute, and Gatekeeper will refuse to open it. Verify it as above, then
  clear the attribute yourself: `xattr -d com.apple.quarantine msm`. Files
  fetched by `curl` or by `msm update` are not quarantined.
* `msm update` replaces the file with a new one, so the quarantine state of
  the old file does not carry over.

## What msm runs

External programs, all started without a shell and with fixed arguments:
GNU `screen` and Java (servers), `rdiff-backup` (optional backups),
`systemctl` / `launchctl` (only when you pass `--enable`), and the newly
downloaded `msm version` (only after the signature and checksums verified).
