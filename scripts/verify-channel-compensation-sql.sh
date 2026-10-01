#!/usr/bin/env bash
#
# Verify the channel compensation SQL against a real Postgres, in about ten seconds, with no
# application and no test harness.
#
# WHY THIS EXISTS. The bug being fixed was invisible to the compiler and to every unit test:
# CreateChannel inserted a channel row, and when the Dgraph write failed it logged and returned,
# leaving the row behind. channels.ch_name is UNIQUE and every channel listing reads Dgraph, so
# that row was a channel nobody could see which still owned the name — and each retry failed on
# the constraint. One transient error cost the user that name permanently.
#
# The fix therefore rests on claims about SQL behaviour, and Go does not check SQL. Four of them
# only execution can settle:
#
#   1. A SOFT delete does NOT free the name. This is why the compensation is a hard delete, and
#      if it were untrue the whole fix would be unnecessary. Asserted first, because it is the
#      premise.
#   2. A HARD delete DOES free the name, so the retry that used to fail now succeeds.
#   3. The hard delete is REFUSED once the channel has children. channels(id) is referenced by
#      posts, webhooks, last_seen_channel and users_channel_notification with no ON DELETE, so
#      this must fail rather than cascade — the compensation window closes before those rows are
#      written, and if it ever ran later it must not destroy a live channel's data.
#   4. The restore path writes deleted_at = NULL and not a zero timestamp. A Go time.Time zero
#      value written as a literal would archive a live channel while undoing a failed rename,
#      which is worse than the divergence the compensation exists to fix.
#
# Usage:  ./scripts/verify-channel-compensation-sql.sh
# Needs:  docker
set -euo pipefail

CONTAINER="oc-channel-compensation-sql-check"
IMAGE="postgres:16-alpine"
DB="chcheck"

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "==> starting $IMAGE"
docker run -d --name "$CONTAINER" \
  -e POSTGRES_PASSWORD=pw -e POSTGRES_DB="$DB" \
  "$IMAGE" >/dev/null

for _ in $(seq 1 40); do
  if docker exec "$CONTAINER" pg_isready -U postgres -d "$DB" >/dev/null 2>&1; then break; fi
  sleep 0.5
done

psql() { docker exec -i "$CONTAINER" psql -v ON_ERROR_STOP=1 -qtA -U postgres -d "$DB" "$@"; }

echo "==> schema (the real shape: migrations 2, 3 and 6)"
psql <<'SQL' >/dev/null
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE users (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4()
);

-- Exactly migration 2. ch_name is a plain table-level UNIQUE, never altered since.
CREATE TABLE channels (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "ch_name" varchar NOT NULL UNIQUE,
    "ch_private" boolean NOT NULL,
    "created_by" uuid REFERENCES users(id),
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);

-- A child table with no ON DELETE, as migration 6 declares it.
CREATE TABLE last_seen_channel (
    "channel_id" uuid REFERENCES channels(id) NOT NULL
);

INSERT INTO users (id) VALUES ('11111111-1111-1111-1111-111111111111');
SQL

fail() { echo "FAIL: $1"; exit 1; }

# ---------------------------------------------------------------------------
echo "==> 1. a SOFT delete does NOT free the name (the premise of the fix)"
# ---------------------------------------------------------------------------
psql <<'SQL' >/dev/null
INSERT INTO channels (id, ch_name, ch_private, created_by)
VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'general', false,
        '11111111-1111-1111-1111-111111111111');
-- The soft delete the product uses everywhere else.
UPDATE channels SET deleted_at = NOW()
 WHERE id = 'aaaaaaaa-0000-0000-0000-000000000001';
SQL

if psql -c "INSERT INTO channels (id, ch_name, ch_private, created_by)
            VALUES ('aaaaaaaa-0000-0000-0000-000000000002','general',false,
                    '11111111-1111-1111-1111-111111111111');" >/dev/null 2>&1; then
  fail "a soft-deleted row did NOT reserve the name — the hard delete would be unnecessary"
fi
echo "    ok: the name stays reserved, so soft delete cannot serve as compensation"

# ---------------------------------------------------------------------------
echo "==> 2. a HARD delete DOES free the name (the retry now succeeds)"
# ---------------------------------------------------------------------------
psql -c "DELETE FROM channels WHERE id = 'aaaaaaaa-0000-0000-0000-000000000001';" >/dev/null

psql -c "INSERT INTO channels (id, ch_name, ch_private, created_by)
         VALUES ('aaaaaaaa-0000-0000-0000-000000000002','general',false,
                 '11111111-1111-1111-1111-111111111111');" >/dev/null \
  || fail "the name was still reserved after a hard delete — the fix does not work"
echo "    ok: the user can create 'general' again"

# ---------------------------------------------------------------------------
echo "==> 3. the hard delete is REFUSED once the channel has children"
# ---------------------------------------------------------------------------
psql -c "INSERT INTO last_seen_channel (channel_id)
         VALUES ('aaaaaaaa-0000-0000-0000-000000000002');" >/dev/null

if psql -c "DELETE FROM channels WHERE id = 'aaaaaaaa-0000-0000-0000-000000000002';" >/dev/null 2>&1; then
  fail "the delete cascaded or was allowed with a child row — a late compensation could destroy a live channel"
fi
echo "    ok: the foreign key refuses it, so this can only reverse a childless create"

# ---------------------------------------------------------------------------
echo "==> 4. the restore writes NULL, not a zero timestamp"
# ---------------------------------------------------------------------------
psql <<'SQL' >/dev/null
INSERT INTO channels (id, ch_name, ch_private, created_by, deleted_at)
VALUES ('bbbbbbbb-0000-0000-0000-000000000001', 'renamed', true,
        '11111111-1111-1111-1111-111111111111', NOW());
-- The restore statement, with a NULL parameter, as RestoreChannelRow issues it.
UPDATE channels
   SET ch_name = 'original', ch_private = false, deleted_at = NULL, updated_at = NOW()
 WHERE id = 'bbbbbbbb-0000-0000-0000-000000000001';
SQL

got=$(psql -c "SELECT COALESCE(deleted_at::text,'NULL') || '|' || ch_name || '|' || ch_private
               FROM channels WHERE id = 'bbbbbbbb-0000-0000-0000-000000000001';")
[ "$got" = "NULL|original|false" ] \
  || fail "restore produced '$got', want 'NULL|original|false'"
echo "    ok: an unarchived channel is restored unarchived"

# And the shape the Go zero time would produce, to show it is a different outcome.
psql -c "UPDATE channels SET deleted_at = '0001-01-01 00:00:00+00'
         WHERE id = 'bbbbbbbb-0000-0000-0000-000000000001';" >/dev/null
zero=$(psql -c "SELECT deleted_at IS NULL FROM channels
                WHERE id = 'bbbbbbbb-0000-0000-0000-000000000001';")
[ "$zero" = "f" ] \
  || fail "a zero timestamp read back as NULL, so the distinction this guards would not matter"
echo "    ok: a zero timestamp is NOT null — writing one would archive a live channel"

echo
echo "PASS: channel compensation SQL behaves as the fix assumes"
