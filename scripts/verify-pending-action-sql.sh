#!/usr/bin/env bash
#
# Verify the ai_pending_actions SQL against a real Postgres, in about ten seconds,
# with no application, no migrations and no test harness.
#
# WHY THIS EXISTS. The claim/settle path is raw SQL against a PARTIAL unique index, and Go
# does not check SQL. Every statement compiled fine and read correctly, and one of them was
# wrong in a way only execution reveals:
#
#   A settled row KEEPS its idempotency key — the unique index is on the key alone, not
#   the key plus a status. So a row left 'failed' by a transient error blocked its own
#   retry forever: the insert conflicts, the state reads "nothing took effect", and the
#   call becomes permanently unrepeatable. The fix (RetakeIdempotencyKey) is guarded to
#   exactly the non-applied states, and getting THAT list wrong would either duplicate an
#   applied write or override a human's refusal.
#
# The ON CONFLICT clause is the other reason. Postgres infers the index from the conflict
# target AND its predicate, so a mismatch with idx_ai_pending_actions_idem is a runtime
# error on the first non-idempotent write — in front of a customer, not in CI.
#
# NOT a replacement for the integration tests. Those exercise the Go code against the real
# schema. This exercises the STATEMENTS, which is the part unit tests cannot reach and the
# part that was wrong. Cheap enough to run on every change to the claim path.
#
# Usage:  ./scripts/verify-pending-action-sql.sh
# Needs:  docker
set -euo pipefail

CONTAINER="oc-pending-sql-check"
IMAGE="postgres:16-alpine"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "==> starting throwaway Postgres"
cleanup
docker run -d --rm --name "$CONTAINER" \
  -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=verify "$IMAGE" >/dev/null

for _ in $(seq 1 60); do
  if docker exec "$CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then break; fi
  sleep 1
done
if ! docker exec "$CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
  echo "!! Postgres did not become ready" >&2
  exit 1
fi

run() {
  local file="$1"
  echo
  echo "==> $file"
  docker cp "$HERE/sql/$file" "$CONTAINER:/tmp/$file" >/dev/null
  # ON_ERROR_STOP is the point: any statement that will not execute fails the script.
  docker exec "$CONTAINER" psql -U postgres -d verify -v ON_ERROR_STOP=1 -f "/tmp/$file"
}

run pending-action-claim.sql
run pending-action-retake.sql

echo
echo "==> every statement executed."
echo "    Read the CASE output above: each one states what it must show."
