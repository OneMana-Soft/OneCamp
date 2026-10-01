# AI Coding Teammate & the Autonomous PR Loop

How to assemble a GitHub coding agent in OneCamp — from "explain this bug" all
the way to "open a PR, follow CI, and report back" — using the existing agent
primitives. Nothing here needs custom code: it's configuration of tools,
triggers, autonomy, and (for writes) a GitHub MCP server.

## The design in one line

**Reads are native and fast; writes are governed and vendor-neutral.** OneCamp
ships read-only code tools built in, and routes code *writes* (commit, open a
PR) through an admin-registered GitHub MCP server so every write is vendor-neutral
and passes the same approval gate as any other action.

## The pieces

### 1. Read-only code tools (built in)
Enable these on an agent in the builder under **Code (GitHub, read-only)**. They
operate on the workspace's connected GitHub repo, run as the agent's owner, and
never write:

| Tool | What it does |
|---|---|
| `repo_summary` | Default branch, top-level structure, recently merged work — orient first |
| `search_repo_code` | Find which files contain a symbol/string (default branch only) |
| `read_repo_file` | Read a specific file at an optional ref (bounded to 64KB) |
| `list_commits` | Real-time, date-accurate commits on a branch (works for private repos) |
| `list_recent_changes` | PRs merged in the last N days |
| `code_analyze` | Retrieve relevant files + return a root-cause explanation and a **proposed** diff (LLM-heavy; never commits) |

Good grounding sequence for a fix: `search_repo_code` → `read_repo_file` →
`code_analyze`.

### 2. Writing code — a GitHub MCP server
To let the agent actually open a PR, an admin registers a GitHub (or coding) MCP
server under Admin → AI → MCP servers. Its write tools (e.g. create-branch,
commit, open-PR) then appear in the builder under **MCP tools** and can be
enabled per agent. Because they're writes, they flow through the agent's
**autonomy** setting:
- **Auto** — executes directly (use only for trusted, well-scoped agents).
- **Approval** — each write is proposed for a human to approve in-thread.
- **Plan-approve** — the whole plan is proposed as one approval.

### 3. Triggers — what starts the agent
Set the agent's trigger in the builder:
- **Mention** — a teammate `@`s the agent on a channel/thread/DM ("fix the null
  check in auth.go and open a PR").
- **Event** — pick a GitHub event so the agent runs itself:
  - `github.issue.opened` — triage/propose a fix when an issue lands.
  - `github.pr.review_submitted` — react when a review lands.
  - `github.check_run.completed` — fires **once per PR when all checks finish**
    (aggregated, not per-check), with the overall pass/fail + counts. This is how
    a PR-follow agent reacts to CI without being spammed.

### 4. Durability, progress, notifications (automatic)
- Turn on **Run tasks in the background** for multi-step work: the run becomes a
  durable job that survives restarts, shows an evolving in-thread status comment
  ("On it… → working… → result"), and pauses on `needs_human`/budget.
- The person who triggered it is **pinged** when it finishes or gets blocked.
- The **In progress** panel (Admin → Agents) shows queued/working/blocked jobs.

## Two worked examples

### A. On-demand fix (mention trigger)
1. Agent tools: `search_repo_code`, `read_repo_file`, `code_analyze`, + the MCP
   open-PR tool. Autonomy: **Approval**. Background: **on**.
2. In a channel: `@CodeBot the /login route 500s on empty body — dig in and open a PR against acme/api`.
3. It searches, reads the file, analyzes, proposes the PR write (a human approves),
   opens the draft PR via MCP, and posts the link in the thread.

### B. Autonomous PR-follow (event trigger)
1. Agent trigger: **Event → `github.check_run.completed`**. Scope: the channel to
   report in. Tools: `read_repo_file` (optional, to read logs/files on failure).
2. When a PR's CI finishes, the agent runs once with the aggregate result and
   posts, e.g., "✅ CI green on PR #482 (12/12 checks)" or "❌ 2 checks failed on
   #482 — @author take a look", tagging the author.

## The full loop
`@mention or github.issue.opened` → `search_repo_code`/`read_repo_file`/`code_analyze`
→ **MCP open-PR** (approval-gated) → `github.check_run.completed` (CI concluded)
→ agent reacts / re-proposes. Every step reuses the agent runtime — budgets,
per-tool permission re-checks, audit, the durable/async engine — so the loop is
governed end to end.

## Guardrails to set
- **Per-agent + per-channel token caps** so an autonomous coder can't run away.
- **Approval autonomy** for any agent with write (MCP) tools until you trust it.
- **Scope** the agent to specific channels so it only acts where invited.
- The read tools **fail soft**: if GitHub isn't connected they return an
  actionable note, never a hard error.

## Requirements
- A connected GitHub integration (Admin → Integrations) for the read tools + events.
- A GitHub/coding **MCP server** registered for the write (open-PR) capability.
- AI enabled with a capable chat model (weak models propose poor diffs).
