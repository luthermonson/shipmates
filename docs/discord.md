# Discord transport (EXPERIMENTAL)

> **Status: experimental.** Each mate speaks as its own Discord bot — inbound and
> outbound — with an optional shared all-hands channel. It ships as the
> `shipmates discord` command. The live path (real bots in a real server talking
> to a running ship) has not been smoke-tested here — see
> [Live smoke-test](#live-smoke-test) — and inbound @mention routing from the
> all-hands channel is not implemented yet (all-hands is **post-only**). Do not
> point this at a busy production server.

## What it does

`internal/discord` bridges Discord channels to the mates on this machine's ship.
Each mate is one Discord bot bound to one channel:

- **Inbound** — a message in a mate's channel, from an allowlisted Discord user,
  becomes the *content* of a `tell` to that persona, dispatched to the ship's
  local captain server via the authenticated `POST /tell/{persona}` seam.
- **Outbound** — each transport polls the captain's `GET /events` and posts its
  persona's assistant replies back to that mate's channel as its own bot.
- **All-hands (optional)** — when `allHandsChannel` is set, every mate *also*
  posts its outbound replies to that shared channel, as its own bot (so
  identities stay distinct). This is **post-only** for now: messages typed in the
  all-hands channel are not routed to any mate — command a mate in its own
  channel.

It reuses the same tell/events seam the voice conversation loop uses
(`internal/fleet/conversation.go`), through `internal/client` (loopback +
`Authorization: Bearer <server.token>`). No new tell/events plumbing is
introduced; Discord is just another front-end onto the existing loop.

## Two ways to configure it

- **Multi-bot mode** — an operator config file `~/.shipmates/discord.yaml`,
  defining one bot per mate plus an optional all-hands channel. This is the
  feature's main path.
- **Single-bot mode** — four environment variables, no file. Used automatically
  when `~/.shipmates/discord.yaml` is absent. This is the quickstart, and the
  simplest way to try one bot.

In both, **a bot token is never stored in config**: the file/env names the
*environment variable* that holds the token (exactly like the openai runtime's
`api_key_env` and `~/.shipmates/personas.yaml`). The token is read at startup and
never logged.

## Multi-bot: `~/.shipmates/discord.yaml`

The file lives in the operator's home, outside every repo checkout — the same
trust boundary as `~/.shipmates/personas.yaml`.

```yaml
allowedUsers: ["<discord-user-id>", ...]   # who may command ANY mate; fail-closed
allHandsChannel: "<channel-id>"            # optional shared channel; omit to disable
mates:
  architect:
    tokenEnv: DISCORD_TOKEN_ARCHITECT      # NAME of the env var holding the token
    channel: "<channel-id>"
  security:
    tokenEnv: DISCORD_TOKEN_SECURITY
    channel: "<channel-id>"
```

- `allowedUsers` — the shared allowlist for every mate. **Fail-closed:** an empty
  list means nobody can command any mate (it is refused at startup as a
  misconfiguration). There is no wildcard.
- `allHandsChannel` — omit it to disable the all-hands mirror.
- `mates` — keys are persona names (must match `.claude/agents/<persona>.md`).
  Each mate names its own `tokenEnv` and `channel`.

**One Discord application per mate.** Discord identity is per bot, so each mate
needs its own application/bot and its own token:

1. For each mate, create an application (Developer Portal → **New Application**),
   open the **Bot** tab, enable **MESSAGE CONTENT INTENT** under *Privileged
   Gateway Intents*, and **Reset Token** to copy its token.
2. Export each token into the env var you named in `tokenEnv`, e.g.:

   ```
   export DISCORD_TOKEN_ARCHITECT='<architect bot token>'
   export DISCORD_TOKEN_SECURITY='<security bot token>'
   ```

3. Invite each bot to your server (OAuth2 → URL Generator → scope `bot`,
   permissions **View Channel**, **Send Messages**, **Read Message History**),
   and create one channel per mate (plus one all-hands channel if you want the
   mirror).
4. With Developer Mode on (User Settings → Advanced), right-click each channel →
   **Copy Channel ID**, and right-click your own name → **Copy User ID** for
   `allowedUsers`.

A mate whose `tokenEnv` variable is unset (or whose persona/channel is invalid)
is **skipped with a warning naming the mate**; the healthy mates still start. The
whole command aborts only if the file is unparseable, `allowedUsers` is empty,
`mates` is empty, or *every* mate fails preflight.

## Single-bot: environment variables

Used automatically when `~/.shipmates/discord.yaml` does not exist.

| Variable                          | Meaning                                                          |
| --------------------------------- | --------------------------------------------------------------- |
| `DISCORD_BOT_TOKEN`               | The bot token. **Secret.** Never logged, never in the repo.     |
| `DISCORD_TRAINING_CHANNEL`        | The single channel id to listen on and post to.                 |
| `SHIPMATES_DISCORD_ALLOWED_USERS` | Comma-separated Discord user ids permitted to command the mate. |
| `SHIPMATES_DISCORD_PERSONA`       | The persona a tell is addressed to (e.g. `captain`).            |

Set up the one bot exactly as in step 1–4 above (MESSAGE CONTENT INTENT, invite,
channel id, your user id).

## Running it

The ship server address is discovered automatically: loopback + the port from
`.shipmates/sessions/server.port`, using the per-run captain bearer token from
`.shipmates/sessions/server.token` — the same files `internal/client` reads. So
run from inside the ship's repo directory, with a captain already running:

```
shipmates discord
```

It resolves config (file if present, else env), validates it — failing fast with
a message that names any missing/invalid variable, and **never printing a token
value** — then connects one bot per mate and runs until interrupted. Ctrl-C
(SIGINT) or SIGTERM cancels the shared run context; every transport shuts down
and the command waits for all of them before exiting. Add `--verbose` on the root
command (`shipmates --verbose discord`) to see connect/dispatch logs; no token
appears in them.

## Live smoke-test

The pure logic (allowlist, sanitization, config-file parsing, token-env
resolution) is unit-tested, but the live wire — real bots talking to a running
ship — has not been exercised here. Minimal multi-bot steps:

1. **Create two bots**, one per mate. For each: Developer Portal → new
   Application → **Bot** → enable **MESSAGE CONTENT INTENT** (without it message
   bodies arrive empty and inbound silently does nothing) → **Reset Token** and
   copy it.
2. **Invite both** (OAuth2 URL Generator → scope `bot`, permissions **View
   Channel / Send Messages / Read Message History**) to a server you control.
   Create one channel per mate and, optionally, one all-hands channel.
3. **Collect ids** (Developer Mode on): each channel id, and your own user id.
4. **Start a ship** with those personas at work so `.shipmates/sessions/server.*`
   exist and `GET /events` + `POST /tell/{persona}` are live — e.g.
   `shipmates tell architect "hello"` and `shipmates tell security "hello"`
   (or `shipmates open`).
5. **Write `~/.shipmates/discord.yaml`** using the schema above, with the two
   personas, their channel ids, their `tokenEnv` names, your user id in
   `allowedUsers`, and an `allHandsChannel`.
6. **Export the token env vars** named by each `tokenEnv`
   (`DISCORD_TOKEN_ARCHITECT`, `DISCORD_TOKEN_SECURITY`, …).
7. **Run from the repo root:** `shipmates --verbose discord`. Confirm it logs
   `mode=file mates=2` and that no token value appears in any log line.
8. **Inbound:** from the allowlisted account, post in the *architect* channel —
   it should reach the architect mate as a tell; post in the *security* channel —
   it should reach security. A message from a non-allowlisted account must be
   ignored in both.
9. **Outbound + identity:** each mate's reply posts back to its own channel as
   its own bot, and also appears in the all-hands channel under that same bot's
   identity. Confirm a reply containing `@everyone` or `<@id>` posts without
   pinging anyone.
10. **Degraded start:** unset one mate's token env and re-run — that mate is
    skipped with a warning, the other still starts.
11. **Shutdown:** Ctrl-C; the process exits cleanly (no error, no stack) after
    all bots disconnect.

(For a one-bot check, skip the file and use the four single-bot env vars instead;
everything else is the same.)

This feature stays marked experimental until steps 7–11 are confirmed against
live bots.

## Security model

The properties the transport enforces, and where — all re-verified in the
multi-bot path:

1. **Tokens are operator-only and never logged.** A token is read only from the
   env var named by `tokenEnv` (multi-bot) or `DISCORD_BOT_TOKEN` (single-bot),
   at the moment of use (`readToken`, `internal/discord/config.go`). `Config`
   holds the env-var *name* (`TokenEnv`), never the value; `MateError` carries
   only the name too, so a per-mate failure names the missing var without ever
   echoing a token. This mirrors the openai runtime's `api_key_env` posture
   (`internal/runtime/openai/config.go`).

2. **Inbound Discord text is hostile input — data, not instructions.**
   `sanitizeInbound` (`internal/discord/sanitize.go`) only trims and hard-bounds
   the message (`MaxInboundRunes`); it never interprets the text, which becomes
   the *content* of a tell and nothing more. Same stance as
   `catalog/routing/github.md`.

3. **The allowlist is fail-closed and shared.** `allowedUsers` builds one
   `Allowlist` (`ParseAllowlistSlice`) shared by every mate; `Allowlist.Allows`
   (`internal/discord/allowlist.go`) returns true only for ids explicitly
   present. Empty allows nobody (and is refused at startup), there is no
   wildcard, and a non-allowlisted message is dropped in `onMessageCreate`
   (`internal/discord/transport.go`).

4. **Outbound mentions are neutralized, twice, and escapes scrubbed — on every
   post, all-hands included.** `stripMentions` neutralizes `@everyone`, `@here`,
   `<@userid>`, and `<@&roleid>`, and `sanitizeOutbound` additionally runs the
   text through the terminal-escape scrubber `bridge.Chrome`
   (`internal/discord/sanitize.go`). Every post — to the mate's own channel and
   to the all-hands mirror — goes through `postTo`
   (`internal/discord/transport.go`), which sets discordgo's `AllowedMentions` to
   an empty parse set, so Discord parses no mentions of any kind.

5. **The captain API is always authenticated.** Both `POST /tell/{persona}` and
   `GET /events` go through `internal/client`, which attaches
   `Authorization: Bearer <server.token>` on every request. No transport talks to
   the captain API unauthenticated.
