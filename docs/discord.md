# Discord transport (EXPERIMENTAL)

> **Status: experimental.** This is a vertical slice that proves one loop —
> one mate ↔ one Discord bot ↔ one channel, inbound and outbound. It ships as
> the `shipmates discord` command, but the live path (a real bot in a real
> server talking to a running ship) has not been smoke-tested here, and there is
> no multi-persona routing yet — see [Live smoke-test](#live-smoke-test). Do not
> point this at a busy production server.

## What it does

`internal/discord` bridges a single Discord text channel to a single mate:

- **Inbound** — a message in the configured channel, from an allowlisted Discord
  user, becomes the *content* of a `tell` to the configured persona. It is
  dispatched to the ship's local captain server via the existing authenticated
  `POST /tell/{persona}` seam.
- **Outbound** — the transport polls the captain's `GET /events` and posts the
  configured persona's assistant replies back to the channel as the bot.

It reuses the same tell/events seam the voice conversation loop uses
(`internal/fleet/conversation.go`), through `internal/client` (loopback +
`Authorization: Bearer <server.token>`). No new tell/events plumbing is
introduced; Discord is just a second front-end onto the existing loop.

## Setup

### 1. Create a bot and get its token

1. Go to the Discord Developer Portal → **Applications** → **New Application**.
2. Open the **Bot** tab → **Add Bot**.
3. Under **Privileged Gateway Intents**, enable **MESSAGE CONTENT INTENT**
   (required to read message text) and **SERVER MEMBERS**/**PRESENCE** are not
   needed.
4. **Reset Token** and copy it. This is a secret — it goes in an environment
   variable, never into the repo or any config file (see [Security model](#security-model)).

### 2. Invite the bot to your server

Build an OAuth2 invite URL (Developer Portal → **OAuth2** → **URL Generator**):

- Scopes: `bot`
- Bot permissions: **View Channel**, **Send Messages**, **Read Message History**

Open the URL and add the bot to a server you control. Then create (or pick) the
single channel it will use.

### 3. Get the channel id

Enable **Developer Mode** in Discord (User Settings → Advanced), right-click the
channel → **Copy Channel ID**.

### 4. Get your Discord user id(s) for the allowlist

Right-click your username → **Copy User ID**. These are the only users allowed to
command the mate. Anyone not listed is read-only.

### 5. Environment variables

All configuration is operator-owned and read from the environment — nothing lives
in the checkout.

| Variable                          | Meaning                                                           |
| --------------------------------- | ---------------------------------------------------------------- |
| `DISCORD_BOT_TOKEN`               | The bot token. **Secret.** Never logged, never in the repo.      |
| `DISCORD_TRAINING_CHANNEL`        | The single channel id to listen on and post to.                  |
| `SHIPMATES_DISCORD_ALLOWED_USERS` | Comma-separated Discord user ids permitted to command the mate.  |
| `SHIPMATES_DISCORD_PERSONA`       | The persona a tell is addressed to (e.g. `captain`).             |

The ship server address is discovered automatically: loopback + the port from
`.shipmates/sessions/server.port`, using the per-run captain bearer token from
`.shipmates/sessions/server.token` — the same files `internal/client` reads.

## Running it

Run the transport from inside the ship's repo directory (so
`.shipmates/sessions/*` resolves), with a captain already running:

```
shipmates discord
```

It reads all four env vars, validates them (failing fast with a message that
names any missing/invalid variable — the token value is never printed), connects
to Discord, and runs until interrupted. Ctrl-C (SIGINT) or SIGTERM cancels the
run context and shuts down cleanly.

## Live smoke-test

The pure logic (allowlist, sanitization, config parsing) is unit-tested, but the
live wire — a real bot in a real server talking to a running ship — has not been
exercised here. Minimal steps to verify it:

1. **Create the bot and enable the intent.** Discord Developer Portal → new
   Application → **Bot** tab → **Reset Token** and copy it. Under **Privileged
   Gateway Intents**, enable **MESSAGE CONTENT INTENT** (without it the bot
   receives empty message bodies and inbound silently does nothing).
2. **Invite it.** OAuth2 URL Generator → scope `bot`, permissions **View
   Channel**, **Send Messages**, **Read Message History**. Open the URL and add
   the bot to a server you control; pick or create one channel for it.
3. **Collect ids** (Discord Developer Mode on): right-click the channel → **Copy
   Channel ID**; right-click your own name → **Copy User ID**.
4. **Start a ship** in the repo so `.shipmates/sessions/server.port` /
   `server.token` exist and `GET /events` + `POST /tell/{persona}` are live —
   e.g. `shipmates tell <persona> "hello"` (or `shipmates open`) puts a mate to
   work and spawns the coordination server.
5. **Export the env vars** (use the persona you just put to work):

   ```
   export DISCORD_BOT_TOKEN='<bot token>'
   export DISCORD_TRAINING_CHANNEL='<channel id>'
   export SHIPMATES_DISCORD_ALLOWED_USERS='<your user id>'
   export SHIPMATES_DISCORD_PERSONA='<persona>'
   ```

6. **Run it from the repo root:** `shipmates discord` (add `--verbose` on the
   root command — `shipmates --verbose discord` — to see connect/dispatch logs;
   the token is never among them).
7. **Inbound:** from the allowlisted account, post a message in the channel. It
   should arrive at the mate as a tell. A message from any other account must be
   ignored.
8. **Outbound:** the persona's next assistant reply should post back into the
   channel as the bot. Confirm a reply containing `@everyone` or a `<@id>`
   mention posts without pinging anyone.
9. **Shutdown:** Ctrl-C; the process should exit cleanly (no error, no stack).

This feature stays marked experimental until steps 7–9 are confirmed against a
live bot.

## Security model

The five constraints this spike enforces, and where:

1. **Token is operator-only and never logged.** The bot token is read from
   `DISCORD_BOT_TOKEN` only, at the moment of use (`tokenFromEnv`,
   `internal/discord/config.go`), and is never stored on `Config`, never
   returned, and never placed in a log line or error message. This mirrors the
   openai runtime's `api_key_env` posture (`internal/runtime/openai/config.go`):
   a secret is named by an env var, never read from config-in-repo.

2. **Inbound Discord text is hostile input — data, not instructions.**
   `sanitizeInbound` (`internal/discord/sanitize.go`) only trims and hard-bounds
   the message (`MaxInboundRunes`). It never interprets the text; the message
   becomes the *content* of a tell and nothing more, so it cannot smuggle a
   command. This is the same stance `catalog/routing/github.md` takes toward
   GitHub-sourced text.

3. **The allowlist is fail-closed.** `Allowlist.Allows`
   (`internal/discord/allowlist.go`) returns true only for ids explicitly
   present. An empty allowlist allows nobody (not everybody), there is no
   wildcard, and a non-allowlisted message is dropped (read-only) in
   `onMessageCreate` (`internal/discord/transport.go`).

4. **Outbound mentions are neutralized, twice, and escapes scrubbed.**
   `stripMentions` (`internal/discord/sanitize.go`) neutralizes `@everyone`,
   `@here`, `<@userid>`, and `<@&roleid>` before posting, and `sanitizeOutbound`
   additionally runs the text through the terminal-escape scrubber
   `bridge.Chrome`. As a second layer, every post sets discordgo's
   `AllowedMentions` to an empty parse set (`postToChannel`,
   `internal/discord/transport.go`), so Discord parses no mentions of any kind.

5. **The captain API is always authenticated.** Both `POST /tell/{persona}` and
   `GET /events` go through `internal/client`, which attaches
   `Authorization: Bearer <server.token>` on every request. The transport never
   talks to the captain API unauthenticated.
```
