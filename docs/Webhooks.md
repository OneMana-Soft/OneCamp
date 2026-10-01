# OneCamp Webhooks — API Documentation

Webhooks let you connect OneCamp to external services. There are two types:

| Type | Direction | Use Case |
|------|-----------|----------|
| **Incoming** | External → OneCamp | Post messages to channels, DMs, or group chats from bots, CI/CD, monitoring tools |
| **Outgoing** | OneCamp → External | Notify your services when events happen (new messages, task changes, etc.) |

---

## Incoming Webhooks

### How It Works

1. An admin creates an incoming webhook in **Admin → Webhooks → Create Webhook**
2. OneCamp generates a unique URL with a secret token
3. You `POST` JSON to that URL — the message appears in the target channel/DM/group chat

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
| `dm_id` | No | UUID | Target DM recipient UUID. |
| `group_chat_id` | No | String | Target group chat grouping_id (not a UUID). |
| `bot_name` | No | String | Display name for the message. Defaults to the webhook's configured bot name. |

\* Either `text` or `blocks` must be provided.

### Routing Destinations

Include one of these fields in the payload to determine where the message is posted:

| Field | Destination |
|-------|-------------|
| `channel_id` | Post in a channel |
| `dm_id` | Send a direct message |
| `group_chat_id` | Post in a group chat |

If none are provided, the webhook's default channel (configured at creation) is used.

### Example: Send a channel message

```bash
curl -X POST https://onecamp.onemana.dev/webhook/incoming/abc123token \
  -H "Content-Type: application/json" \
  -d '{
    "text": "Deploy completed successfully 🚀",
    "bot_name": "Deploy Bot"
  }'
```

### Example: Send a DM

```bash
curl -X POST https://onecamp.onemana.dev/webhook/incoming/abc123token \
  -H "Content-Type: application/json" \
  -d '{
    "text": "Hey! Your PR has been approved.",
    "dm_id": "550e8400-e29b-41d4-a716-446655440000"
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
| `200` | Message posted successfully. Response includes `post_id` or `message_id`. |
| `400` | Missing `text`/`blocks`, or invalid payload. |
| `401` | Invalid or disabled webhook token. |
| `403` | Permission denied (e.g., DM recipient not in org, or webhook creator not in group chat). |
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
3. **Events**: Which event types to subscribe to (see below)
4. **Scope**: Limit events to a specific channel or project, or receive all org-wide
5. **Trigger Words** (optional): Only dispatch when the message contains specific words

### Available Event Types (14 total)

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

### Headers Sent by OneCamp

| Header | Description |
|--------|-------------|
| `Content-Type` | `application/json` |
| `User-Agent` | `OneCamp-Webhook/1.0` |
| `X-OneCamp-Event` | The event type (e.g., `task.status_changed`) |
| `X-OneCamp-Webhook-Id` | The webhook's UUID |
| `X-OneCamp-Delivery` | A unique ID for each delivery attempt |
| `X-OneCamp-Retry-Num` | Retry attempt number (`0` = first try) |
| `X-OneCamp-Signature` | HMAC-SHA256 signature (if a secret is configured) |
| `X-OneCamp-Timestamp` | Unix epoch timestamp used in signature |

### Verifying Signatures

If you configured a **secret** for the webhook, OneCamp signs each payload:

```
X-OneCamp-Signature: v1=<base64>
X-OneCamp-Timestamp: 1710508200
```

The signature is computed over the canonical string:

```
v1:<timestamp>:<payload_bytes>
```

**Verify in your service:**

```python
import hmac, hashlib, base64

def verify_signature(payload_body, signature_header, timestamp_header, secret):
    version, received_b64 = signature_header.split("=", 1)
    if version != "v1":
        return False

    canonical = f"v1:{timestamp_header}:".encode() + payload_body
    expected = base64.b64encode(
        hmac.new(secret.encode(), canonical, hashlib.sha256).digest()
    ).decode()

    return hmac.compare_digest(expected, received_b64)
```

```javascript
const crypto = require('crypto');

function verifySignature(payload, signatureHeader, timestampHeader, secret) {
    const [version, received] = signatureHeader.split('=');
    if (version !== 'v1') return false;

    const canonical = `v1:${timestampHeader}:${payload}`;
    const expected = crypto
        .createHmac('sha256', secret)
        .update(canonical)
        .digest('base64');

    return crypto.timingSafeEqual(Buffer.from(expected), Buffer.from(received));
}
```

### Reliability & Retry

- OneCamp retries failed deliveries **up to 3 times** with exponential backoff (1s, 2s, 4s)
- After **10 consecutive failures**, the webhook is automatically disabled
- You'll see failure counts and logs in **Admin → Webhooks → View Logs**
- A **delivery ID** (`X-OneCamp-Delivery`) uniquely identifies each attempt

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
3. **Verify signatures** — If you set a secret, verify the `X-OneCamp-Signature` header to ensure the request came from OneCamp
4. **Use HTTPS** — Target URLs must use HTTPS
5. **Be idempotent** — The same event may be delivered more than once. Use the delivery ID to deduplicate
6. **Monitor failures** — Check the webhook logs periodically. After 10 failures, the webhook auto-disables

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
