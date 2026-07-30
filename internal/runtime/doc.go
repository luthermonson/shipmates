// Package runtime declares the AI-runtime abstraction shipmates uses to talk
// to Claude Code, Codex and OpenAI-compatible endpoints through one seam.
//
// # Layout
//
//	runtime              this package: the Runtime interface, Caps, events,
//	                     the Selector seam, and VerifyCaps
//	runtime/claude       the `claude` CLI over stdio streaming
//	runtime/codex        the codex app-server transport
//	runtime/openai       any OpenAI-compatible chat-completions endpoint
//	runtime/config       which runtime is selected, and the trust boundary
//	                     between project config and user config
//	runtime/containment  bounding and tearing down spawned processes
//	runtime/factory      resolved config + directories -> a live Runtime
//	runtime/env          the production Selector commands call
//
// # State of the migration
//
// The selection layer is complete and wired: `--runtime` (and
// SHIPMATES_RUNTIME) resolve through internal/runtime/env on every command
// whose behavior depends on the runtime, the default resolves to claude, and a
// project checkout can select a runtime but cannot influence what it executes.
//
// What is NOT yet routed through this interface is the launch path itself.
// The commands that spawn a session still exec the `claude` CLI directly with
// Claude Code's own flags, and internal/server drives it over Claude's
// stream-json protocol. Those call sites are gated on the resolved runtime, so
// selecting codex or openai reports that the launch path cannot serve it rather
// than quietly running claude anyway. See docs/runtime-interface.md for what
// the remaining step needs.
package runtime
