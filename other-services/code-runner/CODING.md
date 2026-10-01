# Code-PR coding runner — deploy & containment

The coding runner is the isolated sidecar that turns an `@mention` / task
assignment into a **verified pull request**: it clones a repo, runs a
model-driven edit/verify loop, and pushes a fresh branch. It is the ONLY
component that executes untrusted repo code, so it is contained in layers and is
**off until you deploy it and enable the feature**.

## What makes it different from the python sandbox

| | python sandbox (`code-runner`) | coding runner (`code-runner-coding`) |
|---|---|---|
| Endpoint | `POST /run` | `POST /code-run` |
| Toolchains | python + data libs | git + Go + Node |
| Network | **egress-less** (`internal: true`) | **git-host-egress-only** (via allowlist proxy) |
| Model | n/a | main server's internal LLM proxy (no internet) |

The coding runner needs the git host, so it can't be fully egress-less. Instead
it has **no direct internet**; its git traffic is forced through an
**allowlisting proxy** (`egress-proxy`, default-deny) that permits only the
hosts in `egress-proxy/filter-allowlist.txt`. The model is reached over the
internal network via the main server's `/internal/code-run/llm` proxy.

## Deploy

1. Build + start the profile (alongside the main stack):
   ```
   make code_runner_coding_up
   # = docker compose -f final-compose.yml -f code-runner-coding-compose.yml \
   #     --profile coding up -d --build
   ```
2. **Prove containment BEFORE enabling** (gates a deploy; exits non-zero on any
   leak):
   ```
   make code_runner_coding_smoke
   ```
   Asserts: the runner answers `/healthz`; the git host is reachable through the
   proxy; a non-allowlisted host is **refused**; there is **no direct internet**.
3. (Recommended) run the handler's own safety suite (needs git + go):
   ```
   make code_runner_coding_test   # go test -tags coderun_integration
   ```
4. In **Admin → AI → Code PRs**: set the runner URL to
   `http://code-runner-coding:9099`, set the shared token (`CODE_RUNNER_TOKEN`,
   matching the compose env), then use **Test runner** to confirm reachability
   and enable. That's the whole admin setup.

   The runner reaches the model through the main server's internal LLM proxy
   (`POST /internal/code-run/llm`). You do **not** normally set this by hand: the
   server defaults to its conventional in-cluster address
   `http://go-service:3000/internal/code-run/llm` (the same on every shipped
   compose file). Only if your topology differs (a different service name/port,
   or the server runs outside the runner's network) set `AI_CODE_PR_LLM_PROXY_URL`
   in the go-service `.env` to the address the runner can reach it at. A wrong or
   unreachable value shows up as a `status=unavailable` coding run.

## Hardening (in `code-runner-coding-compose.yml`)

- non-root (`10001`), `cap_drop: ALL`, `no-new-privileges`, read-only rootfs
- size-capped tmpfs `/work` (the entire checkout + build scratch; wiped per run)
- `pids_limit`, `mem_limit`, `cpus`, per-run wall/CPU/output caps in the handler
- **gVisor recommended** for untrusted code — uncomment `runtime: runsc` once
  the host has it installed
- no published ports (reachable only on the internal network)

## Adding a self-hosted git host or a registry

Add an **anchored** regex to `egress-proxy/filter-allowlist.txt` (e.g.
`^git\.acme\.internal$`), keep it as tight as possible, and re-run
`make code_runner_coding_smoke`. The allowlist is the only internet the runner —
and the untrusted code it executes — can reach.
