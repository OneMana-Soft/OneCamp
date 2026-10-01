# Agent controls: the procurement questions, and where each answer lives

Enterprise agent procurement has converged on a short list of questions. This
maps each to the control that answers it and the file where you can check the
claim yourself, because OneCamp is open source and a claim you can read the code
for is worth more than one you cannot.

**This is not a compliance statement and not legal advice.** OneCamp is not
certified against anything. What this document does is let you answer an
auditor's questions with evidence from the source.

Two things are worth knowing before the table. First, several of these controls
have existed for a long time under names nobody searches for, which is why a
procurement scan concluded they were missing. Second, every row below was
verified against the source on the date in the footer, not written from memory.

## Identity and authority

| The question | The control | Check it |
|---|---|---|
| What identity does an agent act under? | An agent acts **as its owner**, never as a super-user. There is no agent principal with its own standing permissions to escalate. | `migrations/89_create_ai_agents.up.sql` (`created_by`) |
| Are permissions checked once or per action? | **Per tool call**, at execution time, against the owner's rights at that moment. Revoking a person's access revokes it for the agents acting as them. | `business/AIAgent/agentRunner.go` |
| Can an agent reach anything in the workspace? | No. `scope` bounds it to named channels and projects, and `enabled_tools` is an **allow-list**: the runner may call only what is listed, and each call is still permission-checked. | `models/postgres/AIAgent/aiAgentModel.go` |
| How do you tell an agent's work from a person's? | Every bot has a kind (`assistant`, `agent`, `automation`, `bot`), and generated content is marked in the content itself so the marking survives an export. | `domain/User/botKind.go`, `business/AI/meetingRecapAgent.go` |

## Stopping and bounding

| The question | The control | Check it |
|---|---|---|
| How is the kill switch operated? | `is_active` on the agent. Disabled agents are skipped by every trigger and kept intact for inspection, so stopping one does not destroy the evidence of what it did. | `models/postgres/AIAgent/aiAgentModel.go` |
| What stops a runaway loop? | `max_steps` per run, hard-capped in the runner regardless of configuration, plus a wall-clock deadline per run. | `business/AIAgent/agentRunner.go` |
| What stops runaway spend? | Per-agent daily token budgets, and a workspace cap on top. An agent's spend is billed to its own budget rather than its owner's interactive quota, so one busy agent cannot exhaust a person's allowance. | `models/postgres/AIAgent/aiAgentModel.go` (`max_daily_tokens`) |
| What bounds code execution? | Per-agent daily sandbox seconds and run counts, enforced at the executor. | `models/postgres/AIAgent/aiAgentModel.go` (`sandbox_daily_*`) |

## Human oversight

| The question | The control | Check it |
|---|---|---|
| Is there an approval gate for high-stakes actions? | Yes. Autonomy levels decide whether a write executes or becomes a **pending action** a person approves, and a "plan" run proposes its whole set of writes as one approval rather than a drip of them. | `business/AIAgent/agentRunner.go` (`CreatePendingAction`) |
| Who proposed the thing I am approving? | Every pending action records the agent and the run that produced it, so approving one is a decision about a known proposal rather than an anonymous one. | `migrations/147_pending_action_agent_attribution.up.sql` |
| How do you know an agent's work was any good? | Each run records what people did with it: kept, edited, or ignored. "Nobody has looked yet" is kept distinct from "nobody accepted it", so the number cannot flatter or damn an agent by accident. | `business/AIAgent/agentOutcome.go` |

## Truthfulness of the record

| The question | The control | Check it |
|---|---|---|
| What if the agent says it did something it did not do? | Two deterministic gates, both reading the run's execution ledger rather than the model's prose, with no extra model call. One fires when a write was attempted and errored while the draft claims success. The other fires when the draft claims a change and **no write tool succeeded at all**, which is the more common failure and had no check until recently. | `business/AIAgent/agentVerifyClaims.go` |
| Can I reconstruct what an agent was told? | Each run records the model that read the prompt, a fingerprint of the fully composed instructions, and which skills were in them with a fingerprint of each. An instruction edited later cannot silently change what a past run appears to have been asked. | `migrations/152_agent_run_provenance.up.sql` |
| Is the log tamper-evident? | Each entry hashes its own contents plus the previous entry's hash. A verify endpoint recomputes the chain and reports the first divergence. Exports carry per-row hashes. | `models/postgres/AdminAudit/adminAuditModel.go` |
| Are refusals recorded, or only actions? | Both, with one fixed word per outcome (`allowed` / `refused`) so a reviewer can query for them, and an allowed decision carries the resource it was authorised for. | `business/MCPServer/audit.go` |
| Did a person ask for this run, or did it fire on its own? | The audit entry for a run records its trigger, the tools that succeeded and failed, and the refusals policy issued during it, so the run's own governance outcome is in the reviewable record and not only in the run transcript. | `business/AIAgent/agentAudit.go` |

## Change safety

| The question | The control | Check it |
|---|---|---|
| What happens when someone edits a shared instruction? | The library shows how many agents use it **before** the edit, stores why it changed with the version, keeps a history, and reverts. A revert is written as a new revision rather than by deleting what it undid. | `business/AIAgent/agentSkillBusiness.go` |
| Does anything re-test an agent after it changes? | Yes, automatically, in proportion to edits rather than to time. Editing a shared skill also marks every agent that composes it, which is the change most likely to break an agent its author was not thinking about. | `business/AIAgent/agentEvalWatch.go` |
| How do you detect that a change made things worse? | By which scenarios **changed verdict**, not by the average. A suite that fixes one case and breaks another has an identical pass rate, and an aggregate reports nothing. | `business/AIAgent/agentEvalFlips.go` |
| Does the system change itself? | No. Scoring is one thing and acting on a score is another; only the first runs in a background loop. Every change to an agent is a human decision. | `business/AIAgent/agentEvalWatch.go` |

## Data, residency and retention

| The question | The control | Check it |
|---|---|---|
| Where does our data go? | Nowhere. OneCamp runs on your infrastructure with your model keys. There is no vendor in the data path, which is why the questions above are answerable at all. | `docs/DataSovereignty.md` |
| Can we run this with no AI whatsoever? | Yes, as a maintained product line rather than a feature flag, with build guards that fail when AI code reaches it. | `helpers/features.go` |
| How long is data kept? | Configurable, with a six-month floor that a shorter setting cannot go below by accident. Retention **redacts rather than deletes**, so an erasure request does not break the hash chain, and redacted rows are reported separately from verified ones. | `business/AdminAudit/retention.go` |
| Where does one person's data live? | There is an inventory per person, which is the question you cannot answer with a search box and which arrives with a deadline attached. | `business/DataSubject/` |
| Can an auditor replay a specific action? | There is one export carrying the log in chain order with per-row hashes, the chain recomputation, the runs with their instruction fingerprints, and a manifest fingerprinting every section so the pack verifies without trusting the tool that made it. | `business/AdminAudit/evidencePack.go` |

## Getting this into your own tooling

The controls above are worth little to a team that has to open OneCamp to see
them. An organisation running agents across several systems watches one place,
and a governance signal that lives somewhere else is a governance signal nobody
reads.

| The question | The control | Check it |
|---|---|---|
| Can agent activity reach our observability stack? | Every finished run is emitted as an OpenTelemetry span alongside the audit entry: which agent, what triggered it, how long it took, which tools succeeded and failed, and what policy refused. Set `OTEL_EXPORTER_OTLP_ENDPOINT` and it goes to your collector; leave it unset and nothing is exported. | `business/AIAgent/agentTelemetry.go` |
| Will it fit the dashboards we have? | The attributes with an OpenTelemetry standard use the standard name (`gen_ai.operation.name`, `gen_ai.agent.id`, `gen_ai.agent.name`). The GenAI agent conventions are still experimental, and there is no standard at all for "policy refused this", so everything without one is namespaced `onecamp.*` rather than squatting on a key the specification may later define differently. | `business/AIAgent/agentTelemetry_test.go` |
| Will a refusal page our on-call? | No. A run blocked by governance has span status **Ok**, because a refusal is the product working. Only a genuinely failed run is `Error`. A stopped run is Ok too: someone pressed stop. | `business/AIAgent/agentTelemetry_test.go` |

The span carries the run id, not the transcript. What the agent was asked and
what it said stays in OneCamp, on your own infrastructure, and the export
answers "what happened, and was anything refused" without shipping the contents
of your workspace to whoever operates the collector.

## What is deliberately not claimed

- **Integrity is not completeness.** The chain proves the log was not altered after
  it was written. It does not prove the log recorded everything it should have,
  and no export converts one property into the other.
- **No bit-exact replay.** Provenance proves what was sent. Reproducing an
  identical model response across providers and routing is not something any
  honest system promises.
- **No certification.** OneCamp is not certified against ISO 42001, SOC 2, or
  anything else. These are mechanisms you can inspect, not attestations.
- **Fire-and-forget where it says so.** The pre-action audit guarantee, where the
  entry is written before the action and the call aborts if it cannot be, applies
  to MCP tool calls. Administrative audit writes are best-effort.

---

Verified against the source on 6 September 2026. If a row here is wrong, the file
named beside it is the thing to check, and a correction is worth more to us than
the claim was.
