#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export GOCACHE="$tmp/go-cache"

legacy_deps=$(go list -deps ./... | grep -E 'github.com/luthermonson/shipmates/internal/(streamjson|permissions|fleet)$' || true)
test -z "$legacy_deps" || { printf '%s\n' "$legacy_deps" >&2; exit 1; }

if grep -REn --include='*.go' --exclude='*_test.go' 'internal/(streamjson|permissions|fleet)"|exec\.(Command|CommandContext)\([^[:cntrl:]]*"legacyagent"|LookPath\("legacyagent"' internal; then
  echo 'legacy production dependency or spawn remains' >&2
  exit 1
fi

if grep -REni --include='*.go' --exclude='*_test.go' 'multipart|image_url|data:image|attachment[_-]?id|\.shipmates/inbox|/upload|file[_-]?upload' internal/server internal/dashboard; then
  echo 'forbidden M12 upload, URL, inbox, or attachment authority remains' >&2
  exit 1
fi

for dir in internal/streamjson internal/permissions internal/fleet; do
  test ! -d "$dir" || test -z "$(find "$dir" -type f -print -quit)" || { echo "legacy package remains: $dir" >&2; exit 1; }
done

if grep -REni '\.legacy-runtime|remotedialer|pending/resolve|voice|pty|cron|slash.command' docs/fleet-architecture.md catalog/commands catalog/routing; then
  echo 'stale runnable documentation remains' >&2
  exit 1
fi

go build -o "$tmp/shipmates" .
help=$($tmp/shipmates --help)
for removed in pending allow deny; do
  ! grep -Eq "^[[:space:]]+$removed([[:space:]]|$)" <<<"$help" || { echo "removed command reachable: $removed" >&2; exit 1; }
done

echo 'Running complete locally runnable M11 command/server/project gates...'
go test ./internal/commands ./internal/server ./internal/project -run 'TestM11' -count=1
go test ./internal/commands ./internal/server -run 'TestM11(ExactPublicCommandAndSubcommandTree|RemovedCommandSpellingsNeverDispatch|HostileLegacyRuntimeAndLegacyCanariesAreUnreachable|ProductionDependencyAndReachabilityGate|RemovedLegacyRoutesAreUnregistered|ShutdownRequiresExactLocalControlAuthority|ServerConstructionCreatesNoLegacyState|ProductionServerStartupShutdownLeavesNoLegacyAuthority)$' -count=10

echo 'Running M12 local-image contract gates...'
go test ./internal/turninput ./internal/codexapp ./internal/livesession ./internal/server ./internal/dashboard ./internal/commands -run 'TestM12|Test.*Image' -count=1

echo 'Checking whether unrestricted loopback production E2Es are runnable...'
lifecycle_json="$tmp/server-lifecycle.json"
go test -json ./internal/server -run '^TestM11ProductionServerStartupShutdownLeavesNoLegacyAuthority$' -count=1 >"$lifecycle_json"
if grep -q '"Action":"skip"' "$lifecycle_json"; then
  echo 'Loopback listeners are prohibited here; production E2Es remain compiled but are deferred to unrestricted CI.'
else
  echo 'Running hostile-guarded production delegation, approval, observation, steer, and interrupt E2Es...'
  go test ./internal/commands -run '^(TestLiveCommandsRejectStaleCrossProjectServerEndToEnd|TestProductionFleetShipObserveRemoteSteerVerticalSlice|TestProductionFleetShipRemoteInterruptVerticalSlice)$' -count=1
fi

windows_test="$tmp/shipmates-commands-m11-windows-amd64.test.exe"
echo 'Cross-compiling Windows-native hostile-path gate...'
GOOS=windows GOARCH=amd64 go test -c ./internal/commands -o "$windows_test"
test -s "$windows_test" || { echo 'Windows M11 test artifact is missing or empty' >&2; exit 1; }
printf 'Windows native runtime command (PowerShell):\n  .\\%s -test.run %s -test.count=1 -test.v\n' \
  "$(basename "$windows_test")" "'^TestM11HostileLegacyRuntimeAndLegacyCanariesAreUnreachable$'"
printf 'Windows CI source command (PowerShell):\n  go test .\\internal\\commands -run %s -count=1 -v\n' \
  "'^TestM11HostileLegacyRuntimeAndLegacyCanariesAreUnreachable$'"

mkdir -p "$tmp/bin" "$tmp/project"
cat >"$tmp/bin/legacyagent" <<EOF
#!/usr/bin/env sh
echo invoked >"$tmp/legacyagent-invoked"
exit 99
EOF
chmod +x "$tmp/bin/legacyagent"
(
  cd "$tmp/project"
  PATH="$tmp/bin:$PATH" LEGACY_RUNTIME_CONFIG_DIR="$tmp/hostile" FORBIDDEN_PROVIDER_API_KEY=hostile "$tmp/shipmates" init
  PATH="$tmp/bin:$PATH" "$tmp/shipmates" add quartermaster
  PATH="$tmp/bin:$PATH" "$tmp/shipmates" add skipper
  PATH="$tmp/bin:$PATH" "$tmp/shipmates" list >/dev/null
)
test ! -e "$tmp/legacyagent-invoked" || { echo 'hostile legacyagent executable was invoked' >&2; exit 1; }
test ! -e "$tmp/project/.legacy-runtime" || { echo 'ordinary workflow created .legacy-runtime state' >&2; exit 1; }
test ! -e "$tmp/project/.codex/agents/captain.toml" || { echo 'captain agent was installed' >&2; exit 1; }

echo 'Codex runtime closure verified'
