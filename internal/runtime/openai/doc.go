// Package openai implements runtime.Runtime against the OpenAI
// chat-completions HTTP API shape, so a shipmates persona can be backed by
// any OpenAI-compatible endpoint: a vLLM / SGLang / llama.cpp / TGI / Ollama
// server, a vendor gateway, or an enterprise inferencing cloud in front of a
// strong open model.
//
// It is the only runtime that needs neither a vendor CLI installed on the box
// nor a public API. "Works against an arbitrary OpenAI-compatible base URL"
// is the requirement everything else here bends to: the request body is the
// smallest thing that any compatible server accepts, and the response parser
// tolerates the shapes real servers actually emit (null content, content
// arrays, reasoning_content, heartbeat comments, servers that ignore
// stream:true and answer with one JSON object).
//
// # A model is not an agent
//
// Claude Code and Codex are agents: they run tools, edit files, and prompt
// for permission themselves, and shipmates supervises them. An
// OpenAI-compatible endpoint is a model. Nothing executes a tool call unless
// shipmates executes it.
//
// This runtime therefore does exactly one thing: text turns, streamed. Prompt
// in, assistant tokens out as runtime.Events, one turn boundary at the end.
// No file tools, no shell tools, no tool-call loop. See toolloop_seam.go for
// what a future tool loop would need and why it is not here.
//
// # What it does
//
//   - StartSession builds a system prompt from the installed persona file and
//     the persona's bounded memory digest (see persona.go), then keeps the
//     transcript in memory — the API is stateless, so the transcript is ours.
//   - SendTurn appends the user message, streams one chat-completions
//     request, emits TextDelta events, and always closes with a TurnDone.
//   - InterruptTurn cancels the in-flight HTTP request through its context.
//     That is a real interrupt, not a bookkeeping lie: the connection drops.
//   - Everything not implemented returns *runtime.ErrUnsupported.
//
// # Wiring
//
// The factory-facing entrypoints are [ParseConfig], [New], and
// [NewFromSettings]. NewFromSettings has the shape a runtime.Factory wants:
//
//	rt, err := openai.NewFromSettings(ctx, settings) // settings map[string]any
//
// Configuration keys are documented on [Config]. The API key is never in
// config — only the *name* of the environment variable holding it
// (api_key_env), and endpoints that need no auth are supported by leaving it
// unset.
//
// # Bounds
//
// Every input and output is bounded, because the endpoint is not trusted to
// be well-behaved: total response bytes, per-SSE-line bytes, malformed-chunk
// count, prompt bytes, system-prompt bytes, transcript bytes, and transcript
// message count. Defaults are the Default* constants in config.go and each is
// overridable per deployment.
//
// # Secrets
//
// The API key is read from the environment at request time, written to
// exactly one place (the Authorization header of the outbound request), and
// never stored on Config, never put in an error, and never put in an event.
// Anything derived from the server's response — error bodies, deltas — is run
// through [scrub] first, because a misbehaving endpoint can echo the
// credential back at us. TestAPIKeyNeverLeaks covers all of these paths.
//
// # Transport
//
// TLS verification is never weakened. There is no insecure_skip_verify
// option and config parsing rejects one on sight with an explanatory error. A
// private CA is supplied as a PEM file (ca_file) that is appended to the
// system trust store. Plaintext http is allowed to loopback (self-hosted
// servers on the box), requires an explicit allow_plaintext_http opt-in for
// any other host, and is refused outright when an API key is configured —
// a bearer token does not go over cleartext.
package openai
