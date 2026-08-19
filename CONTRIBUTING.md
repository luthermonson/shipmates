# Contributing to Shipmates

## Running tests safely

**Do not run the raw `go test ./...` during development.** On 2026-08-16 an
in-development `server.test.exe` grew to roughly **1 TB of commit charge**; the
Windows Terminal hosting several agent sessions then hard-faulted with
`E_OUTOFMEMORY` and every session died at once. An earlier incident OOM'd the
machine at ~236 GB via an infinite `filepath.Dir` loop. `go test ./...` spawns
one child binary (`<pkg>.test`) per package, so a runaway allocates in that
*child* — watching only the parent `go` process misses it entirely.

Run the suite through the memory watchdog instead. It launches `go test`, polls
the commit charge / resident memory of every `*.test` child descended from that
run, records a per-package peak, and if any child crosses a cap it kills that
child **and** the parent `go` tree, then prints which package tripped and the
tail of its output. It exits non-zero if it kills anything or if `go test`
fails, so it is safe to use in CI or a pre-push hook.

Healthy shipmates packages peak near ~100 MB, so the default 2000 MB cap leaves
plenty of headroom while still catching a runaway long before it can hurt the
machine.

**Windows** (the machine that keeps dying — use this):

```powershell
# whole suite
powershell -ExecutionPolicy Bypass -File scripts\safe-test.ps1

# one package, tighter cap
powershell -ExecutionPolicy Bypass -File scripts\safe-test.ps1 -Target ./internal/server/... -CapMB 500

# pass args through to `go test` after --
powershell -ExecutionPolicy Bypass -File scripts\safe-test.ps1 -Target ./internal/server/... -- -run TestRingBuffer -v
```

**Linux / macOS / CI:**

```sh
# whole suite
scripts/safe-test.sh

# one package, tighter cap
scripts/safe-test.sh -t ./internal/server/... -c 500

# pass args through to `go test` after --
scripts/safe-test.sh -t ./internal/server/... -- -run TestRingBuffer -v
```

Flags: `-Target`/`-t` (package pattern, default `./...`), `-CapMB`/`-c` (per-test
cap in MB, default 2000), `-TimeoutMinutes`/`-m` (wall-clock timeout, default
15), `-PollSeconds`/`-p` (sample interval, default 1). The POSIX script also
reads `SAFE_TEST_CAP_MB`, `SAFE_TEST_TIMEOUT_MIN`, and `SAFE_TEST_POLL_SECS`.

> Windows measures **commit charge** (`PagedMemorySize64`), because that is what
> the OOM was — virtual/committed memory, not working set, which stayed small
> while commit ran to a terabyte. The POSIX script measures resident memory
> (RSS), since a Go process on Linux reserves a large but harmless virtual
> address space that would make a virtual-size cap useless there.

### The rule that caused the incident: never verify a bound by removing it

The 1 TB test was a *bounded*-behavior test written the wrong way. When you test
that some limit holds — a ring-buffer cap, an HTTP body-size limit, an
event-log cap, a bounded channel, a truncation — **do not** verify the
"fails without the guard" half by disabling the bound and letting the code run
unbounded. That turns a bounded test into an unbounded allocator, and it is
exactly how a `*.test` binary reaches a terabyte.

Instead:

- **Assert against an injected small cap.** Make the bound a parameter (or a
  field/option) and set it to something tiny in the test — a 4-entry ring
  buffer, a 1 KB body limit — then prove the (N+1)th item is dropped/rejected.
  A test that fills a 4-slot buffer proves the same invariant as one that tries
  to overflow a production-sized buffer, without allocating anything.
- **Never loop `append`/allocate "until it breaks"** to demonstrate the failure
  mode. Assert the boundary condition directly at a small, fixed size.
- If a test genuinely must exercise large allocation, **run it under the
  watchdog** with an explicit low `-CapMB` so a mistake is a killed process
  rather than a dead machine.

If you find an existing test that proves a bound by disabling it, rewrite it to
assert against an injected small cap.
