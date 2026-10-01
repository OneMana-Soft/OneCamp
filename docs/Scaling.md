# Scaling

## What OneCamp runs as today

**A single `go-service` process.** The shipped compose declares no replicas, and
no deployment has run two. Everything below describes what that means and what
it would take to change, so nobody has to re-derive it from the source, and so
the claim on the sales page and the claim here stay the same claim.

**It has not been load tested.** The sizing table on the website gives hardware
guidance by registered user count. Those are not measured concurrency figures,
and they should not be read as any. For comparison, Mattermost publishes tested
reference architectures at 2,000, 15,000, 30,000, 80,000, 100,000 and 200,000
concurrent users, with the largest verified at 100,000 concurrent on a six-node
cluster behind one database writer and four readers. OneCamp has no equivalent
number because nobody has produced one.

Saying so is deliberate. A capacity figure a customer plans around and then
discovers is wrong costs more than not having published one.

## Why a second node is closer than it looks

The expensive part of clustering a chat product is getting state out of the
process, and that was designed in from the start rather than deferred:

| Concern | Where it lives | Consequence for a second node |
|---|---|---|
| Realtime fan-out | EMQX, an external broker | Nodes do not need to know about each other's clients |
| Sessions, caches, AI history | Redis | A request may land anywhere |
| Webhook rate limiting | Redis sliding window, in-process only as a fallback when Redis is down | The published limit stays the published limit |
| Scheduled agent runs | `ClaimDueScheduledRun`, an atomic claim in Postgres | A routine fires once, not once per node |
| Search | OpenSearch | Shared index |
| Files | MinIO | Shared object store |

This is the opposite of the usual self-hosted chat story, where realtime is held
in the web process and clustering means a rewrite.

`helpers/horizontalScaleGuard_test.go` pins those four properties. It does not
claim multi-node works; it claims the changes that would make it impossible have
not been made.

## What is actually in the way

**1. Nothing has ever run two.** Untested is untested. The list below is derived
from reading, not from load.

**2. In-process caches that are safe but unproven.** There are roughly fifty
package-level mutable variables across `business/` and `services/`. The ones
inspected are caches whose failure mode is duplicated work rather than wrong
answers, and several say so explicitly (`linkedSessions` in `business/LiveKit`
notes that losing it costs one redundant idempotent upsert per live call). They
have not all been audited.

**3. Import OAuth token refresh.** `business/Import/provider/oauth.go` keeps one
`oauth2.TokenSource` per provider and owner in a `sync.Map`, and persists
refreshed tokens to `import_oauth_tokens`. Two nodes refreshing the same token
concurrently is safe for providers that keep refresh tokens stable and a hazard
for providers that rotate them. Narrow, because it only applies during an
import, but real.

**4. The compose file.** `distribute-compose.yml` declares one `go-service` and
no load balancer in front of it. Traefik is there and would do the job, but
nothing configures it to.

## What to do before claiming a number

In this order, because each step makes the next one meaningful:

1. **Measure one node.** Find where a single process actually degrades, with a
   realistic corpus rather than an empty database. Mattermost's harness populates
   100 million posts, 200,000 users and 720,000 channels for exactly this reason:
   an empty database answers a different question.
2. **Publish what you measured**, in the sizing table, as a concurrency figure
   rather than a registered-user figure.
3. **Only then consider a second node.** Two nodes are worth building when one
   node is measurably the constraint for a real customer, and not before. Until
   then the work has no way to be validated and no user to be right for.

## What this does not cover

Database scaling. Postgres is a single writer here, and read replicas, connection
pooling and partitioning are separate questions that only start to matter well
past the point where the web tier does.
