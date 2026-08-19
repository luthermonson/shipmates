#!/bin/sh
# safe-test.sh -- memory-watchdog Go test runner for Linux / macOS / CI.
#
# `go test ./...` spawns one child test binary (`<pkg>.test`) per package. A test
# that allocates without bound blows up in that CHILD, not in the parent `go`
# process, so watching only `go` misses the runaway. On 2026-08-16 a
# `server.test.exe` reached ~1 TB of commit charge and took the whole desktop
# down; an earlier incident hit ~236 GB via an infinite filepath.Dir loop.
#
# This wrapper launches `go test`, then polls the resident memory of every
# `<pkg>.test` process descended from THAT `go` invocation (only ours -- a
# sibling `go test` elsewhere is left alone). It records a per-package peak, and
# if any child crosses the cap it kills that child and the parent `go` tree,
# prints which package tripped and the tail of the output, and exits non-zero.
# A modest wall-clock timeout is enforced too.
#
# On Windows use scripts/safe-test.ps1 instead -- there the runaway is commit
# charge (virtual), which PowerShell samples via PagedMemorySize64.
#
# Usage:
#   scripts/safe-test.sh [-t TARGET] [-c CAP_MB] [-m TIMEOUT_MIN] [-p POLL_SECS] [-- go-test-args...]
#
# Examples:
#   scripts/safe-test.sh
#   scripts/safe-test.sh -t ./internal/permissions/... -c 500
#   scripts/safe-test.sh -t ./internal/server/... -- -run TestRingBuffer -v
#
# Env overrides: SAFE_TEST_CAP_MB, SAFE_TEST_TIMEOUT_MIN, SAFE_TEST_POLL_SECS.

set -u

TARGET='./...'
CAP_MB="${SAFE_TEST_CAP_MB:-2000}"
TIMEOUT_MIN="${SAFE_TEST_TIMEOUT_MIN:-15}"
POLL_SECS="${SAFE_TEST_POLL_SECS:-1}"
TAIL_LINES=40

while [ $# -gt 0 ]; do
    case "$1" in
        -t|--target)       TARGET="$2"; shift 2 ;;
        -c|--cap-mb)       CAP_MB="$2"; shift 2 ;;
        -m|--timeout-min)  TIMEOUT_MIN="$2"; shift 2 ;;
        -p|--poll-secs)    POLL_SECS="$2"; shift 2 ;;
        --)                shift; break ;;
        -h|--help)
            sed -n '2,30p' "$0"; exit 0 ;;
        *)
            echo "safe-test: unknown option '$1'" >&2; exit 2 ;;
    esac
done
EXTRA_ARGS="$*"

CAP_KB=$((CAP_MB * 1024))
TIMEOUT_SECS=$((TIMEOUT_MIN * 60))

LOG="$(mktemp "${TMPDIR:-/tmp}/shipmates-safe-test.XXXXXX")"
PEAKS="$(mktemp "${TMPDIR:-/tmp}/shipmates-safe-test-peaks.XXXXXX")"
SNAP="$(mktemp "${TMPDIR:-/tmp}/shipmates-safe-test-snap.XXXXXX")"

TAIL_PID=''
GO_PID=''

cleanup() {
    [ -n "$TAIL_PID" ] && kill "$TAIL_PID" 2>/dev/null
    rm -f "$LOG" "$PEAKS" "$SNAP" 2>/dev/null
}
trap cleanup EXIT INT TERM

# BFS the process table for every descendant pid of $1. ps -A is POSIX; rss is
# KB and comm is the command basename on Linux / the exe path on macOS (we strip
# the path). One awk pass builds the tree and prints, for each descendant whose
# command ends in `.test`: "<pkg> <pid> <rssKB>".
snapshot() {
    ps -Ao pid=,ppid=,rss=,comm= | awk -v root="$1" '
    {
        pid=$1; ppid=$2; rss=$3; comm=$4;
        RSS[pid]=rss; COMM[pid]=comm;
        kids[ppid] = kids[ppid] " " pid;
    }
    END {
        n=0; queue[n++]=root;
        for (i=0; i<n; i++) {
            m=split(kids[queue[i]], a, " ");
            for (j=1; j<=m; j++) if (a[j] != "") { desc[a[j]]=1; queue[n++]=a[j]; }
        }
        for (p in desc) {
            c=COMM[p]; sub(/.*\//, "", c);
            if (c ~ /\.test$/) { pkg=c; sub(/\.test$/, "", pkg); print pkg, p, RSS[p]; }
        }
    }'
}

# Every descendant pid of $1, one per line (for a full tree kill).
descendant_pids() {
    ps -Ao pid=,ppid= | awk -v root="$1" '
    { kids[$2] = kids[$2] " " $1 }
    END {
        n=0; queue[n++]=root;
        for (i=0; i<n; i++) {
            m=split(kids[queue[i]], a, " ");
            for (j=1; j<=m; j++) if (a[j] != "") { print a[j]; queue[n++]=a[j]; }
        }
    }'
}

kill_tree() {
    for p in $(descendant_pids "$1"); do kill -9 "$p" 2>/dev/null; done
    kill -9 "$1" 2>/dev/null
}

# Merge a fresh sample into the peaks file (update if larger, else insert).
update_peak() {
    awk -v k="$1" -v r="$2" '
        BEGIN { done=0 }
        $1==k { if (r+0 > $2+0) $2=r; print; done=1; next }
        { print }
        END { if (!done) print k, r }
    ' "$PEAKS" > "$PEAKS.tmp" && mv "$PEAKS.tmp" "$PEAKS"
}

set -- test -count=1
[ -n "$EXTRA_ARGS" ] && set -- "$@" $EXTRA_ARGS
set -- "$@" "$TARGET"

echo "safe-test: go $*"
echo "safe-test: cap=${CAP_MB} MB  timeout=${TIMEOUT_MIN} min  poll=${POLL_SECS} s"
printf '%s\n' "------------------------------------------------------------"

go "$@" > "$LOG" 2>&1 &
GO_PID=$!

# Live view of the captured output while we poll.
tail -f "$LOG" 2>/dev/null &
TAIL_PID=$!

START="$(date +%s)"
TRIP_PKG=''
TRIP_PID=''
TRIP_RSS=''
TIMED_OUT=0

while kill -0 "$GO_PID" 2>/dev/null; do
    NOW="$(date +%s)"
    if [ $((NOW - START)) -ge "$TIMEOUT_SECS" ]; then
        TIMED_OUT=1
        break
    fi

    snapshot "$GO_PID" > "$SNAP"
    # `while read < file` runs in the current shell, so trip vars survive.
    while read -r pkg cpid rss; do
        [ -z "$pkg" ] && continue
        if [ "$rss" -gt "$CAP_KB" ]; then
            TRIP_PKG="$pkg"; TRIP_PID="$cpid"; TRIP_RSS="$rss"
            break
        fi
        update_peak "$pkg" "$rss"
    done < "$SNAP"

    [ -n "$TRIP_PKG" ] && break
    sleep "$POLL_SECS"
done

if [ -n "$TRIP_PKG" ]; then
    kill -9 "$TRIP_PID" 2>/dev/null
    kill_tree "$GO_PID"
elif [ "$TIMED_OUT" -eq 1 ]; then
    kill_tree "$GO_PID"
fi

wait "$GO_PID" 2>/dev/null
GO_STATUS=$?

# Stop the live tail before we print our own summary.
[ -n "$TAIL_PID" ] && kill "$TAIL_PID" 2>/dev/null
TAIL_PID=''

echo ""
printf '%s\n' "------------------------------------------------------------"
echo "safe-test: per-package peak resident memory"
if [ -s "$PEAKS" ]; then
    sort -k2 -n -r "$PEAKS" | awk '{ printf "  %8.1f MB  %s\n", $2/1024, $1 }'
else
    echo "  (no *.test child observed -- packages may have had no tests, or ran faster than one poll)"
fi
printf '%s\n' "------------------------------------------------------------"

show_tail() {
    echo "safe-test: last ${TAIL_LINES} line(s) of output:"
    tail -n "$TAIL_LINES" "$LOG" 2>/dev/null | sed 's/^/  | /'
}

if [ -n "$TRIP_PKG" ]; then
    echo ""
    echo "WATCHDOG: package '$TRIP_PKG' (pid $TRIP_PID) crossed the ${CAP_MB} MB cap at $((TRIP_RSS / 1024)) MB resident."
    echo "WATCHDOG: killed it and the parent 'go' process. This is the runaway -- fix the test before re-running the full suite."
    show_tail
    exit 1
fi
if [ "$TIMED_OUT" -eq 1 ]; then
    echo ""
    echo "WATCHDOG: timed out after ${TIMEOUT_MIN} minute(s); killed the 'go' process tree."
    show_tail
    exit 1
fi
if [ "$GO_STATUS" -ne 0 ]; then
    echo ""
    echo "safe-test: go test failed (exit $GO_STATUS)."
    show_tail
    exit "$GO_STATUS"
fi

echo ""
echo "safe-test: clean pass."
exit 0
