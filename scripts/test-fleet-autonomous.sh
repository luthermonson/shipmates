#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
report="${SHIPMATES_TEST_REPORT:-${TMPDIR:-/tmp}/shipmates-fleet-autonomous.json}"
mkdir -p "$(dirname "$report")"

cd "$repo_root"
set +e
go test -json -count=1 -timeout=90s -run '^TestAutonomousFleet' ./internal/fleet | tee "$report"
test_status=${PIPESTATUS[0]}
set -e

echo "Fleet autonomous report: $report"
exit "$test_status"
