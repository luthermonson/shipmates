# Operations guide

This guide covers normal Linux operation, verification, service lifecycle,
recovery, upgrades, and Fleet deployment.

## Daily workflow

For a whole-project outcome, begin with the skipper and sail only after reviewing
the complete plan:

```bash
shipmates open skipper
shipmates sail --dry-run
shipmates sail
```

Inspect `.shipmates/voyage.json` before sailing. On failure, read the persisted
state path printed by `sail`; fix the blocker and use `--retry-failed` only when
repeating those tasks is safe.

Use one-shot delegation for a bounded turn:

```bash
shipmates ask backend 'Implement the parser fix and run focused tests.'
```

Use the dashboard when steering or approval mediation is likely:

```bash
shipmates open backend
```

Use explicit live controls from separate terminals or scripts:

```bash
shipmates live backend 'Investigate the failure.'
shipmates feed backend --follow
shipmates tell backend SESSION THREAD TURN 'Check the edge case too.'
```

Prefer small persona-specific tasks. Parallelize across personas, not multiple
turns for one persona. Shipmates serializes each persona to protect continuity.

## Preflight

```bash
command -v shipmates
command -v codex
codex login status
git rev-parse --show-toplevel
shipmates list
shipmates policy validate backend
```

The Codex login belongs to the OS user running Shipmates. Services need a usable
`PATH`, Codex home, and credentials. Never copy tokens into `shipmates.yaml`.

## Local server lifecycle

For diagnosis, run the project server in the foreground:

```bash
shipmates server serve
```

Attach from another terminal with `shipmates open <persona>`, then stop through
the authenticated path:

```bash
shipmates server stop
```

Do not edit discovery records to retarget clients. If state is stale, confirm no
Shipmates or Codex child remains and remove only transient state. Memory,
manifest, persona definitions, and policy are durable.

## Cancellation and shutdown

Ctrl-C propagates cancellation, terminates and reaps Codex, and does not advance
continuity after failure. Dashboard `/detach` and EOF release the controller but
do not interrupt work. Use `/interrupt` when termination is intended.

## Upgrades

```bash
git pull --ff-only
./scripts/install-codex-adaptation.sh
shipmates --version
shipmates update
```

Review conflicts instead of accepting content automatically. Existing memory is
not an update target.

## Verification

```bash
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
```

The decisive Codex-only smoke check is:

```bash
tmp=$(mktemp -d)
cd "$tmp"
git init -q
shipmates init --crew backend
test -f .codex/agents/backend.toml
shipmates ask backend 'Do not modify files. Reply exactly: Codex-only smoke test passed.'
```

Inspect the generated tree as part of this gate. Native Windows/macOS builds
and WSL setup are outside this repository's supported verification scope.

## Failure diagnosis

### Persona is not installed

Run `shipmates list` and confirm `.codex/agents/<persona>.toml` is a regular
managed file. Use `shipmates add <persona>`; do not hand-create a placeholder.

### Codex cannot be started

Check `command -v codex`, version, login, and the service environment. Shipmates
does not search for or fall back to another backend.

### A turn is already active

Read `shipmates feed <persona>` and attach with `shipmates open <persona>`. Do
not delete locks while a worker may exist.

### Update conflict

Inspect local and catalog changes. Re-run with `--accept ours` to keep local
content or `--accept theirs` to accept the catalog. Memory is unchanged.

### Stale controller or server state

Allow lease expiry, verify process ownership, then restart the local server.
Stale requests are rejected instead of retargeted.

## Fleet deployment checklist

1. Run the observer behind TLS with a durable authority store.
2. Create separate ship, observer, and operator identities.
3. Restrict observer credentials to the minimum ship allowlist.
4. Put credentials in environment or protected service state, never project
   configuration or command history.
5. Enroll the ship, confirm status, then start observation.
6. Verify read-only roster, snapshot, events, and follow first.
7. Test control against a disposable active turn.
8. Verify audit, replay rejection, expiry, rotation, and revocation.

Fleet is not a remote shell, job starter, approval channel, or file transport.
