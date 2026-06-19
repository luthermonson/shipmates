# Shipmates — Architecture Diagrams

Companion to [`architecture.md`](architecture.md). Three views: system topology, the delegation + hook lifecycle, and the install/update flow.

---

## 1. System topology

How the pieces sit at runtime. The **lead session** is the only long-running `claude`; **crew** are transient subprocesses spawned per delegation; the **server** is a transient local HTTP process the lead owns. Memory is plain files on disk, one dir per persona.

```mermaid
flowchart TB
    Human(["👤 Human captain<br/>(at desk, or phone via remote-control)"])

    subgraph LeadSession["Lead session — long-running: claude --agent lead"]
        direction TB
        LeadAI["Lead persona<br/>strategy · files issues · pushes back<br/>does NOT ship code"]
        LeadMem[("memory/lead/")]
        LeadAI -. reads/writes .-> LeadMem
    end

    Server{{"Lead-spawned HTTP server<br/>127.0.0.1:PORT · transient<br/>/events /permission /feed<br/>/register /resolve /shutdown"}}

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

    Human <-->|chat| LeadAI
    LeadAI -->|"1 . spawns on first delegation"| Server
    LeadAI -->|"2 . shipmates ask / fanout"| CrewBox
    CrewBox -->|"hooks: PreToolUse, Stop,<br/>SessionStart/End, PermissionRequest"| Server
    Server -->|"GET /feed (live activity)"| LeadAI
    Server -. "PermissionRequest →<br/>approve from phone/Slack" .-> Human

    Sec -. reads/writes .-> SecMem
    Arch -. reads/writes .-> ArchMem
    Back -. reads/writes .-> OtherMem
    Front -. reads/writes .-> OtherMem
    Test -. reads/writes .-> OtherMem

    LeadAI -. "files issues" .-> Routing
    Routing -. "dispatches work" .-> CrewBox

    classDef transient stroke-dasharray: 5 5
    class Server,CrewBox transient
```

> Dashed-border boxes (server, crew) are **transient** — they exist only while work is happening. Solid = persistent (lead session, memory dirs).

---

## 2. Delegation + hook lifecycle

One `shipmates ask security "review PR 10"` from start to finish. The crew member is a one-shot subprocess that resurrects security's saved session for a single turn, streams activity to the server via hooks, and persists the new turn back to disk on exit.

```mermaid
sequenceDiagram
    actor Human
    participant Lead as Lead session
    participant Srv as Lead server<br/>(127.0.0.1:PORT)
    participant Crew as security<br/>(claude -p, transient)
    participant Mem as memory/security/

    Human->>Lead: "tell security to double-check PR 10"
    Note over Lead: server up? if not, spawn it,<br/>write server.port + server.pid
    Lead->>Srv: GET /health (wait-for-ready)
    Lead->>Crew: exec claude -p --agent security<br/>--session-id <uuid> --settings <hooks><br/>"double-check PR 10"

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
    Crew-->>Lead: final response (stdout)
    Note over Srv: ref==0 → POST /shutdown<br/>(or stays warm for next delegation)
    Lead-->>Human: summary + what security found
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

*Source: these are [Mermaid](https://mermaid.js.org) diagrams — they render on GitHub automatically and can be edited inline.*
