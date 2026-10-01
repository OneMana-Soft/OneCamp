# Releasing OneCamp

Two edition lines, one migration sequence, and no channel from us to a running
install. Those three facts decide everything else in this document.

## The two lines

| Line | Branch | Tags | Contains |
|---|---|---|---|
| v2 | `main` | `v2.x.y` | The AI edition |
| v1 | `without-ai` | `v1.x.y` | The AI-free edition |

`beta` is where work lands first and where the beta host runs. Neither release
line is merged from `beta`; changes are cherry-picked onto each line, so the same
change appears on all three branches with different hashes.

Every customer is entitled to both editions. The licence records which version
was current at purchase; it is not a ceiling.

## The migration invariant

**Both lines carry the identical migration set, including migrations for
features only one edition has.**

This is not an accident and it is not overhead. The AI-free edition ships every
AI migration and creates the AI tables; only the Go and TypeScript code is
absent. As of v1.4.7 and v2.5.0 both lines carried the same 143 migrations, with
no differences.

It exists so that switching editions is a code change and never a schema change.
A customer who buys AI later, or an AI customer whose policy changes and who
moves to v1, keeps the same database. Nothing has to be migrated sideways, and no
version arithmetic has to reason about which columns exist.

**So a migration goes to BOTH lines, always, even when only one edition reads the
table it creates.** An AI-only table sitting empty on a v1 install costs nothing.
A divergent sequence costs a customer their upgrade path: numbering drifts, one
line gains a 145 the other does not have, and switching editions then applies
migrations out of order.

If a migration cannot be edition-agnostic, that is a design smell in the
migration, not a reason to split the sequence.

## How a customer updates

Customers pull a version and apply it themselves:

```
make update
```

which does, in order: back up, apply migrations while the current version is
still running, rebuild and restart, then verify. That order matters because the
server refuses to start against a schema older than the build requires, so the
migration has to land before the new binary does.

`make restore` ends the same way for the same reason: replaying an older backup
under a newer build leaves the schema behind, and the next restart fails.

## What we can and cannot control

The installer asks onemana which version is newest for its line, and downloads
it. That is the only direction traffic flows.

**A running install never contacts us.** There is no licence check, no
activation, no heartbeat and no telemetry, by design: it is the guarantee the
product is sold on, and it is what a regulated buyer verifies first.

The consequence is worth stating plainly:

- We control **what is published**. Tagging a release makes it the newest for
  that line, and every install that checks will see it.
- We do **not** control **what is installed**. We cannot push a version, pin a
  customer to one, force an upgrade, or read what any workspace is running.
- Nothing about a customer's version is known to us unless they tell us.

Anything that changes this trades away the central promise. A version-reporting
heartbeat is still a heartbeat: the honest way to describe it would be "the
product contacts the vendor", and that sentence loses the deals the architecture
was built to win. If fleet visibility becomes necessary, it belongs in the hosted
Cloud offering, where we own the machine and can say so, not in the self-hosted
product.

## Cutting a release

1. Confirm `beta` is green on the full CI suite.
2. Cherry-pick onto `main`, including **every** migration.
3. Cherry-pick onto `without-ai`, including **every migration**, and excluding
   only code that imports an AI package. Frontend changes to shared components go
   across hunk by hunk: a whole-commit cherry-pick can drag an AI import along
   and the AI-free guard will fail the build, which is the guard working.
4. Run CI on both lines.
5. Tag `v2.x.y` and `v1.x.y`.
6. Say in the notes what changes behaviour on upgrade. A setting that becomes
   opt-in stops working for anyone who had it on, and they will find out on a
   Monday if nobody wrote it down.

## Publishing the open-source release

The server is open source at https://github.com/OneMana-Soft/OneCamp under
AGPL-3.0, with a commercial licence beside it. Each release is published there
as one commit, never with this repository's history, which held credentials
before anything checked for them.

After tagging a release:

```
scripts/publish-public.sh v2.x.y           # builds and checks the export
scripts/publish-public.sh v2.x.y --push    # publishes it (main for v2, without-ai for v1)
```

The script removes everything listed in `public-repo/EXCLUDE`, adds the public
README, licence and contribution terms from `public-repo/`, and refuses to
publish if gitleaks finds a secret or the export names a login, a home path or
a person's address. Add a path to `EXCLUDE` before it is ever tagged, not after.
