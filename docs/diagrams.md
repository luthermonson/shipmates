# Runtime diagrams

These diagrams describe only the shipped Codex-native runtime. Source Mermaid
files live in [`docs/diagrams/`](diagrams/).

## Local delegation and control

```mermaid
sequenceDiagram
    actor Operator
    participant CLI as Shipmates CLI
    participant Codex as Codex runtime
    participant Local as Local loopback server
    participant Memory as Project memory
    Operator->>CLI: ask persona prompt
    CLI->>Codex: managed Codex turn boundary
    Codex->>Memory: read persona context and memory
    Codex-->>CLI: normalized events and final response
    CLI-->>Operator: response
    Operator->>CLI: open persona
    CLI->>Local: attach controller lease
    Local->>Codex: app-server turn/control
    Codex-->>Local: bounded events or approval request
    Local-->>CLI: normalized feed / approval card
    CLI->>Local: message, interrupt, or one-request decision
```

## Installed state

```mermaid
flowchart LR
    Catalog[Embedded catalog] --> Add[add / update]
    Add --> Persona[.codex/agents/persona.toml]
    Add --> Policy[.shipmates/policies/persona.yaml]
    Add --> Memory[.shipmates/memory/persona]
    Add --> Manifest[.shipmates/manifest.json]
    Update[shipmates update] --> Manifest
    Manifest --> Decision{managed file changed?}
    Decision -->|no| Replace[refresh managed file]
    Decision -->|yes| Explicit[keep, take, or sidecar]
    Update -. never overwrites learned state .-> Memory
```

## Fleet observation and exact-turn operations

```mermaid
flowchart LR
    Project[Project local server] -->|bounded projection| Tunnel[Outbound authenticated tunnel]
    Tunnel --> Observer[Read-only Fleet observer]
    Observer --> UI[Accessible observer UI]
    Steer[Scoped steer capability] -->|one exact active turn| Tunnel
    Interrupt[Scoped interrupt capability] -->|one exact active turn| Tunnel
    Tunnel --> Audit[Sanitized immutable audit]
```

## Upgrade and explicit legacy migration

Normal `update` operates only on the current managed manifest. A manifest-v1
project must invoke `migrate codex --plan` and then explicitly choose `--apply`.
Modified and untracked legacy data is preserved and is never executed.
