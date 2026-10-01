# Slash Commands & Apps

How OneCamp's slash-command and app platform works, and — most importantly —
**how to add a new app without touching backend code**.

This model closely mirrors Slack's slash-command architecture: a request URL, a
signing secret, an immediate ACK, and an async Block Kit response.

---

## TL;DR

- **Adding a normal app requires NO backend code and NO redeploy.** An admin
  installs an app (handler URL + signing secret) from the admin UI; the app is
  just data in Postgres.
- **Built-in handlers** (`giphy`, `poll`, `remind`, `/dm`, `/away`, …) are
  first-party commands that ship *with* the product and run in-process. Adding
  one of these *does* require Go code + a deploy. Use this path sparingly.
- When in doubt, build an **external app**, not a built-in.

---

## The two execution paths

Every command resolves (in `business/Command/dispatcher.go → Execute`) to an
`ExecMode` stored on its catalog row. There are two worlds:

### 1. External apps — the default, data-driven path

`ExecMode = ExecExternal`

This is how the overwhelming majority of apps should be added. The flow:

1. An admin installs an app row with a `HandlerUrl` and a `SigningSecret`
   (via the admin Apps UI, or seeded in `marketplace.go` for one-click installs).
2. A user runs the command. OneCamp **ACKs instantly** with an ephemeral
   "Working on `/cmd`…" so the composer feels snappy.
3. `dispatchExternal → forwardToApp` POSTs a signed, SSRF-guarded JSON payload
   to the app's `HandlerUrl`.
4. The app replies with a `CommandResponse` (Block Kit JSON). That reply is
   delivered back to the invoker **asynchronously over MQTT**.
5. Button/select clicks on a card flow through `dispatchExternalInteraction`
   the same way (payload `type: "block_action"`).

**Adding an external app = inserting a row. Zero Go code, zero redeploy.**

This is the Slack-equivalent model:

| Slack                | OneCamp                                  |
|----------------------|------------------------------------------|
| Request URL          | `HandlerUrl`                             |
| Signing Secret       | `SigningSecret` (HMAC v1, see `signV1`)  |
| `response_url`       | async MQTT delivery (`deliverAsync`)     |
| Block Kit            | `commandAdapter.Block` / `CommandResponse` |

#### Request signing

Outbound calls are signed with HMAC-SHA256 over `v1:{timestamp}:{body}` and
sent as:

- `X-OneCamp-Timestamp: {unix}`
- `X-OneCamp-Signature: v1={hex}`

Apps must verify this signature (constant-time compare) and reject stale
timestamps. All outbound URLs are validated against SSRF via
`helpers.ValidateOutboundURL` + `helpers.SSRFSafeClient`.

#### Request payload (command)

```json
{
  "command": "/jira",
  "text": "create login is broken",
  "user_id": "…uuid…",
  "user_name": "akash",
  "trigger_id": "…",
  "timezone": "Asia/Kolkata",
  "channel_id": "…uuid…",     // present in a channel
  "dm_group_id": "…"          // present in a DM/group
}
```

#### Response payload

Return a `CommandResponse`. Minimal example:

```json
{
  "response_type": "ephemeral",
  "text": "Created JIRA-123",
  "blocks": [ /* optional Block Kit */ ]
}
```

`response_type` is `ephemeral` (only the invoker sees it) or `in_channel`
(posted to the channel). If omitted it defaults to ephemeral.

---

### 2. Built-in handlers — in-process, requires code

`ExecMode = ExecInline | ExecDeferred | ExecInteractive`

Built-ins register a Go handler in `commandRegistry` (and optionally
`interactionRegistry`) from an `init()` in `business/Command/`. They run
in-process — no external server, no network hop per interaction.

Files: `builtins.go`, `poll.go`, `reminder.go`, `giphy.go`.

The exec modes:

- **`ExecInline`** — runs synchronously, returns immediately (`/dm`, `/me`,
  `/away`, `/status`, `/search`). Most return a **client action** the FE
  executes (e.g. `set_presence`, `open_dm`, `post_message`).
- **`ExecDeferred`** — schedules work for later (`/remind`); the result is
  pushed over MQTT when it fires.
- **`ExecInteractive`** — renders a Block Kit card with buttons and handles
  clicks in-process via a registered interaction handler (`/poll`, `/giphy`).

#### Why are Giphy / polls built-in rather than external?

The same reason Slack ships Giphy as a first-party integration rather than a
generic webhook app — **UX, cost, and trust**. A built-in handler:

- needs no external server to host or pay for (a GIF picker would otherwise
  require an always-on service),
- gives instant in-process Shuffle/Send with atomic Redis state and no network
  round-trip per click,
- keeps secrets (e.g. the Giphy API key) inside OneCamp's encrypted secret bag
  rather than handed to a third party.

Note that Giphy is still *installed as an app* (its API key lives in the
installed `giphy` app's encrypted secret bag under `api_key`); only its command
execution happens in-process. It's a first-party app with an in-process handler,
not a hardcoded feature.

#### App kinds

An installed app row has a `kind`:

- **`builtin`** — first-party, in-process (Giphy, OneCamp AI). No handler URL or
  signing secret; its commands are seeded org-scoped built-ins (not app-linked
  rows), and the admin editor hides the handler/signing fields and shows the
  commands read-only. May still hold a secret (e.g. the Giphy API key).
- **`external`** — third-party webhook app (handler URL + signing secret).
- **`oauth`** — external app that also stores a per-workspace OAuth token.

> ⚠️ **Built-in commands must NOT also be created as app-linked command rows.**
> They already exist as seeded org-scoped built-ins, and the unique index on
> `(command, scope_type, scope_entity_id)` will reject the duplicate — which
> silently leaves the app showing "No commands yet". `CreateApp`/`UpdateApp`
> skip command sync for `kind = builtin`, and `buildAppViewFrom` surfaces a
> built-in app's commands from its marketplace template instead.

#### ⚠️ When NOT to add a built-in

Do **not** reach for a built-in just because dropping a `.go` file in
`business/Command/` is easy. If the app is essentially "call an API and render a
card," it belongs on the **external path** so it stays data-driven (no deploy
per app). Reserve built-ins for first-party commands that genuinely need
in-process execution, zero external hosting, or secret custody.

---

## How to add an app

### A. External app (recommended — no code)

1. Open the admin **Apps** UI.
2. Create an app: name, icon, `HandlerUrl`, `SigningSecret`, and one or more
   commands (each with `command`, `description`, `usage_hint`,
   `exec_mode = external`, `response_type`).
3. Install it. The catalog version is bumped (`bumpCatalogVersion`) so every
   user's cached typeahead picks up the new command within seconds.
4. Your handler service verifies the signature and replies with a
   `CommandResponse`.

To make an app **one-click installable** from the marketplace, add an entry to
the catalog in `business/Command/marketplace.go` (this is data, not logic — it
defines the listing, its commands, and its setup fields).

### B. Built-in command (first-party, requires deploy)

1. Create a handler in `business/Command/` and register it in an `init()`:

   ```go
   func init() {
       Register("mycmd", handleMyCmd)
       // if interactive:
       RegisterInteraction("mycmd", handleMyCmdInteract)
   }
   ```

2. Add a catalog row so it shows in the typeahead. For pure built-ins this is
   the `builtinCatalog` list in `business/Command/seed.go` (keep it in sync with
   `Register()`); `SeedBuiltinCommands` seeds it at startup.

3. Build, test, deploy:

   ```bash
   gofmt -w business/Command/<file>.go
   go build ./business/Command/...
   go test ./business/Command/...
   ```

---

## Catalog, scoping & caching

- `GetCatalog` returns the scope-filtered command list for a user (team- and
  channel-scoped commands are only visible to members), cached in Redis so the
  composer typeahead never hits Postgres on the hot path.
- The cache key embeds a workspace-wide **catalog version**. Any admin
  install/toggle/remove calls `bumpCatalogVersion`, invalidating every user's
  cache at once. There's also a short TTL for eventual consistency.

## Rate limiting

Per-user fixed-window limit (`commandRateLimit = 60/min`) enforced in
`checkRate` for both execution and interactions.

## Security checklist (external apps)

- ✅ Verify the `v1=` HMAC signature (constant-time) on every inbound request.
- ✅ Reject requests with a stale `X-OneCamp-Timestamp`.
- ✅ OneCamp validates your `HandlerUrl` against SSRF before every call.
- ✅ Secrets are encrypted at rest (AES-256-GCM) and never returned to the FE —
  only `has_*` booleans are exposed.

---

## File map

| File | Responsibility |
|------|----------------|
| `dispatcher.go` | `Execute`/`HandleInteract`, external dispatch, signing |
| `commandBusiness.go` | `Register` / `RegisterInteraction`, registries |
| `builtins.go` | inline built-ins (`/dm`, `/away`, `/status`, …) |
| `poll.go`, `reminder.go`, `giphy.go` | interactive/deferred built-ins |
| `seed.go` | built-in catalog seeding (`builtinCatalog`) |
| `marketplace.go` | one-click marketplace app catalog (data) |
| `appBusiness.go`, `appOAuth.go` | app install / secrets / OAuth |
| `delivery.go` | async MQTT delivery + FCM push |
