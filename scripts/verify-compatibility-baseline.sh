#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source_file="$repo_root/init/msm"
baseline_file="$repo_root/compatibility/baseline.env"

fail() {
  printf 'compatibility baseline: %s\n' "$*" >&2
  exit 1
}

[[ -f "$source_file" ]] || fail "missing init/msm"
[[ -f "$baseline_file" ]] || fail "missing compatibility/baseline.env"

# This file contains constants only. Validate its grammar before reading it.
if grep -Ev '^(UPSTREAM_REPOSITORY|UPSTREAM_COMMIT|UPSTREAM_MSM_VERSION|COMMAND_COUNT|GLOBAL_SETTING_COUNT|SERVER_SETTING_COUNT|CONFIG_FILE_KEY_COUNT|VERSION_PROFILE_COUNT|UPSTREAM_TEST_FUNCTION_COUNT)=[A-Za-z0-9._/-]+$|^$' "$baseline_file" | grep -q .; then
  fail "baseline.env contains an unexpected assignment"
fi
# shellcheck disable=SC1090
source "$baseline_file"

actual_version="$(sed -n 's/^VERSION="\([^"]*\)"/\1/p' "$source_file")"
[[ "$actual_version" == "$UPSTREAM_MSM_VERSION" ]] ||
  fail "MSM version drift: expected $UPSTREAM_MSM_VERSION, got $actual_version"

actual_commands="$(grep -c 'register_command "' "$source_file")"
fixture_commands="$(awk -F '\t' 'NR > 1 && NF { count++ } END { print count + 0 }' "$repo_root/compatibility/commands.tsv")"
[[ "$actual_commands" -eq "$COMMAND_COUNT" ]] ||
  fail "source command count drift: expected $COMMAND_COUNT, got $actual_commands"
[[ "$fixture_commands" -eq "$COMMAND_COUNT" ]] ||
  fail "command fixture count drift: expected $COMMAND_COUNT, got $fixture_commands"

tmp_root="$(mktemp -d "${TMPDIR:-/tmp}/msm-compat.XXXXXX")"
trap 'rm -rf "$tmp_root"' EXIT

actual_command_file="$tmp_root/source-commands.tsv"
fixture_command_file="$tmp_root/fixture-commands.tsv"
while IFS= read -r line; do
  [[ "$line" =~ register_command\ \"([^\"]+)\"\ \"([^\"]+)\" ]] || continue
  printf '%s\t%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" >> "$actual_command_file"
done < "$source_file"
awk -F '\t' 'NR > 1 && NF { print $2 "\t" $3 }' "$repo_root/compatibility/commands.tsv" > "$fixture_command_file"
cmp -s "$actual_command_file" "$fixture_command_file" ||
  fail "command fixture does not match init/msm registrations"

actual_setting_file="$tmp_root/source-settings.tsv"
fixture_setting_file="$tmp_root/fixture-settings.tsv"
while IFS= read -r line; do
  if [[ "$line" =~ ^[[:space:]]*register_setting\ ([A-Z_]+)(\ \"([^\"]*)\")?[[:space:]]*$ ]]; then
    printf 'global\t%s\t%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[3]:-}" >> "$actual_setting_file"
  elif [[ "$line" =~ ^[[:space:]]*register_server_setting\ ([A-Z_]+)(\ \"([^\"]*)\")?[[:space:]]*$ ]]; then
    printf 'server\t%s\t%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[3]:-}" >> "$actual_setting_file"
  fi
done < "$source_file"
awk -F '\t' 'NR > 1 && NF && ($1 == "global" || $1 == "server") { print $1 "\t" $2 "\t" $3 }' "$repo_root/compatibility/settings.tsv" > "$fixture_setting_file"
cmp -s "$actual_setting_file" "$fixture_setting_file" ||
  fail "settings fixture does not match init/msm registrations"

actual_global_settings="$(awk -F '\t' '$1 == "global" { count++ } END { print count + 0 }' "$actual_setting_file")"
actual_server_settings="$(awk -F '\t' '$1 == "server" { count++ } END { print count + 0 }' "$actual_setting_file")"
[[ "$actual_global_settings" -eq "$GLOBAL_SETTING_COUNT" ]] ||
  fail "global setting count drift: expected $GLOBAL_SETTING_COUNT, got $actual_global_settings"
[[ "$actual_server_settings" -eq "$SERVER_SETTING_COUNT" ]] ||
  fail "server setting count drift: expected $SERVER_SETTING_COUNT, got $actual_server_settings"

config_keys="$tmp_root/config-keys"
inventoried_config_keys="$tmp_root/inventoried-config-keys"
sed -n 's/^\([A-Z][A-Z0-9_]*\)=.*/\1/p' "$repo_root/msm.conf" | sort -u > "$config_keys"
awk -F '\t' '
  NR > 1 && NF && $1 == "global" { print $2 }
  NR > 1 && NF && $1 == "server" { print "DEFAULT_" $2 }
  NR > 1 && NF && $1 == "legacy-config" { print $2 }
' "$repo_root/compatibility/settings.tsv" | sort -u > "$inventoried_config_keys"
actual_config_keys="$(wc -l < "$config_keys" | tr -d ' ')"
[[ "$actual_config_keys" -eq "$CONFIG_FILE_KEY_COUNT" ]] ||
  fail "msm.conf key count drift: expected $CONFIG_FILE_KEY_COUNT, got $actual_config_keys"
missing_keys="$(comm -23 "$config_keys" "$inventoried_config_keys")"
[[ -z "$missing_keys" ]] ||
  fail "msm.conf has keys absent from settings.tsv: $missing_keys"

actual_profiles="$(find "$repo_root/versioning" -mindepth 2 -maxdepth 2 -type f -name '*.sh' | wc -l | tr -d ' ')"
[[ "$actual_profiles" -eq "$VERSION_PROFILE_COUNT" ]] ||
  fail "version-profile count drift: expected $VERSION_PROFILE_COUNT, got $actual_profiles"

actual_tests="$(grep -c '^test[A-Za-z0-9_]*()' "$repo_root/test.sh")"
[[ "$actual_tests" -eq "$UPSTREAM_TEST_FUNCTION_COUNT" ]] ||
  fail "upstream test count drift: expected $UPSTREAM_TEST_FUNCTION_COUNT, got $actual_tests"

fixture_root="$tmp_root/fixture"
cp -R "$repo_root/compatibility/fixtures/minimal" "$fixture_root"
escaped_root="${fixture_root//\\/\\\\}"
escaped_root="${escaped_root//&/\\&}"
sed "s&__FIXTURE_ROOT__&$escaped_root&g" "$fixture_root/msm.conf" > "$fixture_root/msm.conf.tmp"
mv "$fixture_root/msm.conf.tmp" "$fixture_root/msm.conf"

[[ ! -e "$fixture_root/servers/inactive-example/active" ]] ||
  fail "inactive fixture unexpectedly has an active marker"
[[ -f "$fixture_root/servers/active-example/active" ]] ||
  fail "active fixture is missing its active marker"
[[ -f "$fixture_root/servers/active-example/worldstorage/world/inram" ]] ||
  fail "RAM-intent sentinel is missing"
grep -Fq "SERVER_STORAGE_PATH=\"$fixture_root/servers\"" "$fixture_root/msm.conf" ||
  fail "temporary fixture paths were not materialized"
grep -Rq '__FIXTURE_ROOT__' "$fixture_root" &&
  fail "temporary fixture still contains unresolved root placeholders"

printf 'Compatibility baseline verified: %s commands, %s global settings, %s server settings, %s config-file keys, %s profiles, %s upstream tests.\n' \
  "$actual_commands" "$actual_global_settings" "$actual_server_settings" "$actual_config_keys" "$actual_profiles" "$actual_tests"
