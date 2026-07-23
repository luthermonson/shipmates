# Operations guide

This guide covers normal Linux operation, verification, service lifecycle,
recovery, upgrades, and Fleet deployment.

## Packaged runtime installation

For released Linux binaries, the single offline runtime entry point is
`sudo shipmates install`. Use `--dry-run --json` to inspect the fixed manifest
and platform composition before changing files. The installer is idempotent,
refuses drift, never starts a unit or qualification, and retains its journal,
credentials, authority, and state when `--uninstall` removes Shipmates-owned
release assets. Limited WSL and non-systemd containers retain ordinary
Shipmates operation when hardened containment is unavailable. See
[Installer and platform contract](installer-platforms.md).

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

Fleet exact-turn control requires separate protected credentials. Discover a
fresh bounded target first, then pass only that opaque reference to the matching
control command; discovery never starts or queues work:

```bash
shipmates fleet steer-targets --fleet "$FLEET_URL" --credential-file "$STEER_CREDENTIAL" --json
shipmates fleet interrupt-targets --fleet "$FLEET_URL" --credential-file "$INTERRUPT_CREDENTIAL" --json
```

Observer credentials and the opposite operation credential are refused before
target disclosure. Do not copy private local session, thread, or turn IDs into
Fleet commands or reports.

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

## Fleet deployment

Use the [Fleet v0.4.0-beta.2 runbook](fleet-beta2-runbook.md) as the single
zero-to-observed-real-Codex procedure. It covers public bootstrap, verified TLS,
read-only observation, exact-turn steering/interruption, credential lifecycle,
recovery, containerized WSL evidence, UI, bounded fake-Codex load/soak, and
teardown. Keep this operations guide focused on general Shipmates lifecycle;
do not create a second Fleet command sequence here.
