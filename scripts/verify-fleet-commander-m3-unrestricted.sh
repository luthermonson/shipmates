#!/usr/bin/env bash
set -euo pipefail

# One deliberately opt-in, fail-closed M3 qualifier. This is not a managed
# substitute: it must run on an unrestricted Linux host with real cgroupfs,
# pidfds, a trusted placement helper, and a real TLS Fleet destination.
if [[ "${SHIPMATES_M3_UNRESTRICTED:-}" != "1" ]]; then
  echo 'refusing unrestricted M3 qualifier: set SHIPMATES_M3_UNRESTRICTED=1' >&2
  exit 2
fi

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
evidence_dir=${SHIPMATES_M3_EVIDENCE_DIR:-"$tmp/evidence"}
mkdir -p "$evidence_dir"
evidence="$evidence_dir/evidence.jsonl"

fail() {
  printf '{"event":"prerequisite_failed","reason":"%s"}\n' "$1" | tee -a "$evidence" >&2
  exit 1
}

[[ "$(uname -s)" == Linux ]] || fail 'linux_required'
[[ -n "${SHIPMATES_CGROUP_ROOT:-}" ]] || fail 'SHIPMATES_CGROUP_ROOT_required'
fleet_config=${SHIPMATES_M3_FLEET_CONFIG:-}
[[ -n "$fleet_config" ]] || fail 'SHIPMATES_M3_FLEET_CONFIG_required'

# This validates the disposable delegated subtree and then performs the real
# protected TLS/M7 capability handshake. It fails before any M3 lifecycle work.
GOCACHE="$tmp/go-cache" go run ./cmd/shipmates-m3-probe \
  --cgroup-root "$SHIPMATES_CGROUP_ROOT" \
  --fleet-config "$fleet_config" \
  --evidence-dir "$evidence_dir" || fail 'delegated_probe_failed'

codex=${SHIPMATES_M3_CODEX_BIN:-codex}
command -v "$codex" >/dev/null 2>&1 || fail 'codex_binary_unavailable'

# Verify the kernel pidfd path without creating a child or touching external
# state. Production launch is gated by the command configuration above and the
# real production tests below.
python3 -c 'import os; fd=os.pidfd_open(os.getpid()); os.close(fd)' 2>/dev/null || fail 'pidfd_unavailable'
printf '{"event":"prerequisites","linux":true,"cgroup_v2":true,"pidfd":true,"protected_tls_m7_m3_binding":true,"evidence":"sanitized"}\n' >>"$evidence_dir/evidence.jsonl"

# These are the existing production-composed outbound gates. They are opt-in,
# bounded by the caller's test timeout, and nonzero on any lifecycle failure.
GOCACHE="$tmp/go-cache" go test ./internal/commands ./internal/fleettunnel \
  -run '^(TestProductionFleetShipObserveRemoteSteerVerticalSlice|TestProductionInterruptRestartReplaysUnfinishedAsFleetRestarted|TestRunProjectedFairlySchedulesCommanderAfterInitialSnapshot)$' \
  -count=1

printf '{"event":"production_gates","result":"pass","external_values_redacted":true}\n' >>"$evidence_dir/evidence.jsonl"
cat "$evidence_dir"/*.json "$evidence_dir"/*.jsonl 2>/dev/null || true
