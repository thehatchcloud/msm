# Parity matrix

[`compatibility/parity.tsv`](../../compatibility/parity.tsv) has one row for
every command in `commands.tsv`, every setting in `settings.tsv` and every
surface in `surfaces.tsv`: 178 rows. Each row says how the Go port accounts
for it, and `go test ./compatibility` fails if any row is missing, names a
file or test that does not exist, or claims a status the rules below do not
allow. A row is never silently "done": a behavior is either preserved and
tested, deliberately different under a reviewed deviation, or explicitly
deferred.

| Status | Rows | Meaning |
|---|---|---|
| implemented | 151 | preserved; `implementation` lists the Go files and `verification` the tests (`path::TestName`) that prove it |
| deviation | 24 | deliberately different or absent; `deviations` names the entries in [deviations.md](deviations.md) that a reviewer approved, and `verification` still tests what the port does instead |
| deferred | 3 | owned by a later task (P15 packaging and services, P16 migration); the row says what is missing |

The command rows also name their section of the golden file
[`internal/cli/testdata/commands.golden`](../../internal/cli/testdata/commands.golden)
(`#CMD-nnn`): the exit status, standard output and standard error of the
command's signature, each alias, `all`, and a missing and an extra argument,
on a fixed installation. Run `make golden` after a reviewed change to
regenerate it.

How to read a row: the settings' `verification` always includes
`TestEverySettingIsListedWithItsDefault` (the setting is shown by
`msm config` with the contract's default) plus the test of the package that
acts on it, where one names the setting. A setting no code reads is a
deviation, not an implementation: `DEBUG` (DEV-035), `VERSIONING_*` (DEV-005),
`UPDATE_URL` (DEV-006) and the `CONFIRM_*` family (DEV-034).

## Deferred

| Row | What is not done, and who does it |
|---|---|
| SURF-027 | `init/msm.service` (system-wide systemd unit): P15 ships service definitions; the commands they run exist |
| SURF-028 | `init/msm@.service` (per-server unit): P15 |
| SURF-029 | Debian, Red Hat and Arch installers: P15 installs the binary, the data directories, the configuration, the schedule that runs `msm maintenance run <job>` and the shell completions in [`completions/`](../../completions/) |

The schedule behind SURF-030 to SURF-037 follows the same rule: the Go jobs
are implemented and tested, and P15 installs the timers that run them (DEV-012).
