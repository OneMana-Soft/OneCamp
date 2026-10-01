# EU AI Act: this edition ships no AI system

OneCamp is sold as two product lines. **This build is the AI-free one.** It
contains no model provider, no agents, no assistant, and no inference of any
kind: the AI packages are not compiled into it, and a build guard fails if AI
code reaches this line.

That is the whole answer to most of the question, and it is worth stating
plainly rather than burying under a table of mechanisms.

**This is not a compliance statement and not legal advice.** Whether an
obligation applies to you depends on how you deploy and what you build with it,
and the deployer of a self-hosted system carries obligations no vendor can
discharge for them.

## What this means for the Act's AI-specific duties

The transparency obligations that became applicable on 2 August 2026 attach to
providers and deployers **of AI systems**. Article 50's duties to disclose that a
person is interacting with an AI system, and to mark AI-generated content, have
nothing to attach to here: this build produces no AI output to mark and exposes
no AI principal to disclose.

The same goes for the record-keeping duty on automatically generated logs from an
AI system. There is no such system in this build, and therefore no such logs.

If you later move to the edition that includes AI, those duties arise and the
mechanisms that address them are documented there. Do not assume a control
exists here because it exists on that line.

## What this build does do

These are not AI controls. They are the audit and retention properties of the
workspace itself, and they hold on both editions.

| Property | Mechanism | Check it |
|---|---|---|
| Administrative actions are recorded | Who changed what, when, and from where, with authorisation decisions recorded on refusal as well as on action. | `models/postgres/AdminAudit/adminAuditModel.go` |
| The log is tamper-evident | Each entry hashes its own contents plus the previous entry's hash. A verify endpoint recomputes the chain and reports the first divergence; exports carry per-row hashes. | `models/postgres/AdminAudit/adminAuditModel.go` |
| Retention has a floor and does not break the chain | Configurable, refusing a window below the six-month minimum, and it REDACTS rather than deletes so an erasure request does not destroy tamper evidence. Redacted rows are reported separately from verified ones. | `business/AdminAudit/retention.go` |
| Evidence can be taken away | One export carries the log in chain order with per-row hashes, the chain recomputation, the retention policy in effect, and a manifest fingerprinting every section so the pack verifies without trusting the tool that made it. | `business/AdminAudit/evidencePack.go` |
| Data residency | The workspace runs on your infrastructure. There is no vendor in the data path, which is why the above is answerable at all. | `docs/DataSovereignty.md` |

## What is deliberately not claimed

- **Integrity is not completeness.** The chain proves the log was not altered
  after it was written. It does not prove the log recorded everything it should
  have.
- **No certification.** OneCamp is not certified against ISO 42001, SOC 2, or
  anything else.
- **This document describes this build only.** The AI edition's mechanisms are
  documented on that line, and none of them are present here.

---

Verified against the source on 1 September 2026.
