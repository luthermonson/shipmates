# Getting started

This is the canonical setup guide for a Linux Shipmates project.

## Requirements

- Linux. WSL is acceptable because it supplies Linux; WSL setup is outside this
  repository. Native Windows and macOS runtimes are not supported here.
- Go 1.26.5 or newer when building from source.
- Codex installed and authenticated as the same OS user that runs Shipmates.
- A Git repository containing the project.

Check the environment:

```bash
go version
codex --version
codex login status
git rev-parse --show-toplevel
```

If `codex login status` is not ready, run `codex login` and complete the
interactive authentication flow.

## Install Shipmates

For a release, download the published Linux asset from the
[latest release](https://github.com/luthermonson/shipmates/releases/latest),
place `shipmates` on `PATH`, and verify:

```bash
shipmates --version
command -v shipmates
```

To build and install this checkout instead:

```bash
./scripts/install-codex-adaptation.sh
shipmates --version
command -v shipmates
```

The installer is user-scoped and writes `~/.local/bin/shipmates`. Keep that
directory on `PATH` in interactive shells and in any service environment.
The installer may add the path to `~/.bashrc`; with `--no-shell-config`,
add it yourself. Restart the shell or export the path before continuing.

The current installer and runtime target Linux only. Do not use native
Windows/macOS build instructions from older documentation.

## Initialize a project

Run at the Git repository root:

```bash
shipmates init --crew quartermaster,skipper,backend,security,tester
shipmates list
```

Initialization creates:

- `shipmates.yaml` for project configuration;
- `.codex/agents/<persona>.toml` for canonical Codex instructions;
- `.shipmates/policy.yaml` and persona policy overlays;
- `.shipmates/memory/<persona>/` for durable project memory;
- `.shipmates/manifest.json` for managed-file ownership;
- `.shipmates/sessions/` for private continuity and local server state.

Memory is copied only when missing. Managed edits are preserved and update
conflicts require an explicit choice. Beads is not required; initialize the
external graph only when the project wants it:

```bash
shipmates beads init
```

## First delegation

Start with a bounded one-shot turn:

```bash
shipmates ask security "Review the current diff and report one actionable issue."
```

The final response is stdout; progress is stderr. Later turns resume the
persona’s Codex thread when configuration is compatible. Use `--fresh` to
start a new thread. Use `shipmates open <persona>` for interactive work,
or `live`, `feed`, `tell`, and `interrupt` for exact live-turn control.

For a whole-project outcome, use the Captain-Skipper workflow:

```bash
shipmates plan
shipmates sail --dry-run
shipmates sail
```

The Captain reviews and explicitly approves the structured plan before sailing.
Read [Sailing projects](sailing.md) for execution and optional Beads behavior.

## Generated files and state

Shipmates owns managed persona, policy, memory, manifest, continuity, and server
state beneath the paths listed above. Do not hand-edit private session state or
copy credentials into `shipmates.yaml`. `remove` preserves memory unless
`--purge` is explicitly requested.

## Troubleshooting handoffs

- **Codex unavailable:** verify `command -v codex`, `codex --version`, and
  `codex login status`; fix Codex installation/authentication first.
- **Shipmates unavailable:** verify `command -v shipmates` and `PATH`, then
  rerun the release install or checkout installer.
- **Not a project:** run from the Git repository root and confirm
  `git rev-parse --show-toplevel`.
- **Persona missing:** run `shipmates list`, then `shipmates add <persona>`.
- **Policy failure:** run `shipmates policy validate <persona>`; repair the
  reported project or persona policy before retrying.
- **Busy or stale live state:** inspect `shipmates feed <persona>`, attach with
  `shipmates open <persona>`, and do not delete locks while a child may run.
- **Update conflict:** use `shipmates update <persona> --accept ours|theirs`
  only after reviewing the managed-file diff.
- **Service issue:** run `shipmates server serve` in a foreground terminal,
  then use `shipmates server stop` from the project root.

For Fleet enrollment, credentials, TLS, and exact-turn operations, hand off to
[Operations](operations.md), [Fleet architecture](fleet-architecture.md), and
[Security](security.md).
