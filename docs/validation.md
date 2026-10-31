# Native validation (P16)

Cross-compiling shows that the code builds for a platform. It does not show that
the executable runs there. This page says what is run natively, on what, what
that proves, and what is still open. The release check
(`msm-release evidence`) refuses a release for any platform without passing
evidence, so a gap here is a visible blocker, not a silent skip.

## What runs

[`internal/e2e`](../internal/e2e) starts the **built `msm` executable** against a
**real GNU screen** and a **real JVM**:

* the "server" is [`testdata/FakeServer.java`](../internal/e2e/testdata/FakeServer.java),
  compiled with the machine's JDK and run with `java -jar` in a real screen
  session, as `msm` runs any server;
* it logs `logs/latest.log` in the Minecraft 1.13+ format, prints the `Done`
  line when ready, answers `save-all`, `save-off`, `save-on`, `say`, `list` and
  `whitelist on` with the vanilla wording the `minecraft/1.13.0` profile matches,
  keeps its world in `world/` (a link into `worldstorage/`, as the legacy layout
  has it) and exits on `stop`;
* it is **not** a Minecraft server: no network protocol, no chunks, no plugins.
  What it validates is the manager, screen, the operating system and the JVM
  working together, not Mojang's or Paper's code.

| Scenario | What is asserted |
|---|---|
| `start` | `start` brings a JVM up behind screen and waits for `Done`; `status` says running; the server is marked active; a second `start` says it is already running |
| `commands` | `say`, `cmd`, `save all` (waits for the confirmation line), `whitelist on` and `connected` reach the JVM through screen and are answered |
| `attach-detach` | `console` under a real pseudo-terminal: a typed line reaches the server, Ctrl-A d detaches, the server keeps running and still answers |
| `graceful-stop` | the player warning, then `save-all`, then `stop`, in that order in the server's log; the server ends not running **and not active**; stopping a stopped server says so |
| `backup-and-restore` | `backup` and `worlds backup` produce archives; after more play, an `unzip` of the archive (there is no restore command, DEV-011) holds the backed-up world, not the later one |
| `global-intent` | global `stop` stops everything but keeps every server active; global `start` brings them back; an explicit `<server> stop` removes only that server's intent |
| `crash-recovery` | the JVM is killed with `SIGKILL`; the server stays marked active; `maintenance run recover` (the hourly job) restarts it as a new process and does not start a server stopped on purpose |
| `ram-sync` | a world flagged for RAM is served from the RAM volume; the disk copy is behind until `all worlds todisk` writes it; `stop` writes it too; clearing the flag removes the RAM copy |
| `other-user-is-refused` | run as an ordinary user against an installation owned by another account, `start` exits 5 and changes nothing (no active marker), and `server delete` fails |
| `root-acts-as-the-owner` | **Linux only.** As root with an installation owned by `nobody`, the server directory and the JVM belong to `nobody` |

## Declared versions

| | Validated | Not validated |
|---|---|---|
| Operating systems | Linux (Ubuntu 24.04 runners), macOS 15 | any other release or distribution |
| Architectures | amd64 and arm64 on each (see below for runner availability) | |
| Java | 17 and 21 (Eclipse Temurin, as installed on the runner images; the suite fails if a runner has no such JDK). The stand-in is compiled with `--release 17` | 8, 11, 25 |
| GNU screen | the version the runner's package manager installs (Ubuntu's 4.9.1; Homebrew's current), recorded in each evidence file. The macOS-bundled 4.00.03 is refused by `msm` and that refusal is tested in `go.yml` | other versions |
| Minecraft server | the console protocol of 1.13 and later, through the stand-in | **real Mojang, Paper or Spigot jars; the legacy profiles (1.2 to 1.12) against a live server**. Profiles are checked against recorded log fixtures, not live servers |
| macOS root privilege drop | | **not validated natively**: no account exists on the hosted runner that the suite can rely on. The unit tests cover the code path; run the root scenario on a macOS host you control if you depend on it |

Validating against a real server jar needs a pinned, checksummed download and a
EULA acceptance, and the scenarios that write a world need the stand-in's
`world-write` hook. That is open work for the release review (P17), and until it
is done a release says "validated with a console stand-in", not "validated with
Minecraft". **Stage your real server first** ([migration.md](migration.md)).

## Runners

[`validate.yml`](../.github/workflows/validate.yml) runs every scenario on
each target and each Java version, then uploads an evidence file
(`e2e-<os>-<arch>-java<N>.json`, plus `-root` for the Linux root checks) with:
the platform, Go, Java and screen versions, the commit, `msm version`, the
**SHA-256 of the executable under test**, and every scenario's result.

* Pull requests and pushes run `linux/amd64` and `darwin/arm64`, which every
  GitHub-hosted account has.
* `linux/arm64` (`ubuntu-24.04-arm`) and `darwin/amd64` (`macos-15-intel`) run
  from **Run workflow** with *extended* checked. Whether GitHub provides those
  runners to the repository depends on the account (arm64 Linux hosted runners
  are not offered to every private repository, and Intel macOS runners are being
  retired). **If a runner is unavailable the target has no evidence and the
  release is blocked** for it, or the target is declared unsupported in
  [release.md](release.md) and dropped from `internal/evidence.Targets`.
  Nothing quietly passes in its place.
* A separate job rehearses a migration and rollback on a copy on both OSes
  ([migration.md](migration.md#rehearse-it-first)).

## The release check

```sh
# download the e2e-evidence-* artifacts of an extended run into ./evidence
go run ./cmd/msm-release evidence -dir ./evidence -commit <40-hex release commit> \
    [-release ./dist/release -pubkey "$PUBLIC_KEY"]
```

It fails, listing every gap (`BLOCKED linux/arm64, Java 17, as a user: no
evidence …`), unless each of the four platforms has, for the release commit and
each declared Java version, every scenario above passing (and, on Linux, the
root scenario). With `-release` it also requires that the evidence was made with
**the exact executable that is being released**: the checksum recorded in each
file must equal the checksum of the `msm` inside the verified release archive of
that platform. To produce such evidence, extract the executable from the release
archive on a native machine and run the suite with `MSM_E2E_BIN` pointing to it.

## Running it yourself

Needs GNU screen, a JDK (17 or newer) and, for RAM worlds, a RAM volume:

```sh
export MSM_E2E=1
export MSM_E2E_RAMDIR=/dev/shm/msm-e2e          # Linux; macOS: see ram-worlds.md
export MSM_E2E_BIN=/path/to/msm                 # optional: test this executable
go test -count=1 -v ./internal/e2e
MSM_E2E_USER=nobody sudo -E env "PATH=$PATH" go test -count=1 -run UserPermissions ./internal/e2e   # root
```

`make native` is the same as the first command (it sets `MSM_E2E=1`). Without it the package skips (so `go test ./...` stays fast and needs
no screen). With it, a missing screen, JDK or RAM volume is a **failure**, never
a skip. Each scenario runs in a private directory with its own `SCREENDIR`; the
tests start no process outside it, and never touch a real installation.
`MSM_E2E_EVIDENCE=dir` writes the evidence file.

## What validation is not

CI does not migrate a production server and does not modify real worlds: every
scenario uses temporary directories, and the migration is rehearsed on a copy
with `--root`. None of this authorizes a deployment.
