#!/usr/bin/env bash
# Rehearses a migration and its rollback on a copy of a legacy installation
# built from the files this repository ships (msm.conf, cron/msm, init/msm),
# using the msm executable named by the first argument. Everything happens
# below a temporary directory (msm migrate --root): no service, cron file or
# real world is touched, and nothing needs root. CI runs it on Linux and
# macOS; run it yourself before migrating a real host, with your own files
# copied into the tree (docs/migration.md).
set -euo pipefail

msm="${1:?usage: rehearse.sh /path/to/msm}"
repo="$(cd "$(dirname "$0")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
root="$work/root"
me="$(id -un)"

mkdir -p "$root/etc/cron.d" "$root/etc/init.d" "$root/usr/local/bin" \
  "$root/opt/msm/servers/alpha/worldstorage/world" "$root/opt/msm/jars/minecraft"
sed "s/\"minecraft\"/\"$me\"/" "$repo/msm.conf" > "$root/etc/msm.conf"
sed "s/ minecraft / $me /" "$repo/cron/msm" > "$root/etc/cron.d/msm"
cp "$repo/init/msm" "$root/etc/init.d/msm"
chmod 755 "$root/etc/init.d/msm"
ln -s /etc/init.d/msm "$root/usr/local/bin/msm"
printf 'level data' > "$root/opt/msm/servers/alpha/worldstorage/world/level.dat"
: > "$root/opt/msm/servers/alpha/active"
printf '\177ELF stand-in for the Go executable' > "$work/msm-go"
chmod 755 "$work/msm-go"

# snapshot lists every path below $1 (except the migration's own state) with
# its type, mode and content checksum.
snapshot() {
  (cd "$1" && find . -path ./var -prune -o -print | LC_ALL=C sort | while IFS= read -r p; do
    if [ -L "$p" ]; then
      printf '%s -> %s\n' "$p" "$(readlink "$p")"
    elif [ -f "$p" ]; then
      printf '%s %s %s\n' "$p" "$(ls -ld "$p" | cut -c1-10)" "$(shasum -a 256 < "$p" | cut -d' ' -f1)"
    else
      printf '%s %s\n' "$p" "$(ls -ld "$p" | cut -c1-10)"
    fi
  done)
}

snapshot "$root" > "$work/before"

echo "== preflight"
"$msm" migrate preflight --root "$root"

echo "== apply"
"$msm" migrate apply --root "$root" --binary "$work/msm-go" --external-backup --yes
test ! -e "$root/etc/cron.d/msm"
test -f "$root/etc/cron.d/msm-maintenance"
grep -q 'maintenance run backup' "$root/etc/cron.d/msm-maintenance"
test ! -x "$root/etc/init.d/msm"
test -f "$root/usr/local/bin/msm" && test ! -L "$root/usr/local/bin/msm"
test -f "$root/etc/systemd/system/msm.service"
test -f "$root/opt/msm/servers/alpha/active"
test "$(cat "$root/opt/msm/servers/alpha/worldstorage/world/level.dat")" = "level data"
if "$msm" migrate apply --root "$root" --binary "$work/msm-go" --external-backup --yes 2> "$work/again.err"; then
  echo "a second apply succeeded"; exit 1
fi
grep -q 'already migrated' "$work/again.err"

echo "== rollback"
"$msm" migrate rollback --root "$root" --yes
snapshot "$root" > "$work/after"
if ! diff "$work/before" "$work/after"; then
  echo "rollback did not restore the installation byte for byte"; exit 1
fi
echo "rehearsal ok: migrated and rolled back with no difference"
