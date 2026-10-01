# AI Agent Code Sandbox

How OneCamp lets an AI teammate **run bounded data analysis and render charts**
by executing short Python programs in an isolated, network-less **code-runner**
sidecar — and how an operator turns it on, secures it, and keeps it bounded.

The feature is **OFF by default** and completely inert until an admin points it
at a deployed runner. Nothing in the main server ever executes untrusted code
itself.

## What the agent can do with it

The sandbox exposes one read-only tool, `run_analysis`. Given a short Python
program (and optionally one or more permission-checked table inputs), it:

- runs the code with **no network**, **no credentials**, and an **ephemeral
  filesystem**, under hard OS-level CPU/memory/time/output limits;
- returns the program's stdout plus any artifacts it produced — a `chart` spec
  is rendered inline as an SVG chart (same renderer as the ` ```chart ` block),
  and any other file is noted by name;
- is metered against per-workspace / per-channel / per-agent daily budgets.

Table inputs are resolved **as the agent owner**: every table the code can read
is permission-checked exactly like `read_table` / `query_table`. Data is handed
to the sandbox as read-only CSV files under a relative `data/` directory; the
code sees nothing else.

## Trust boundary

```
┌────────────── main server (trusted) ──────────────┐      ┌── code-runner (isolated) ──┐
│ resolve permission-checked inputs → CSV            │      │ no network                 │
│ gate admin switch + daily budgets                  │ HTTP │ ephemeral fs (tmpfs)       │
│ assemble a self-contained Job (code + files +      │─────▶│ read-only rootfs           │
│   limits — NO tokens, NO network refs)             │      │ setrlimit CPU/AS/FSIZE/... │
│ classify returned artifacts, audit every run       │◀─────│ run python3 bootstrap.py   │
└────────────────────────────────────────────────────┘      └────────────────────────────┘
```

The server side only resolves inputs, gates budgets, submits the Job, classifies
the result, and audits. The runner is a disposable container that actually runs
the code. The two talk over an internal HTTP API authenticated with a shared
token; the server never sends credentials or network references into a Job.

## Turning it on

1. **Deploy the runner.** Build and run the `other-services/code-runner` image
   (defined in `final-compose.yml` under the `code-execution` profile; bring it
   up with `make code_runner_up`). It runs on an **egress-less internal
   network**, with a **read-only rootfs**, a small **tmpfs** workdir, all Linux
   capabilities dropped, and (recommended) a **gVisor** (`runsc`) runtime for a
   second isolation layer. Pin the image by digest.
2. **Configure it.** Admin → AI models → **Code analysis sandbox**:
   - **Runner URL** — the sidecar's `/run` endpoint (must be an absolute
     `http(s)` URL, reachable only from the server).
   - **Runner token** — the shared auth token (stored encrypted, write-only;
     leave blank to keep the stored one).
   - **Image digest** — optional, pins the runner image for auditability.
   - **Daily budgets** — workspace/channel caps in runner-seconds and run count
     (`0` = unlimited). Per-agent caps live on each agent.
3. **Self-test.** Click **Run sample analysis**. It submits a trivial probe to
   the runner (no table inputs) and reports reachability / auth / execution —
   this works *before* you enable the sandbox, so you can validate the
   deployment first.
4. **Enable.** Flip the section's switch on and **Save**. Enable `run_analysis`
   on the specific agents that should have it (Admin → Agents → edit → tools).

A new binary requires **migration 119** to be applied first (adds the sandbox
settings columns, the per-agent caps, and the `sandbox_runs` ledger).

## Limits & budgets

Per-run limits are clamped by `codesandbox.ClampLimits` and can never exceed the
safety ceilings baked into the server (wall ≤ 5m, CPU ≤ 4m, memory ≤ 2 GiB,
≤ 512 processes, ≤ 32 MiB output, ≤ 32 artifacts). Daily budgets are clamped to
≤ 24h of runner-seconds and ≤ 100k runs per tier. When a tier's daily cap is
hit, `run_analysis` refuses cleanly with a typed reason (the agent gets an
actionable message; nothing crashes).

**Used today** (workspace runner-seconds + run count) is shown live in the admin
card so you can watch spend against the caps.

## Kill switch

The **Disable now** button (shown whenever the sandbox is currently enabled)
calls the dedicated kill-switch endpoint and turns the sandbox off **instantly**
without touching the runner URL, token, or budgets. Use it if a runner
misbehaves; re-enable with the section switch when you're ready.

## Auditing

Every run — success, failure, or budget refusal — writes a `sandbox_runs` row:
the acting user, a SHA-256 of the executed code, the resolved input references
(ids/queries, never raw data), the outcome status, and measured usage (wall/CPU
ms, peak memory, artifact count). Because `run_analysis` is a normal agent tool,
the executed code and its result also appear in the agent's run transcript
alongside every other tool call.

## Failure modes (all safe)

| Situation | Behavior |
|---|---|
| Sandbox disabled / no runner URL | `run_analysis` returns "sandbox unavailable"; nothing runs |
| Runner unreachable / auth fails | Self-test / run reports it; the agent gets a clean failure message |
| Code raises / non-zero exit | Returns a sanitized error (host paths stripped, user frames only) |
| Wall / CPU / output / artifact cap hit | Killed with a typed status (`timeout`, `killed_limit`, `oom`) |
| Daily budget hit | Refused with a typed budget reason; resumes after the daily reset |
| Unauthorized table input | Whole run fails with "you don't have access to that table" |
