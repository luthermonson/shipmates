# Shipmates — Architecture Diagrams

Companion to [`architecture.md`](architecture.md). Four views: system topology, the delegation + hook lifecycle, the install/update overview, and the upgrade path in detail.

---

## 1. System topology

How the pieces sit at runtime. The **captain session** is the only long-running `claude`; **crew** are transient subprocesses spawned per delegation; the **server** is a transient local HTTP process the captain owns. Memory is plain files on disk, one dir per persona.

```mermaid
flowchart TB
    Human(["👤 Human operator<br/>(at desk, or phone via remote-control)"])

    subgraph CaptainSession["Captain session — long-running: claude --agent captain"]
        direction TB
        CaptainAI["Captain persona<br/>strategy · files issues · pushes back<br/>does NOT ship code"]
        CaptainMem[("memory/captain/")]
        CaptainAI -. reads/writes .-> CaptainMem
    end

    Server{{"Captain-spawned HTTP server<br/>127.0.0.1:PORT · transient<br/>/events /permission /feed<br/>/register /resolve /shutdown"}}

    subgraph CrewBox["Crew — transient subprocesses: claude -p (one per delegation)"]
        direction LR
        Sec["security"]
        Arch["architect"]
        Back["backend"]
        Front["frontend"]
        Test["tester"]
    end

    SecMem[("memory/<br/>security/")]
    ArchMem[("memory/<br/>architect/")]
    OtherMem[("memory/<br/>backend · frontend · tester/")]

    Routing[["Routing layer (optional, external)<br/>Code Conductor · Agent Teams · …"]]

    Human <-->|chat| CaptainAI
    CaptainAI -->|"1 . spawns on first delegation"| Server
    CaptainAI -->|"2 . shipmates ask / fanout"| CrewBox
    CrewBox -->|"hooks: PreToolUse, Stop,<br/>SessionStart/End, PermissionRequest"| Server
    Server -->|"GET /feed (live activity)"| CaptainAI
    Server -. "PermissionRequest →<br/>approve from phone/Slack" .-> Human

    Sec -. reads/writes .-> SecMem
    Arch -. reads/writes .-> ArchMem
    Back -. reads/writes .-> OtherMem
    Front -. reads/writes .-> OtherMem
    Test -. reads/writes .-> OtherMem

    CaptainAI -. "files issues" .-> Routing
    Routing -. "dispatches work" .-> CrewBox

    classDef transient stroke-dasharray: 5 5
    class Server,CrewBox transient
```

> Dashed-border boxes (server, crew) are **transient** — they exist only while work is happening. Solid = persistent (captain session, memory dirs).

---

## 2. Delegation + hook lifecycle

One `shipmates ask security "review PR 10"` from start to finish. The crew member is a one-shot subprocess that resurrects security's saved session for a single turn, streams activity to the server via hooks, and persists the new turn back to disk on exit.

```mermaid
sequenceDiagram
    actor Human
    participant Captain as Captain session
    participant Srv as Captain server<br/>(127.0.0.1:PORT)
    participant Crew as security<br/>(claude -p, transient)
    participant Mem as memory/security/

    Human->>Captain: "tell security to double-check PR 10"
    Note over Captain: server up? if not, spawn it,<br/>write server.port + server.pid
    Captain->>Srv: GET /health (wait-for-ready)
    Captain->>Crew: exec claude -p --agent security<br/>--session-id <uuid> --settings <hooks><br/>"double-check PR 10"

    Crew->>Srv: SessionStart hook → POST /register (ref++)
    Crew->>Mem: load persona + accumulated memory
    loop agentic work
        Crew->>Srv: PreToolUse / PostToolUse → POST /events
        opt tool needs approval (mode: ask)
            Crew->>Srv: PermissionRequest hook (BLOCKS)
            Srv-->>Human: forward prompt (feed / phone / Slack)
            Human-->>Srv: allow / deny
            Srv-->>Crew: {behavior: allow|deny}
        end
    end
    Crew->>Mem: write new learnings
    Crew->>Srv: SessionEnd hook → POST /deregister (ref--)
    Crew-->>Captain: final response (stdout)
    Note over Srv: ref==0 → POST /shutdown<br/>(or stays warm for next delegation)
    Captain-->>Human: summary + what security found
```

---

## 3. Install & update flow (embedded catalog)

The catalog ships **inside the Go binary** (`//go:embed`). The binary version *is* the catalog version. `shipmates update` refreshes installed files but never touches accumulated memory, and prompts with a diff on conflict.

```mermaid
flowchart TB
    Binary["shipmates binary<br/>(catalog embedded via //go:embed)"]

    subgraph Catalog["embedded catalog/"]
        direction LR
        Personas["personas/<br/>agent.md + memory-seeds/"]
        Commands["commands/<br/>standup.md …"]
        Settings["settings/<br/>hooks.json.tmpl"]
    end

    Binary --- Catalog

    Add{{"shipmates add &lt;persona&gt;"}}
    Update{{"shipmates update"}}

    Agents[".claude/agents/&lt;persona&gt;.md"]
    Cmds[".claude/commands/&lt;name&gt;.md"]
    MemDir["memory/&lt;persona&gt;/<br/>(seeded once, then sacred)"]
    Manifest[".shipmates/manifest.json<br/>(SHA of each shipped file)"]

    Personas -->|"vendor"| Agents
    Personas -->|"copy seeds ONCE (first add)"| MemDir
    Commands -->|"vendor"| Cmds
    Add --> Agents
    Add --> MemDir
    Add --> Manifest

    Update -->|"compare SHA vs manifest"| Decision{"file diverged?"}
    Decision -->|"unchanged"| Overwrite["overwrite + bump manifest"]
    Decision -->|"user-edited,<br/>catalog same"| Leave["leave alone"]
    Decision -->|"user-edited AND<br/>catalog updated"| Conflict["show unified diff<br/>keep / take / sidecar"]
    Update -. "never touches" .-> MemDir

    classDef sacred fill:#eef,stroke:#446
    class MemDir sacred
```

---

## 4. Upgrade path (end-to-end)

What [diagram #3](#3-install--update-flow-embedded-catalog) glosses over. Starts at "user dropped in a new `shipmates` binary" and ends at "manifest rewritten, summary printed." Covers the full per-file decision tree, the conflict-prompt UX (TTY *and* non-TTY), orphan flagging, and the memory dir's untouchable status — the contract being: `shipmates update` is safe to run anytime, and the worst it can do to your work is ask you a question.

Distribution of the new binary itself (`brew upgrade`, `winget upgrade`, `go install ...@latest`, raw `curl`) is out of scope — the diagram starts the moment a newer binary is on `$PATH`.

```mermaid
flowchart TB
    Bin(["distribute new binary<br/>(brew, winget, curl, go install)<br/>distribution: out of scope"])
    Cmd["shipmates update"]
    Bin --> Cmd

    Cmd --> ReadMan["read .shipmates/manifest.json<br/>last catalog ver + baseline SHA per file"]
    ReadMan --> Ver{"binary catalog ver<br/>same as manifest?"}
    Ver -->|same| Done0(["nothing to do, exit 0"])
    Ver -->|newer| Iter["for each file in manifest"]

    Iter --> Disk{"on disk?"}
    Disk -->|missing| ReAdd["restore from catalog<br/>was previously added"]
    Disk -->|present| Edit{"disk SHA same as<br/>baseline SHA?"}

    Edit -->|unedited| CatA{"catalog SHA<br/>changed?"}
    CatA -->|no| Skip1["no-op"]
    CatA -->|yes| Over["overwrite with shipped<br/>bump baseline SHA"]

    Edit -->|user-edited| CatB{"catalog SHA<br/>changed?"}
    CatB -->|no| Skip2["leave alone<br/>user customization"]
    CatB -->|yes| Conflict[/"CONFLICT<br/>render unified diff"/]

    Conflict --> TTY{"stdout is TTY?"}
    TTY -->|no| NonTTY{"--accept flag?"}
    NonTTY -->|none or ours| Keep["keep yours<br/>default in CI"]
    NonTTY -->|theirs or --force| Take["take shipped<br/>bump baseline SHA"]

    TTY -->|yes| Prompt{"k keep, t take, s sidecar, d re-diff<br/>a keep-all, T take-all (auto-apply rest)"}
    Prompt -->|k or a| Keep
    Prompt -->|t or T| Take
    Prompt -->|s| Side["write &lt;file&gt;.new sidecar<br/>baseline SHA unchanged"]
    Prompt -->|d| Conflict

    Iter -. orphan check .-> Orphan{"manifest entry no<br/>longer in catalog?"}
    Orphan -->|yes| Flag["leave on disk<br/>flag (orphaned) in shipmates list<br/>NEVER auto-delete"]

    Iter -. NEVER touched .-> Mem[("memory/&lt;persona&gt;/<br/>SACRED, user's wisdom")]

    Over --> Finish
    ReAdd --> Finish
    Take --> Finish
    Keep --> Finish
    Skip1 --> Finish
    Skip2 --> Finish
    Side --> Finish
    Flag --> Finish

    Finish["rewrite manifest.json<br/>bump catalog ver<br/>bump SHAs for files taken"] --> Summary(["summary: X updated, Y kept,<br/>Z conflicts, N orphans"])

    classDef sacred fill:#eef,stroke:#446
    classDef conflict fill:#fee,stroke:#a44
    class Mem sacred
    class Conflict conflict
```

> **Three invariants worth naming.** (1) The memory dir is never read or written by `update` — accumulated learnings survive every upgrade unconditionally. (2) Orphaned files (in your manifest, removed from a newer catalog) are flagged but never deleted — removal is always a human decision. (3) Per-file *baseline SHAs* are only bumped for files actually overwritten or taken; "keep yours" leaves the baseline alone so a later catalog update can still detect the original divergence. The manifest's overall *catalog version* bumps once per successful run regardless, so you can always tell which shipped version your project was last reconciled against.

---

*Source: these are [Mermaid](https://mermaid.js.org) diagrams — they render on GitHub automatically and can be edited inline.*
