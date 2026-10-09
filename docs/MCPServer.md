# OneCamp as an MCP Server

Connect Claude, Cursor, or your own agent to a OneCamp workspace over the
[Model Context Protocol](https://modelcontextprotocol.io). The agent reads and acts on
real workspace data — channels, docs, tasks, projects, tables — as a specific person,
bounded by that person's own permissions.

Two things make this different from pointing an agent at an API:

- **Every call is authorised against the specific object**, live, at call time. Not
  against a role granted once. If someone loses access to a channel this morning, their
  agent loses it on the next call.
- **Every call is recorded, including the refusals.** "Your agent tried to read
  #board-private and was stopped because the person who authorised it is not a member" is
  a sentence you can get out of the audit log.

---

## Before you start: turn the surface on

**The MCP surface is off by default, including on upgrade.** A workspace that has said
nothing exposes nothing.

This is deliberate. Exposing workspace content to external AI clients is a decision
somebody should make on purpose, and defaulting it on would mean it was never decided.
If you are upgrading an existing OneCamp instance and had agents connected, **they will
stop working until an admin completes this step.**

In **Admin → AI Models → MCP server**:

1. Turn on **Allow external agents to connect**.
2. Choose which **tool groups** to expose. Start with one, watch the audit log, then
   widen.
3. Save. Both values save together, because enabling the surface while nothing is
   selected exposes nothing.

Tool groups are the read/write areas of the product, not individual tools: `tasks`,
`projects`, `docs`, `messages`, `tables`, `calendar`, `data_sources`, `search`. The
choice is deliberately coarse — "may agents read our documents" is a question an admin
can weigh, while "should `read_doc` be on but `summarize_channel` off" is one people
answer by enabling everything, which is the same as having no control.

Selecting **All tool groups** is a real answer and means "including groups added by
future upgrades", so you are not re-approving on every release.

While the surface is off, or a tool's group is not enabled, the endpoint refuses the call
and the tool does not appear in the catalogue. A client cannot discover that the
workspace has a capability you chose not to expose.

---

## Connecting

### 1. Create a token

**Settings → API tokens → New token.** Choose a name, the scopes it needs, and
optionally an expiry.

The token is shown **once**. It is stored only as a SHA-256 hash, so it cannot be
retrieved or emailed to you later — if you lose it, revoke it and make another.

Tokens look like `oc_` followed by 40 hex characters. Each person may hold up to 50
active tokens.

The dialog that shows the token also offers a ready-made client config with **that token already
substituted**, under *Use this token with an MCP client*. That is the only moment it can: after the
dialog closes, only a hash remains, so nothing can produce the credential again. Copy it from there
and you can skip the next step.

### 2. Point your client at the endpoint

Your instance prints its own endpoint in **Admin → AI Models → External agent access**, once the
surface is enabled, along with a copy-paste client config and a one-line request to test it. Prefer
those over the examples below — they carry your real hostname rather than a placeholder.

```
POST https://your-onecamp-instance.com/v1/mcp
Authorization: Bearer oc_your_token_here
Content-Type: application/json
```

Streamable-HTTP MCP: one endpoint, JSON-RPC 2.0 in, JSON-RPC 2.0 out. The server agrees to
protocol `2025-06-18`, `2025-03-26` or `2024-11-05`, whichever the client asks for, and
answers `2025-06-18` otherwise. It keeps no session state, so there is nothing to reconnect
and nothing to expire mid-conversation.

**Signing in without a token.** Any MCP client that supports OAuth connects by URL, whatever
model it runs on: Open WebUI on a local Ollama model, Claude, ChatGPT, Grok Bot, Cursor and
others. Add `https://your-onecamp-instance.com/v1/mcp` as a remote MCP server and it signs you
in. A client that can only send a header uses an API token instead, as below. You approve the connection in OneCamp, choosing the agent it acts as (a new one named
after the client, or one you already sponsor) and what it may do. The client then holds an
ordinary credential bound to that agent, renewed hourly, which the agent inventory lists and
the agent's kill switch stops. An admin still has to allow outside agents above first.

For a client that reads a config file, the shape is usually:

```json
{
  "mcpServers": {
    "onecamp": {
      "url": "https://your-onecamp-instance.com/v1/mcp",
      "headers": { "Authorization": "Bearer oc_your_token_here" }
    }
  }
}
```

### 3. Check what you can see

`tools/list` returns the intersection of three narrowings, and each one can only reduce
the list:

| Narrowing | Question it answers | Set by |
|---|---|---|
| Token scopes | What may this credential do? | whoever created the token |
| Admission | What does this workspace expose at all? | an admin |
| Agent toolset | What was this agent configured to use? | the agent's owner, when the token is bound to an agent |

An empty list is normal and usually means the surface is off or no group is enabled.

---

## What the token can reach

A token acts **as the person who created it**, never more. The scopes narrow that person
down; they never widen it. A token with `docs:read` held by someone who cannot open
`#private-hr` still cannot read its documents.

### Scopes

| Scope | Grants |
|---|---|
| `tasks:read` / `tasks:write` | read tasks; change status, assignee, due date |
| `projects:read` / `projects:write` | read projects and teams; create projects |
| `docs:read` / `docs:write` | read documents; create documents |
| `messages:read` / `messages:write` | summarise conversations; post messages and DMs |
| `tables:read` / `tables:write` | read and query tables; create and update rows |
| `calendar:write` | create reminders and events |
| `data_sources:read` | read and query connected external data sources |
| `attention:read` | read what is waiting for you: unread counts (`GET /v1/unread`) and, in the AI edition, approvals and overdue tasks (`GET /v1/attention`); nothing else |
| `search:read` | search across the workspace, memory, and connected apps |

Grant the narrowest set that does the job. A read-only agent should hold only `:read`
scopes — that alone makes the whole write path unreachable for it.

---

## Governed tools

All 31 of the 31 public tools are **governed**: before the tool runs, the server resolves
exactly what the call will touch and checks the authorising person's live permission on
it.

| Group | Read | Write |
|---|---|---|
| `tasks` | `list_tasks`, `list_project_tasks` | `update_task_status`, `assign_task`, `set_task_due_date`, `create_task` |
| `projects` | `list_projects`, `read_project`, `list_teams` | `create_project` |
| `docs` | `read_doc` | `create_doc` |
| `messages` | `summarize_channel`, `summarize_dm`, `summarize_group_chat` | `send_message`, `send_dm`, `send_group_chat` |
| `tables` | `list_tables`, `read_table`, `query_table`, `query_plan` | `create_table_row`, `update_table_row`, `link_table_rows` |
| `search` | `search_workspace` | — |
| `data_sources` | `list_data_sources`, `read_data_source`, `query_data_source`, `query_data_source_plan` | — |
| `calendar` | — | `set_reminder` |

The authority rule depends on what the object is, and matches what the app itself
requires:

- **Channel** — membership to read. Posting to an admins-only channel additionally
  requires being an admin of it.
- **Task** — membership of the owning project to read; **project admin** to change.
  Seeing a task on a board does not carry the right to reassign it.
- **Document** — a private document needs an explicit grant on it; a non-private one is
  readable workspace-wide. Editing is decided separately and does not follow from being
  able to read: a public document being readable by everyone says nothing about who may
  rewrite it.
- **Table** — the table's visibility rule to read; its manage rule to write.
- **Conversation** — participation. A DM additionally checks that the person on the other
  end can actually receive it.
- **Project** — membership to read.
- **Team** — membership to read; team **admin** to create a project in it. A deleted team
  is refused, even though it still lists its admins.
- **External data source** — the source's own query rule: an admin, its creator, or the
  source being marked workspace-visible. A source an admin has disabled is refused. The
  connectors are read-only by construction, and a write request against one is refused
  outright rather than being authorised against some other rule.

A deleted or archived object is refused, and refused with that reason rather than "not
permitted", so an operator reading the log is not sent hunting.

### Authority for the tools that create

A create names nothing of its own, so it is authorised against what it creates **into**:

| Tool | Creates into | Requires |
|---|---|---|
| `create_task` | a project | project admin |
| `create_project` | a team | team admin |
| `create_table_row` | a table | the table's manage rule |

Being a *member* of a project or team is not enough to create in it. That is the same rule
the app applies, so an agent cannot create work its owner could not create by hand.

Two creations name no container at all — `create_doc` and `set_reminder` make something
that belongs to you and sits in no parent. There is no object to check the way a channel or
a task is checked, because the object does not exist yet and will be yours. Their authority
is you being an active member plus the token scope, and they are marked that way internally
so the narrower guarantee is explicit rather than implied.

They still get everything else, and the part that matters most is deduplication: a retried
`create_doc` does not make two documents.

To keep them unreachable, do not enable the `calendar` group and grant no `docs:write`
scope.

---

## Writes

### Nothing is done twice

MCP clients may retry a call whose response was lost, and the specification treats the
hints a server publishes as advisory — so a client may retry whatever it likes. A write
that is not naturally safe to repeat therefore carries an identity derived from its own
arguments, and the second arrival of the same call is recognised and answered without
running again.

So a retried `send_message` does not post twice, and a retried `create_table_row` does
not add a second row. Sending genuinely identical content much later is a new message,
not a retry.

Writes that are naturally safe to repeat — setting a status, an assignee, a due date, a
row's values — need none of this: applying them twice leaves what applying them once
leaves.

### Approvals

The surface supports requiring a human to approve a specific act before it runs, using
the same in-thread Approve/Deny card OneCamp already shows for work an in-app agent
proposes. The approver is whoever is looking at the workspace, and it is never the
caller — so an approved destructive write involves two independent people: the one who
authorised the credential and the one who approved the act.

**No tool currently triggers this.** Every governed write today is additive — it adds
content and removes none — and prompting on every message an agent sends would train
people to click Approve without reading, which is worse than not asking. The mechanism is
in place for the destructive tools that come later.

### What this surface deliberately cannot do

OneCamp's in-app assistant can send email, create Google Calendar events, and comment on
GitHub issues. **None of those is reachable over this surface, by design.** Each acts as a
particular person through a connection that person authorised interactively, and each has
an effect that leaves the workspace and cannot be recalled: mail is delivered, attendees
are notified, a comment is published under their name.

A bearer credential cannot supply what those actions require. It proves a token was
issued; it does not put a person in front of a screen to confirm that this mail, to this
address, should go now. So they are absent from the public tool map rather than exposed
with a warning attached, and adding one would mean first giving this surface a way to
demand approval *before* the call runs, not after.

The same rule binds OneCamp's own autonomous agents, which do run those tools: an agent
at full autonomy still routes each of them to a human, because the property that makes
them unsafe unattended is the action's own irreversibility and not the caller's identity.

---

## Rate and cost controls

| Control | Default | Scope |
|---|---|---|
| Read calls | 240 / minute | per credential |
| Write calls | 30 / minute | per credential |
| Daily AI tokens | admin-set | per workspace, per person, per agent, per channel |

Reads and writes are budgeted separately on purpose. A runaway read loop wastes cycles; a
runaway write loop puts a thousand messages in a channel, and no amount of auditing
un-sends those.

The call budget **fails open**: if the rate-limit cache is unavailable, calls proceed.
Every authorisation and audit control fails closed. The distinction is deliberate — fail
closed on questions of authority, fail open on questions of volume, because turning a
cache outage into a product outage protects nobody.

Tools that reach a model — `search_workspace` embeds your query, the summarisers run a
completion — spend AI tokens and are metered against the workspace, the person, and, when
the token is bound to an agent, that agent's own daily cap.

---

## Agent identities

A token can be bound to an **agent identity** at creation: **Settings → API tokens → New
token → Act as an agent**.

An agent is a first-class identity rather than a human to impersonate or a shared service
account to hide inside. It holds no credentials of its own — the token is a separate
thing that points at it — so one agent can hold several tokens (a rotation window, one per
client) and every one of them resolves to the same actor in the audit trail.

Binding changes four things:

| | Unbound token | Bound to an agent |
|---|---|---|
| Audit trail says | "an API client" | the agent's name |
| Deactivating the agent | no effect | stops every one of its tokens, on the next call |
| AI spend counts against | the workspace and the owner | those **and** the agent's own daily cap |
| Tools available | whatever the scopes allow | **only** the tools the agent has enabled |

That last row surprises people: a bound token can do **less** than its scopes suggest. If
the agent has no tools enabled, the token cannot do anything at all. The token creation
screen lists the agent's enabled tools when you select it, so this is visible before the
token exists.

You may only bind a token to an agent you own (or any agent, if you are an admin).
Binding is set at creation and never changes — a credential that could change which
identity it acts as would make every audit row written before the change mean something
different.

**Deactivating an agent is the kill switch.** It is checked on every call, so it takes
effect on the next request rather than whenever someone remembers to hunt down its
tokens. A kill switch that waits for a rotation is not a kill switch.

### Where a bound agent may act

If the agent is confined to particular channels or projects in the agent builder, that
confinement applies here too — a call naming a channel or project the agent was not scoped
to is refused, on both the governed and ungoverned tools.

The confinement is about **the channels and projects an agent acts in**, not a general
reduction of what it can see. An agent scoped to `#support` can still read a document or
run a workspace search that its owner could, because a call naming neither a channel nor a
project is not confined. This is exactly how the same agent behaves inside OneCamp, on
purpose: a control that changed meaning depending on how the agent was invoked would be
worse than one with a clearly stated boundary.

An agent with no confinement configured may act wherever its owner can — that is the
default, and every agent created before the setting existed has it.

---

## Auditing

Every decision is recorded before the tool runs — allowed and refused alike — under the
**integration** category in **Admin → Settings → Audit log**. A failure to record is fatal to the
call: acting on someone's workspace with no record that it happened is worse than not
acting.

Each row carries who acted (the agent by name, or the credential), the accountable
person, the tool, the object, the outcome, and the reason. Refusals are the half worth
reading: they are how you find an agent trying to reach something it should not, before
anything goes wrong.

---

## Troubleshooting

| What you see | What it means |
|---|---|
| You cannot find the endpoint URL | Admin → AI Models → External agent access shows it once the surface is on, with a copy button. It is `/v1/mcp` on your API host. |
| `tools/list` returns nothing | The surface is off, no tool group is enabled, or the token holds no scopes. Check Admin → AI Models → MCP server first. |
| "the MCP surface is not enabled for this workspace" | An admin has not turned it on. |
| "the *X* tool group is not enabled for this workspace" | Deliberate narrowing. Ask an admin whether to widen it. |
| "this credential does not carry the scope this tool requires" | Make a new token with the missing scope; scopes are fixed at creation. |
| "invalid or inactive credential" | Revoked, expired, or mistyped. All three answer identically on purpose — telling them apart would let someone probe for valid tokens. |
| "the agent identity behind this credential is deactivated" | The kill switch. Reactivate the agent or use an unbound token. |
| *agent name* "has no tools enabled" | The agent's own configuration is empty. Enable tools on the agent, not on the token. |
| *agent name* "is not scoped to this channel" / "this project" | The agent is confined in the agent builder. Widen its scope there, or use a token bound to a differently scoped agent. |
| "the originating person is not a member of this channel" | Working as intended. The token cannot exceed its owner's access; add them to the channel. |
| "this channel is admins-only for posting" | Reading is fine for any member; posting is restricted. |
| "changing this task requires being an admin of its project" | Reading a task and changing it are different rights. |
| "read/write call budget exhausted for this credential" | Usually a loop. The response says when to retry. |
| "an identical call was already applied" | Deduplication working: a retry was recognised rather than performed twice. |

---

## Related

- [`docs/Webhooks.md`](./Webhooks.md) — for one-way notifications, which need no agent.
- [`docs/SlashCommandsAndApps.md`](./SlashCommandsAndApps.md) — for in-app commands.
- `migrations/92_create_api_tokens.up.sql`, `migrations/138_*`, `migrations/139_*` — the
  schema behind tokens, agent binding, and admission control.
