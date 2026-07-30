package openai

// This file is the seam where a model-directed tool loop would attach, and a
// record of why there isn't one. It contains no code on purpose.
//
// # What is missing today
//
// Claude Code and Codex are agents. They decide to run a command, run it, and
// ask a human when their own rules say to; shipmates watches. An
// OpenAI-compatible endpoint is a model: it can emit a `tool_calls` array, but
// nothing happens next unless shipmates executes it. Today shipmates has never
// executed a model-directed tool call — every exec.Command in the tree is
// shipmates running its own subprocess (git, the claude CLI, a PTY), not a
// model's instruction.
//
// So adding a tool loop here is not "wire up two more request fields". It is
// the first place in this codebase where a remote model's output would become
// a local side effect. Three things would have to exist first, and none of them
// do.
//
// # 1. A policy vocabulary that can describe the request
//
// internal/permissions is modelled on Claude Code's settings-driven
// permission system: Evaluator.EvaluateFor(persona, tool, input) switches on
// tool name — Bash/PowerShell, Read/Edit/Write/MultiEdit/NotebookEdit,
// WebFetch/WebSearch — and matches patterns an operator wrote in
// .claude/settings.json. Two properties matter here:
//
//   - Its default for anything it does not recognise is ALLOW
//     ("default-allow: non-gated tool …", and there is a test pinning that
//     behaviour). A new tool name invented by this runtime would sail through
//     ungated on a project that never wrote a rule for it.
//   - It has no notion of a *scope* the runtime must then enforce. "Which
//     files may this session write" and "which hosts may it reach" are
//     expressible as patterns only if the operator happened to write them;
//     there is no deny-by-default file or network schema, and no place a
//     runtime declares the effective sandbox it is honouring.
//
// A tool loop needs the inverse posture: deny by default, with explicit
// request kinds (file read, file write, path scope, network host, process
// exec) carried through ApprovalResponse.PolicyContext, and an evaluator that
// returns "unsupported request kind → deny" rather than "unknown tool →
// allow". Building the loop against today's engine would mean a persona
// configured with capability "edit" gets unbounded filesystem write on a
// project whose settings.json never anticipated a non-Claude runtime. That is
// the single most dangerous change available in this repository, which is why
// this file is empty.
//
// # 2. Tool schemas derived from the persona capability vocabulary
//
// runtime.PersonaSpec.Capabilities is the canonical list: "read", "edit",
// "bash", "browse". Those four names — and nothing broader — would map to JSON
// Schema tool definitions sent in the request's `tools` field, so a persona
// that declares only "read" cannot be handed an edit tool by an endpoint that
// asks for one. The mapping belongs next to InstallPersona (persona.go), and
// the installed persona file already records declaredCapabilities so the
// generated schema set is auditable on disk instead of implicit in a prompt.
//
// The loop itself is the standard shape: send with tools, receive
// finish_reason "tool_calls", emit runtime.KindToolCall, gate, execute, append
// a role:"tool" message with tool_call_id, resend. Each step is easy. Each
// step is also a place to leak: tool_call arguments are attacker-influenced
// text (they come from a model that read repository content), argument JSON
// must be validated against the schema rather than trusted, path arguments
// must be resolved and confined to the project root *after* symlink
// resolution, and a bounded iteration count is mandatory or a model can loop
// the runtime forever.
//
// # 3. Execution under the existing containment watcher
//
// runtime.Caps.Containment and internal/runtime/containment already describe
// process containment: the operator's posture is resolved once from user
// config and baked into a runtime at construction, and a runtime that reports
// Containment true routes its spawns through that watcher. Any "bash"
// capability here would have to execute through the same watcher — not os/exec
// directly — so the resource limits and process-tree teardown an operator
// chose apply to a command a remote model asked for. Where containment is
// unavailable, the honest behaviour is to refuse the capability, not to run the
// command unconfined; this runtime reports Containment false today precisely
// because it spawns nothing.
//
// # What this runtime does instead
//
// Text turns. A crew member you can `ask`, whose answers land in a room, with
// a real interrupt and honest Caps. Everything above stays unbuilt until the
// policy engine can express what it would be permitting.
