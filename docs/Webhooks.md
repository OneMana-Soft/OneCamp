# OneCamp Webhooks — API Documentation

Webhooks let you connect OneCamp to external services. There are two types:

| Type | Direction | Use Case |
|------|-----------|----------|
| **Incoming** | External → OneCamp | Post messages to channels from bots, CI/CD, monitoring tools |
| **Outgoing** | OneCamp → External | Notify your services when events happen (new messages, task changes, etc.) |

---

## Incoming Webhooks

### How It Works

1. An admin creates an incoming webhook in **Admin → Webhooks → Create Webhook**
2. OneCamp generates a unique URL with a secret token
3. You `POST` JSON to that URL — the message appears in the target channel

### Webhook URL

```
POST https://your-onecamp-instance.com/webhook/incoming/{token}
```

The `{token}` is auto-generated. Keep it secret — anyone with the token can post messages.

### Request Format

```json
{
  "text": "Build #42 passed ✅ — deploying to production",
  "channel_id": "550e8400-e29b-41d4-a716-446655440000",
  "bot_name": "CI/CD Bot"
}
```

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `text` | Yes* | String | Message body. Supports plain text and HTML. |
| `blocks` | Yes* | Array | Slack-style rich blocks (see below). If provided, takes precedence over `text`. |
| `channel_id` | No | UUID | Target channel UUID. Overrides the webhook's default channel. |
| `channel` | No | String | The channel's name, with or without `#` (`#alerts`), when `channel_id` isn't given. |
| `bot_name` | No | String | Display name for the message. Defaults to the webhook's configured bot name. |

\* Either `text` or `blocks` must be provided.

### Routing Destinations

A message is posted in a channel, as the webhook's bot: the one `channel_id` or `channel` names, or else the webhook's default channel (configured at creation).

An incoming webhook doesn't message people. A payload with `dm_id` or `group_chat_id` is refused with `400`, even beside a channel. To message people, use an app.

### Example: Send a channel message

```bash
curl -X POST https://onecamp.onemana.dev/webhook/incoming/abc123token \
  -H "Content-Type: application/json" \
  -d '{
    "text": "Deploy completed successfully 🚀",
    "bot_name": "Deploy Bot"
  }'
```

### HTML Support

If your `text` starts with `<`, it's treated as HTML:

```json
{
  "text": "<p><strong>Alert</strong></p><ul><li>CPU: 92%</li><li>Memory: 78%</li></ul>",
  "bot_name": "Monitoring"
}
```

> **Note:** HTML is sanitized for security. Scripts, event handlers, and dangerous tags are stripped.

### Slack-Style Blocks (Rich Formatting)

For rich, structured messages, use a `blocks` array instead of plain `text`:

```json
{
  "blocks": [
    {
      "type": "section",
      "text": {
        "type": "mrkdwn",
        "text": "*Deployment Alert*\nBuild #42 passed"
      }
    },
    { "type": "divider" },
    {
      "type": "image",
      "image_url": "https://example.com/chart.png",
      "alt_text": "Performance chart"
    },
    {
      "type": "actions",
      "actions": [
        { "type": "button", "text": { "type": "plain_text", "text": "View Logs" }, "url": "https://example.com/logs" }
      ]
    }
  ]
}
```

Supported block types: `section`, `divider`, `image`, `actions`

- If `blocks` is provided, it takes precedence over `text`
- `text` is still required as a fallback for notifications

### Slash Commands

Incoming webhooks support slash commands. When a user types a registered command in a channel where the webhook bot is present, OneCamp routes it to the webhook.

**Default commands** (always available):
- `/help` — Show available commands
- `/status` — Show webhook status

**Custom commands** can be registered when creating or editing the webhook via the `commands` field.

Commands return an immediate response. If processing takes time, the handler can return a `response_url` and OneCamp will POST the final result asynchronously.

### Responses

| Status | Meaning |
|--------|---------|
| `200` | Message posted successfully. Response includes `post_id`. |
| `400` | Missing `text`/`blocks`, an invalid payload, or a payload naming `dm_id` or `group_chat_id`. |
| `401` | Invalid or disabled webhook token. |
| `403` | The webhook's creator isn't a member of the channel. |
| `429` | Rate limit exceeded (30 requests per minute per token). |

### Rate Limiting

Incoming webhooks are rate-limited to **30 requests per minute** per token.

- HTTP `429 Too Many Requests` is returned when the limit is exceeded
- The limiter is Redis-backed with an in-memory fallback
- Wait ~60 seconds before retrying

---

## Outgoing Webhooks

### How It Works

1. Admin creates an outgoing webhook in **Admin → Webhooks → Create Webhook**
2. Select which events to subscribe to and provide a target URL
3. When those events occur, OneCamp `POST`s a JSON payload to your URL
4. Your service processes the event and returns `2xx` to acknowledge

### Setting Up

1. **Name**: A descriptive name for the webhook
2. **Target URL**: Your HTTPS endpoint that will receive events
3. **Events**: Which event types it is sent (see below); at least one. An outgoing webhook is sent only
   the events it names, so creating or changing one with none is refused. An older webhook with none
   is listed as "Sends nothing: no kinds ticked" until an admin edits it and ticks one.
4. **Scope**: Limit events to a specific channel or project, or receive all org-wide
5. **Trigger Words** (optional): Only dispatch when the message contains specific words

### Available Event Types (23 total)

The list is the code's `EventTypes` (controllers/Webhook/webhookController.go), which `GET /admin/webhooks/event-types` answers; this page follows it.

#### Posts
| Event | Description |
|-------|-------------|
| `post.created` | A new post is created in a channel |
| `post.updated` | A post is edited |
| `post.deleted` | A post is deleted |

#### Chat Messages
| Event | Description |
|-------|-------------|
| `chat.created` | A new DM or group chat message is sent |
| `chat.updated` | A chat message is edited |
| `chat.deleted` | A chat message is deleted |

#### Tasks
| Event | Description |
|-------|-------------|
| `task.created` | A new task is created |
| `task.status_changed` | A task's status changes |
| `task.deleted` | A task is archived / soft-deleted |
| `task.restored` | A task is unarchived / restored |
| `task.changed` | A task's priority, due date, tags or name changed, with the field, what it was and what it is |

#### Channels
| Event | Description |
|-------|-------------|
| `channel.created` | A new channel is created |
| `channel.archived` | A channel is archived |

#### Users
| Event | Description |
|-------|-------------|
| `user.joined` | A user joins a channel |
| `user.left` | A user is removed from or leaves a channel |

#### Docs
| Event | Description |
|-------|-------------|
| `doc.created` | A doc is made |
| `doc.updated` | A doc's content changed: at most one a minute per doc, sent when the minute closes, so a burst of saves is one event |
| `doc.deleted` | A doc is moved to the trash |
| `doc.shared` | Someone gains access to a doc |
| `doc.comment.created` | Someone comments on a doc |

A doc's event goes to a webhook only when the person who set the webhook up can open
the doc (for `doc.deleted`, could open it before it went to the trash). A doc event
carries ids, never the doc's text, and a private doc's title is never in it.

#### Tables
| Event | Description |
|-------|-------------|
| `table.rows.changed` | A table's rows changed: one event for each change, however many rows it touched (a cell edited, a paste, a bulk delete, an undo, rows restored from the trash) |

A bulk change sends one `table.rows.changed` that names every row it touched, in place
of one event a row (see its payload below). It carries ids, never a cell's value.

#### Calls
| Event | Description |
|-------|-------------|
| `call.started` | A call in a channel or a meeting began |
| `call.ended` | A call in a channel or a meeting ended, with its times and how many joined |

A call's event goes to a webhook only for calls in channels and meetings the person who
set the webhook up can open. It carries ids, times and how many joined, never a name or
anything said. A call in a DM or a group chat is never sent.

### Payload Format

All outgoing webhooks receive:

```json
{
  "event": "task.status_changed",
  "timestamp": "2024-03-15T14:30:00Z",
  "data": {
    "task_id": "550e8400-...",
    "old_status": "inProgress",
    "new_status": "done",
    "project_id": "660e8400-...",
    "updated_by": "user_dgraph_uid"
  }
}
```

| Field | Description |
|-------|-------------|
| `event` | The event type string |
| `timestamp` | ISO 8601 UTC timestamp of the event |
| `data` | Event-specific payload (varies by event type) |

### Event-Specific Payload Examples

**post.created:**
```json
{
  "post_id": "uuid",
  "channel_id": "uuid",
  "text": "<p>Hello world</p>",
  "plain_text": "Hello world",
  "created_by": "user_dgraph_uid"
}
```

**chat.created (DM):**
```json
{
  "message_id": "uuid",
  "group_id": "grouping_id",
  "to_uuid": "recipient_dgraph_uid",
  "text": "<p>Hey!</p>",
  "plain_text": "Hey!",
  "created_by": "user_dgraph_uid"
}
```

**chat.created (Group Chat):**
```json
{
  "message_id": "uuid",
  "group_chat_id": "grp_abc123",
  "text": "<p>Team meeting in 5</p>",
  "plain_text": "Team meeting in 5",
  "created_by": "user_dgraph_uid"
}
```

**task.created:**
```json
{
  "task_id": "uuid",
  "project_id": "uuid",
  "name": "Fix login button",
  "description": "The login button is unresponsive on mobile",
  "status": "todo",
  "priority": "high",
  "created_by": "user_dgraph_uid"
}
```

**task.status_changed:**
```json
{
  "task_id": "uuid",
  "old_status": "inProgress",
  "new_status": "done",
  "project_id": "uuid",
  "updated_by": "user_dgraph_uid"
}
```

**table.rows.changed:**
```json
{
  "table_id": "uuid",
  "table_name": "Deals",
  "change_set_id": "uuid",
  "change": "paste",
  "rows_changed": 2,
  "created_row_ids": [],
  "updated_row_ids": ["uuid", "uuid"],
  "deleted_row_ids": [],
  "restored_row_ids": [],
  "field_ids": ["uuid"],
  "row_field_ids": {"uuid": ["uuid"], "uuid": ["uuid"]}
}
```

Each row the change touched is in exactly one of the four lists, by what happened
to it: made, changed (its cells, its place or its links), deleted (to the table's
trash), or brought back. `row_field_ids` names, row by row, the fields whose cells
or links changed; `field_ids` is all of them. `change` is what kind of change it was:
`edit`, `create`, `delete`, `paste`, `fill`, `import`, `move`, `links`, `undo`, `redo`,
`restore` or `field` (a field's change rewrote its cells: an option renamed, its type
changed). A change is at most 1,000 rows, except a field's, which can change every
row of its table: its event names at most 1,000 and `rows_changed` says how many
it changed. A webhook sent only ids keeps every key here but `table_name` and
`change`. An AI column's fill sends none.

**doc.shared:**
```json
{
  "doc_uuid": "uuid",
  "doc_private": false,
  "doc_title": "Launch notes",
  "actor_uuid": "uuid",
  "user_uuids": ["uuid"],
  "level": "edit",
  "at": "2026-10-14T09:30:00Z"
}
```

**call.started** (a call starting in a channel) and **call.ended** (a meeting's call ending):

```json
{
  "event": "call.started",
  "timestamp": "2026-11-02T10:00:03Z",
  "data": {
    "session_id": "3f2b8c1e-5d4a-4f6b-9c2d-7e8f9a0b1c2d",
    "channel_id": "c9f0f895-fb98-4b91-8e1f-2d3c4b5a6978",
    "actor_uuid": "45c48cce-2e2d-4fbd-a2b1-6f7e8d9c0b1a",
    "started_at": "2026-11-02T10:00:02.418329Z"
  }
}
```

```json
{
  "event": "call.ended",
  "timestamp": "2026-11-02T10:31:07Z",
  "data": {
    "session_id": "3f2b8c1e-5d4a-4f6b-9c2d-7e8f9a0b1c2d",
    "meeting_id": "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d",
    "started_at": "2026-11-02T10:00:02.418329Z",
    "ended_at": "2026-11-02T10:31:06.902114Z",
    "duration_s": 1864,
    "people": 6,
    "reason": "last_left"
  }
}
```

Both carry `session_id` (the call: its two events share it), `started_at`, and the call's
`channel_id` or `meeting_id`. `actor_uuid` is who started the call, or the host who ended it
for everyone; it is absent when a call ended by itself. `call.ended` adds `ended_at`,
`duration_s`, `people` (how many joined) and `reason`: `host_ended`, `last_left`,
`room_finished`, or `reconciled` (OneCamp found the call had already ended, as after a
restart). The fields are business/CallState/webhooks.go's.

### Headers Sent by OneCamp

| Header | Description |
|--------|-------------|
| `Content-Type` | `application/json` |
| `User-Agent` | `OneCamp-Webhook/1.0` |
| `X-OneCamp-Event` | The event type (e.g., `task.status_changed`) |
| `X-OneCamp-Webhook-Id` | The webhook's UUID |
| `X-OneCamp-Delivery` | The event's id: the same on every try and every replay, so you can drop a repeat |
| `X-OneCamp-Retry-Num` | `0` on the first try, then `1`, `2` and so on |
| `X-OneCamp-Signature` | `v1=` and the HMAC-SHA256 signature, in lowercase hex (below) |
| `X-OneCamp-Timestamp` | When this try was signed, in Unix seconds |

### Verifying Signatures

Each outgoing webhook gets its own signing secret when it is made: **Admin → Webhooks** shows it,
and **Regenerate Secret** replaces it. Every delivery is signed:

```
X-OneCamp-Signature: v1=<hex>
X-OneCamp-Timestamp: 1710508200
```

The signature is the HMAC-SHA256, in lowercase hex, keyed with the secret, of the canonical string:

```
v1:<timestamp>:<body>
```

where `<timestamp>` is the `X-OneCamp-Timestamp` header and `<body>` is the raw request body.
Compute it over the body's bytes exactly as they arrived, compare in constant time, and refuse
an old timestamp (for example, one more than five minutes old) so a captured request can't be
replayed. The code that signs is `Sign` in business/Hooks/deliveries.go.

**Python:**

```python
import hashlib
import hmac


def verify(body: bytes, signature: str, timestamp: str, secret: str) -> bool:
    version, _, received = signature.partition("=")
    if version != "v1":
        return False
    expected = hmac.new(secret.encode(), f"v1:{timestamp}:".encode() + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, received)
```

**Node.js:**

```js
const crypto = require("crypto")

// body is the raw request body, as a Buffer.
function verify(body, signature, timestamp, secret) {
  const [version, received] = signature.split("=")
  if (version !== "v1" || !received) return false
  const expected = crypto.createHmac("sha256", secret).update(`v1:${timestamp}:`).update(body).digest("hex")
  return received.length === expected.length && crypto.timingSafeEqual(Buffer.from(expected), Buffer.from(received))
}
```

### Reliability & Retry

- Answer with any `2xx` within 10 seconds. Any other answer, or none, is a failed try.
- A failed delivery is tried again, up to **10 tries** in all over about a day: after 10 seconds,
  1 minute, 5 minutes and 30 minutes, then 1, 2, 4, 6 and 8 hours, each a little later at random
  so webhooks that failed together come back apart. After its last try it is kept 30 days in
  **Admin → Webhooks**, where it can be sent again.
- After **50 failed tries in a row**, the webhook pauses itself and OneCamp emails the person who
  set it up. **Admin → Webhooks** says why, with **Resume**. A paused webhook's deliveries wait
  (up to 30 days) and go when it is resumed.
- Every try and every replay of an event carries the same body and the same `X-OneCamp-Delivery`,
  the event's id.
- **Admin → Webhooks → View Logs** shows each delivery and its answer.
- These numbers are the code's: `retrySchedule`, `MaxTries` and `PauseAfter` in
  business/Hooks/deliveries.go.

### Scope Filtering

Control which events your webhook receives:

| Scope | Receives Events From |
|-------|---------------------|
| **Organization-wide** (`org`, default) | All events across the entire organization |
| **Channel** (`channel`) | Only events from a specific channel |
| **Project** (`project`) | Only task events from a specific project |
| **DM** (`dm`) | Only DM events |
| **Group Chat** (`group_chat`) | Only group chat events |

### Trigger Words

Filter events by message content. For example, if trigger words are `["urgent", "p0"]`:

- Message: "Hey team, can someone review my PR?" → **Not dispatched** ❌
- Message: "URGENT: Production is down!" → **Dispatched** ✅

Trigger words are case-insensitive and match against the message text.

### Testing

Use **Admin → Webhooks → Test** to send a test event:

```json
{
  "event": "test",
  "timestamp": "2024-03-15T14:30:00Z",
  "data": {
    "test": true,
    "message": "This is a test event from OneCamp"
  }
}
```

Test results appear immediately in **Admin → Webhooks → View Logs**.

### Viewing Logs

Each delivery is logged with:
- Timestamp
- Event type
- HTTP status code received
- Duration in milliseconds
- Error message (if any)
- Success/failure indicator

Logs are available at **Admin → Webhooks → View Logs** for each webhook.

### Best Practices

1. **Respond quickly** — OneCamp has a 10-second timeout. Acknowledge the event and process it asynchronously
2. **Return 2xx** — Any 2xx status code counts as success. Non-2xx counts as failure
3. **Verify signatures** — Check the `X-OneCamp-Signature` header (above) to make sure the request came from OneCamp
4. **Use HTTPS** — Target URLs must use HTTPS
5. **Be idempotent** — The same event may be delivered more than once. `X-OneCamp-Delivery` is the event's id: use it to drop a repeat
6. **Monitor failures** — Check the webhook logs periodically. After 50 failed tries in a row, the webhook pauses itself until it is resumed

---

## Admin Management

Admins can manage webhooks at **Admin → Webhooks**:

| Action | Description |
|--------|-------------|
| **Create** | Set up a new incoming or outgoing webhook |
| **Edit** | Change name, description, target URL, events, scope, active status |
| **Regenerate Token** | Generate a new token (invalidates the old one). Also resets the failure count to 0. |
| **Regenerate Secret** | Generate a new HMAC secret (invalidates old signatures) |
| **Test** | Send a test event to verify connectivity |
| **View Logs** | See delivery history with status codes and errors |
| **Delete** | Soft-delete the webhook (can be restored later if needed) |
