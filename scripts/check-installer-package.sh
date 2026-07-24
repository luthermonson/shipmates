#!/usr/bin/env bash
set -euo pipefail

# Offline packaging check. It builds and inspects artifacts only; it never
# installs, starts a service, provisions credentials, contacts Fleet, or runs
# qualification.
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

manifest="$tmp/manifest.json"
GOCACHE="${GOCACHE:-$tmp/go-cache}" go run ./cmd/shipmates-installer-manifest >"$manifest"
jq -e '.schema == "shipmates.install.manifest.v1" and .release == "shipmates-runtime-v1" and (.assets | length == 3)' "$manifest" >/dev/null
jq -e '.assets[] | select(.destination == "/usr/libexec/shipmates/shipmates-m3-qualifier-run" or .destination == "/usr/libexec/shipmates/shipmates-cgroup-launcher" or .destination == "/etc/systemd/system/shipmates-m3-qualifier.service")' "$manifest" >/dev/null

GOCACHE="${GOCACHE:-$tmp/go-cache}" GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$tmp/shipmates" ./
test -s "$tmp/shipmates"
test -f docs/installer-platforms.md
test -f docs/cli-reference.md
test -f docs/security.md
test -f internal/installer/assets/shipmates-m3-qualifier.service
test -x internal/installer/payloads/linux_amd64/shipmates-m3-qualifier-run
test -x internal/installer/payloads/linux_amd64/shipmates-cgroup-launcher
test -x internal/installer/payloads/linux_arm64/shipmates-m3-qualifier-run
test -x internal/installer/payloads/linux_arm64/shipmates-cgroup-launcher
test "$(grep -R -l 'sudo shipmates install' docs README.md | wc -l)" -ge 1
test "$(grep -R -l 'shipmates-runtime-v1' internal/installer scripts magefile.go | wc -l)" -ge 1

epoch=${SOURCE_DATE_EPOCH:-1700000000}
for n in one two; do
  dir="$tmp/archive-$n/shipmates-runtime-v1"
  mkdir -p "$dir"
  cp "$tmp/shipmates" "$dir/shipmates"
  cp "$manifest" "$dir/shipmates-installer-manifest.json"
  cp README.md LICENSE docs/installer-platforms.md "$dir/"
  (cd "$tmp/archive-$n" && sha256sum shipmates-runtime-v1/* > shipmates-runtime-v1.sha256)
  tar --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner -C "$tmp/archive-$n" -czf "$tmp/$n.tar.gz" shipmates-runtime-v1
done
cmp "$tmp/one.tar.gz" "$tmp/two.tar.gz"
tar -tzf "$tmp/one.tar.gz" | sort >"$tmp/members"
diff -u <(printf '%s\n' \
  'shipmates-runtime-v1/' \
  'shipmates-runtime-v1/LICENSE' \
  'shipmates-runtime-v1/README.md' \
  'shipmates-runtime-v1/installer-platforms.md' \
  'shipmates-runtime-v1/shipmates' \
  'shipmates-runtime-v1/shipmates-installer-manifest.json') "$tmp/members"
echo 'installer package: manifest, docs, archive allowlist, checksum, and reproducibility PASS'
