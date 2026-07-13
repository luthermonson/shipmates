# Getting started

This guide takes a new project from installation through one-shot delegation,
interactive work, policy inspection, and safe persona lifecycle management.

## Requirements

- Linux, macOS, or Windows with WSL for the full terminal dashboard experience.
- Go 1.26 or newer when building from source.
- An authenticated `codex` CLI on `PATH`.
- A Git repository. Codex refuses untrusted non-repository working directories.

Verify prerequisites:

```bash
go version
codex --version
codex login status
git rev-parse --show-toplevel
```

## Install from this checkout

The user-scoped installer builds the current revision and writes the binary to
`~/.local/bin/shipmates`:

```bash
./scripts/install-codex-adaptation.sh
command -v shipmates
shipmates --version
```

Ensure `~/.local/bin` is on `PATH` in non-interactive shells and service
environments as well as your interactive terminal.

## Initialize a project

Run initialization at the Git repository root:

```bash
shipmates init --crew captain,backend,security,tester
```

Initialization creates:

- `shipmates.yaml` for project configuration;
- `.codex/agents/<persona>.toml` for canonical Codex persona instructions;
- `.shipmates/policy.yaml` and persona policy overlays;
- `.shipmates/memory/<persona>/` for durable project-scoped memory;
- `.shipmates/manifest.json` for managed-file ownership and conflict detection;
- `.shipmates/sessions/` for private transient coordination state.

It does not overwrite existing memory seeds or silently replace edited managed
files. Run `shipmates list` to see catalog and installation status.

## Delegate a bounded task

```bash
shipmates ask security 'Review the current diff for authentication problems.'
shipmates ask tester --timeout 20m 'Run the focused tests and report gaps.'
```

Each persona owns separate memory and Codex continuity. A later `ask` resumes
the same Codex thread when its configuration fingerprint still matches. Use
`--fresh` to deliberately start a new thread:

```bash
shipmates ask backend --fresh 'Re-evaluate the implementation from first principles.'
```

Progress summaries go to stderr. The final persona response goes to stdout,
which makes redirection and scripting predictable.

## Attach local images

```bash
shipmates ask frontend --image ./screens/home.png 'Review this layout.'
shipmates live frontend --image ./before.png --image ./after.webp 'Compare these states.'
```

Images must be existing PNG, JPEG, GIF, or WebP files inside the canonical
project root. Shipmates validates magic bytes, count, size, containment, link
safety, and filesystem identity before handing local paths to Codex. It does
not upload, copy, retain, fetch, or decode image metadata.

## Start interactive work

Use `open` for the normal interactive workflow:

```bash
shipmates open backend
```

The dashboard attaches to one exact persona session and owns a renewable local
controller lease. Submitted text starts a turn while idle or steers the exact
active turn while working.

Dashboard commands:

- `/help` shows local controls.
- `/interrupt` interrupts the exact active turn.
- `/allow-once` approves the current mediated request once.
- `/deny` denies the current mediated request once.
- `/image add <path>` adds an image for the next idle text submission.
- `/image remove <index>` removes one pending image.
- `/image clear` clears pending images.
- `/detach` or `/quit` releases the controller without terminating work.
- `//text` sends a literal message beginning with `/`.

Pending images exist only in the local dashboard and are cleared on submission,
detach, stale refusal, lease replacement, or terminal failure. Images cannot be
steered into an already active turn.

## Observe and control a live turn

`shipmates live` starts a turn and prints its exact identity tuple:

```bash
shipmates live backend 'Implement the bounded parser change.'
shipmates feed backend --follow
```

Use the returned session, thread, and turn identifiers for exact control:

```bash
shipmates tell backend SESSION THREAD TURN 'Also cover malformed UTF-8.'
shipmates interrupt backend SESSION THREAD TURN
```

Old or mismatched tuples fail closed. Shipmates never redirects a stale request
to the persona's current turn.

## Inspect policy

```bash
shipmates policy validate backend
shipmates policy explain backend --command-exact 'go test ./internal/project'
```

Policy loading combines the base project policy and persona overlay into an
immutable semantic snapshot. Diagnostics are bounded and do not echo policy
secrets or unrestricted command content.

## Update and remove personas

```bash
shipmates update
shipmates update backend --accept ours
shipmates update backend --accept theirs
shipmates remove backend
shipmates remove backend --purge
```

`update` overwrites only files that still match the manifest baseline. Edited
files produce a conflict and are preserved unless an explicit resolution is
provided. `remove` deletes managed persona and policy artifacts but preserves
memory by default. `--purge` is the explicit destructive memory operation.

## Next reading

- [CLI reference](cli-reference.md)
- [Configuration and state](configuration.md)
- [Operations and Fleet](operations.md)
- [Security model](security.md)
- [Architecture](architecture.md)
