# AI Teammate Background & Async Runs

How an AI agent (a "teammate") answers a mention or DM either **instantly**
(synchronous, the default) or as a **durable background job** that shows live
progress, survives restarts, and pauses for a human when it's blocked.

## The two modes

| | Synchronous (default) | Background (durable) |
|---|---|---|
| When | Every agent, out of the box | Per-agent opt-in, or a bounded sync run that hands off |
| UX | One reply when the run finishes | An evolving in-thread comment: "On it… → working (used: …) → result" |
| Survives a restart | No | Yes (job is reclaimed and retried) |
| Blocked on a human decision | Posts the question, run ends | Parks as `awaiting_input`; a reply in the thread resumes it |
| Hit a daily budget cap | Posts a short pause note | Parks and resumes after the cap resets |

Both modes run through the **same** agent runner: the owner envelope, per-tool
permission re-checks, token/step caps, and autonomy modes all still apply.

## Turning it on

### Per-agent (recommended)
Admin → Agents → edit an agent → **Run tasks in the background** (`run_in_background`).
When on, that agent's channel/thread @mentions, group-chat @mentions, and 1:1
DMs run as durable jobs. Best for multi-step work; leave off for instant,
single-shot replies. Surfaced in the agents list as a **Background** badge.

### Automatic hand-off (opt-in, cautious)
Set the environment flag **`AI_AGENT_ASYNC_HANDOFF=true`** (default OFF) to let a
*synchronous* channel-mention run that hits a hard bound (step / per-run token /
wall-clock limit) **with real tool progress** continue as a durable job instead
of returning a truncated answer. The synchronous run's conversation is seeded
into the durable job so it resumes mid-flight — the runner's dedupe set prevents
repeating a write it already performed. Any per-agent opt-in does **not** depend
on this flag.

## Reply surfaces

A durable job carries a small, generic `Surface` descriptor so the same worker
can post its status on any surface:

- `task` — a comment on the assigned project task (the original durable surface)
- `channel_post` — an in-thread comment on the triggering channel post
- `group_chat` — an in-thread comment on the triggering group-chat message
- `dm` — an in-thread comment on the triggering 1:1 DM message

Adding a new surface is a new status poster, not a change to the worker loop.

## Resuming a paused run

When a durable run pauses (a `needs_human` blocker or a budget cap), it posts a
question/pause note in the thread. A human **reply in that same thread** resumes
the job in place — no re-@mention needed. This works on all surfaces above; the
follow-up is appended to the durable conversation so the resumed run continues
with full context.

## Seeing what's in flight

Admin → Agents shows an **In progress** panel: the open durable jobs across the
agents you can see, bucketed as **blocked** (waiting on you), **working**, and
**queued** — blocked first. It polls gently and hides itself when nothing is
active. This is the roll-up; the evolving in-thread status comment remains the
real-time, click-through surface for a specific job. Backed by
`GET /agents/work` (scoped: admins see the workspace, members their own agents).

## Operator tuning (environment)

| Variable | Default | Meaning |
|---|---|---|
| `AI_AGENT_ASYNC_HANDOFF` | `false` | Allow a bounded sync run to hand off to a durable continuation |
| `AI_AGENT_TASK_CONCURRENCY` | `6` | Workspace-wide ceiling on in-flight durable agent jobs |
| `AI_AGENT_TASK_PER_AGENT` | `3` | How many jobs one teammate may run at once (`0` = only the global ceiling) |
| `SERVICE_ROLE` | `all` | Which half of the server this process is: `all`, `api` (requests only, no loops) or `worker` (loops only, `/health` only). Add `go-worker` replicas with `docker compose --profile workers up -d --scale go-worker=N`; the ceiling above is per process, so N workers is N times it. The `agent-queue` system check goes red if due work sits unclaimed for two minutes, which is what an all-`api` fleet looks like. |

All AI work is inert while AI is disabled: an idle workspace never leases a job
or spends a model call.

## Migrations

Applied manually by the operator:

- **116** `ai_agent_tasks.surface` (jsonb) — the reply-surface descriptor. Older
  rows without it decode to the legacy `task` surface, so existing jobs are
  unchanged.
- **117** `ai_agents.run_in_background` (boolean, default `false`) — the
  per-agent opt-in.

## Where the config lives (Postgres vs Dgraph)

Agent **configuration** and the **durable queue** are structured records, so they
live in Postgres (`ai_agents`, `ai_agent_tasks`). Dgraph holds the **social
graph** only (users, channels, posts, comments, memberships). This keeps agent
config transactional and queryable without touching the graph.

## Multimodal note

If a mention/DM carries an image and a **vision model** is configured (Admin →
AI Models → Vision), the agent is given a text description of the image so it can
"see" a shared screenshot. This is best-effort and **inert** until an actual
multimodal model is selected — it is a separate setting from the chat model.
