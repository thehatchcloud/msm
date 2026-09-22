# Approved compatibility deviations

These are requirements for the Go port, not optional cleanup. A behavior not
listed here must be preserved or proposed for review before implementation.

| ID | Legacy behavior | Required Go behavior | Owner | Rationale |
|---|---|---|---|---|
| DEV-001 | Global and per-server config is interpreted through Bash `eval`; invocation is passed through a shell command string | Parse literal supported assignments and placeholders; execute Java/screen with argument vectors; reject executable shell syntax with a location and migration message | P03, P06 | Configuration must not be arbitrary code |
| DEV-002 | JAR and updater downloads use `wget --no-check-certificate` and do not verify a release manifest | Require verified TLS, deadlines, bounded responses, atomic staging, and available upstream hashes/signatures; the manager updater requires independently trusted release provenance | P07, P15 | Prevent interception and corrupted publication |
| DEV-003 | Delete prompts can still be awkward in noninteractive use and server deletion stops a running server after confirmation | Require a TTY confirmation or explicit noninteractive flag, show the resolved target, reject unsafe roots/symlinks, and document running-server behavior | P05, P07 | Avoid hangs and accidental recursive deletion |
| DEV-004 | Unknown commands and some operational failures can exit zero; several source call sites use undefined symbolic exit names | Define stable named nonzero usage/operational exit codes and deterministic aggregate status for bulk work | P14 | Automation must be able to detect failure |
| DEV-005 | Version behavior is supplied by sourced `.sh` files, with profiles only through Minecraft 1.7-era log formats | Encode profiles as inert Go data, retain the five legacy profiles, and add an explicitly tested modern profile; deprecated commands return capability errors where appropriate | P08 | Preserve legacy behavior without executing code and avoid false success on modern servers |
| DEV-006 | Self-update recursively downloads/replaces scripts and version files from the Bash project's update URL | Update only this fork's platform-specific Go release artifact after provenance verification, with atomic replacement and rollback | P15 | A compiled application cannot safely use the legacy script updater |
| DEV-007 | Live backup cleanup is process-local; interruption can leave saving disabled or publish partial archives | Use staged archives, durable pending-operation state, cleanup/recovery, and restore tests; disclose full-consistency limits for live plugin data | P10, P11 | A backup must be recoverable and must not endanger the live server |
| DEV-008 | RAM worlds default enabled and can treat an ordinary configured directory as a RAM disk | Default new installations to disabled, require explicit validated RAM-backed storage, preserve imported intent, and report durability limits | P12 | Avoid a misleading or unsafe default |
| DEV-009 | JAR-group rename leaves existing JAR symlinks pointing at the old path | Update affected references atomically or refuse the rename with actionable output | P07 | A reported-success rename must not break servers |
| DEV-010 | Log confirmation can match old/unrelated lines, and waits may be unbounded | Correlate from a fresh offset/event, model readiness separately from liveness, and enforce deadlines | P04, P06, P08 | Prevent false success and hung automation |
| DEV-011 | Complete backup symlink-following can capture data outside the managed server tree | Preserve the configuration option but preview/validate external targets and refuse unsafe or recursive capture by default | P03, P10 | Prevent unintended data disclosure and archive recursion |
| DEV-012 | Maintenance retention uses broad `find | xargs rm` pipelines | Implement scoped Go pruning with dry-run, root validation, locking, and recovery retention rules | P13 | Avoid unsafe deletion and shell dependencies |

## Decision rule

Compatibility means preserving user intent, data layout, and useful operational
semantics. It does not mean preserving shell injection, disabled certificate
checks, false-positive success, unbounded waits, unsafe recursive deletion, or
obsolete behavior that modern Minecraft explicitly rejects.
