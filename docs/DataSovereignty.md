# Data Sovereignty and Control

What OneCamp guarantees about where your data goes, and the mechanism that
enforces each guarantee. Written for the person who has to sign off on a
deployment, so every claim names the thing that enforces it rather than
asserting good intentions.

Nothing here is a certification. OneCamp is not certified against any scheme,
and this document does not claim it is. What it does is let you answer the
questions an auditor asks, with evidence you can verify in the source.

## The short version

OneCamp runs entirely on infrastructure you control. There is no OneCamp cloud
in the data path, no vendor telemetry, and no shared tenancy. AI is bring your
own key, and an admin can make it structurally impossible for workspace content
to reach a cloud model at all.

## 1. Where the data lives

One deployment, one customer, one server. Postgres, Dgraph, OpenSearch, MinIO,
Redis and the collaboration service all run in your compose stack. There is no
call home: the server does not require outbound connectivity to function, and
nothing about your workspace is reported anywhere.

**Verify it:** the compose files list every service. There is no OneCamp-operated
endpoint among them.

## 2. Where AI inference happens, and how to forbid the cloud

AI is off until an admin configures a provider, and the key is yours.

The important control is `local_only_mode`. When an admin turns it on, the
provider dial guard refuses any non-local model endpoint, and the model resolver
refuses to select a cloud model. It is enforced at the point the connection is
made, not by a policy document, so a misconfigured agent cannot route around it.

**Verify it:** `services/AI/httpguard.go` holds the dial guard;
`services/AI/modelResolver.go` refuses cloud models when the flag is set. Setting
`AI_LOCAL_ONLY_MODE=true` pins it on, and the admin UI will not offer to disable
it, so the guarantee survives a change of workspace administrator.

For installs that permit cloud models but want less exposure,
`pii_redaction_enabled` scrubs detected PII from outbound prompts before they
reach a non-local model. Local endpoints are never redacted, because there is
nothing to protect them from.

## 3. What an AI agent can reach

Agents act as a real user, with that user's access. There is no service account
with a superset of permissions, and no synthetic identity that could surface
content to somebody who should not see it.

- Tool calls resolve resources as the agent's owner, so an agent cannot read
  past a permission boundary its owner could not cross.
- The meeting recap is posted by a real call participant, and the weekly channel
  report by the channel's creator, precisely so the post lands with genuine
  access rather than elevated access.
- Content indexed for retrieval is scoped the same way, so a search cannot
  return something the asker could not open directly.

## 4. Executing untrusted code

Two sandboxes, deliberately different, because they have different needs.

The **analysis sandbox** runs short programs with no network at all, no
credentials, an ephemeral tmpfs, a read-only root filesystem, and OS-level
`setrlimit` caps on CPU, address space and output size. It is off until an admin
points it at a deployed runner.

The **code-PR runner** must reach a git host, so it cannot be egress-less.
Instead it has no direct internet: its traffic is forced through a default-deny
allowlisting proxy that permits only the git host you name. It holds no model
credentials of its own and asks the main server for each completion, so provider
keys never enter the sandbox. A containment smoke test proves the boundary
before the feature is enabled.

**Verify it:** `code-runner-coding-compose.yml` documents the network model, and
`make code_runner_coding_smoke` proves it.

## 5. What is recorded

Every AI action is accountable after the fact:

- A per-run ledger for coding runs, with the outcome, the scope judgement and
  the usage.
- An audit log for administrative changes, recording who changed what.
- Per-workspace, per-channel and per-agent daily token budgets, so spend has a
  ceiling that is enforced rather than monitored.

This matters beyond hygiene. Industry research through 2026 consistently finds
that enterprise agent programmes fail on governance and observability rather
than model quality, and that a majority of deployed agents run with no logging
at all. OneCamp's position is that the ledger is not an add-on tier.

## 6. Where meeting audio goes

Speech to text is the one place where the most sensitive content in a workspace,
a recording of people talking, has to be handed to a model. OneCamp offers three
answers and names the consequence of each on the settings page rather than
leaving it to be inferred from a provider's name.

| Mode | Where the audio goes | Cost |
|---|---|---|
| Self-hosted (bundled) | nowhere. A Whisper server in your own compose stack | none per minute |
| Deepgram / Google | to that vendor | billed per minute |
| OpenAI-compatible | to whatever endpoint you enter | depends who runs it |
| Browser (frontend mode) | nowhere; each browser transcribes locally and posts only text | none |

**The bundled server** is opt-in because it costs memory: `make stt_up` starts it,
and Admin > Transcription > Test confirms it is reachable before you rely on it.
The default `base` model needs about 700 MB of RAM and 145 MB of disk. It
downloads its weights once, and setting `WHISPER_LOCAL_ONLY=1` forbids all
downloads afterwards, which is what an air-gapped install wants.

**Verify it:** the `whisper` service in the compose files publishes no ports and
sits only on the internal stack network, so nothing it holds is reachable from
outside the machine. Its endpoint is a server-side constant, not an
admin-editable field, which is why it is the one endpoint the backend probes
without the outbound-URL guard.

It also refuses every request without a bearer token, including a request to
list its models. That token is the stack's existing `INTERNAL_SECRET`, so there
is nothing extra to generate, and an install with that unset is told so by the
Test button rather than discovering it as a call with no captions.

**Recording is not required.** A transcript used to exist only as part of a
recording, so a team that does not record its meetings could not have written
notes at all. It is now filed against the call itself, and a call nobody
recorded still produces a transcript and a recap. Wanting notes and wanting a
stored video of your colleagues are different decisions, and they are separate
settings now.

**Browser mode keeps its words too, and never sends audio anywhere.** Each
participant's browser transcribes locally and posts only the resulting TEXT,
authenticated as that person: the audio never leaves their machine, and the
server takes the speaker from the session rather than the request, so nobody can
file words under someone else's name or into a call they are not in.

**The limit that remains** is that the browser recognizer only exists in Chrome
and Edge. A participant on Firefox or Safari contributes nothing, so the
transcript has their turns missing with nothing on the page to say so. The
server-side modes have no such gap, and the bundled server is the one that
closes it without sending the audio anywhere either.

## 7. Treating model input as data

Tool results, message content, PR descriptions and issue text are untrusted
input. They are passed to the model as data with an explicit instruction that
they are not instructions, and the agent's memory layer records provenance so a
value injected by content cannot be laundered into a durable fact.

**Verify it:** `services/AI/untrusted.go`.

## 8. The option to have no AI at all

OneCamp ships in two editions. The AI-free edition is not the AI edition with a
switch turned off: the AI packages are not in the build. There are no AI routes
to call and no AI components in the frontend bundle, and a test fails the build
if an AI import reappears.

For an organisation whose policy forbids AI processing outright, this is the
difference between a configuration promise and a structural one.

## What this does not claim

- OneCamp holds no certifications. If your procurement requires SOC 2, ISO 27001
  or a specific national scheme, OneCamp does not have one.
- Regulatory obligations under schemes such as NIS2 fall on you as the operator.
  Running your own infrastructure changes compliance from a negotiation with a
  vendor into a matter of your own configuration, which is a genuine advantage,
  but it does not transfer the obligation to us.
- Encryption at rest for the underlying volumes is the host's responsibility.
  OneCamp encrypts secrets it stores (provider keys, integration tokens) with a
  key-encryption key you supply.

## Questions an auditor will ask, and where the answer is

| Question | Answer |
|---|---|
| Where is our data stored? | Only on your server, in your compose stack |
| Does the vendor have access? | No. There is no vendor endpoint in the data path |
| Can content reach a third-party model? | Only if an admin configures one. `local_only_mode` forbids it at the dial |
| Whose permissions does the AI use? | The requesting user's. There is no elevated service identity |
| Can untrusted code reach the network? | Analysis sandbox: no network. Code runner: allowlisted git host only |
| What is retained about AI use? | A per-run ledger, an audit log, and enforced budgets |
| Can we run with no AI whatsoever? | Yes, and the packages are absent from that build |
