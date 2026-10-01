#!/usr/bin/env bash
#
# Verify the user compensation SQL against a real Postgres, in about ten seconds, with no
# application and no test harness.
#
# WHY A SEPARATE SCRIPT FROM THE CHANNEL ONE. The channel script was not extended to teams and
# projects because those tables have the identical shape it already proves — a plain UNIQUE
# varchar and foreign keys with no ON DELETE. users does NOT. It is referenced by 55 tables with a
# MIX of ON DELETE CASCADE, ON DELETE SET NULL and no clause at all, and it carries TWO unique
# columns rather than one. That difference is the risk in this change, and it is exactly the kind
# of thing reading the schema does not settle.
#
# Five claims, each of which only execution can settle:
#
#   1. An orphaned row reserves email_id, so the person cannot sign up again. This is the bug.
#   2. It reserves username too — the second unique column, easy to forget.
#   3. A hard delete frees both, so the retry succeeds.
#   4. THE DANGEROUS ONE: with an ON DELETE CASCADE child present, the delete SUCCEEDS and takes
#      the child with it. That is why the compensation is confined to the window before any child
#      exists, and why this must never be reused as a general "delete user" routine.
#   5. With a RESTRICT child present it is refused outright — so in the mixed case the outcome
#      depends on which table has rows, which is the strongest possible argument for the window.
#
# Also asserts the naming assumption domain.uniqueViolationTargets depends on: that Postgres names
# a unique constraint "<table>_<column>_key", so matching the column against pqErr.Constraint is
# sound. If that ever stops holding, the username-collision retry loop silently stops retrying.
#
# Usage:  ./scripts/verify-user-compensation-sql.sh
# Needs:  docker
set -euo pipefail

CONTAINER="oc-user-compensation-sql-check"
IMAGE="postgres:16-alpine"
DB="usercheck"

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
fail() { echo "FAIL: $1"; exit 1; }

echo "==> schema (users as migrations 1 and 22 declare it, plus one child of each FK flavour)"
psql <<'SQL' >/dev/null
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- Migration 1 plus migration 22's ALTER. Both unique columns, as shipped.
CREATE TABLE users (
    "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    "email_id" varchar NOT NULL UNIQUE,
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);
ALTER TABLE users ADD COLUMN IF NOT EXISTS "username" varchar UNIQUE;

-- A child that CASCADES, as agent_skills and agent_eval do.
CREATE TABLE agent_skills (
    "id" uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "created_by" uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE
);

-- A child with no ON DELETE clause, and therefore RESTRICT, as most of the 55 are.
CREATE TABLE users_channel_notification (
    "user_id" uuid REFERENCES users(id) NOT NULL
);
SQL

# ---------------------------------------------------------------------------
echo "==> 1. an orphaned row reserves email_id (this is the bug)"
# ---------------------------------------------------------------------------
psql <<'SQL' >/dev/null
INSERT INTO users (id, email_id, username)
VALUES ('aaaaaaaa-0000-0000-0000-000000000001', 'alice@example.com', 'alice');
SQL

if psql -c "INSERT INTO users (id, email_id, username)
            VALUES ('aaaaaaaa-0000-0000-0000-000000000002','alice@example.com','alice2');" >/dev/null 2>&1; then
  fail "email_id was not reserved — the lockout this fix prevents would not occur"
fi
echo "    ok: alice cannot sign up again while the orphan exists"

# ---------------------------------------------------------------------------
echo "==> 2. it reserves username as well (the second unique column)"
# ---------------------------------------------------------------------------
if psql -c "INSERT INTO users (id, email_id, username)
            VALUES ('aaaaaaaa-0000-0000-0000-000000000003','other@example.com','alice');" >/dev/null 2>&1; then
  fail "username was not reserved — a soft delete would still strand the handle"
fi
echo "    ok: the handle is held too, so both must be freed"

# ---------------------------------------------------------------------------
echo "==> 3. the naming assumption uniqueViolationTargets relies on"
# ---------------------------------------------------------------------------
email_con=$(psql -c "SELECT conname FROM pg_constraint
                     WHERE conrelid='users'::regclass AND contype='u'
                       AND pg_get_constraintdef(oid) LIKE '%email_id%';")
user_con=$(psql -c "SELECT conname FROM pg_constraint
                    WHERE conrelid='users'::regclass AND contype='u'
                      AND pg_get_constraintdef(oid) LIKE '%username%';")
case "$email_con" in *email_id*) ;; *) fail "email constraint is named '$email_con'; the predicate matches on the column name appearing in it";; esac
case "$user_con"  in *username*) ;; *) fail "username constraint is named '$user_con'; the predicate matches on the column name appearing in it";; esac
echo "    ok: '$email_con' and '$user_con' both contain their column, so matching on Constraint is sound"

# ---------------------------------------------------------------------------
echo "==> 4. a hard delete frees both, so the retry succeeds"
# ---------------------------------------------------------------------------
psql -c "DELETE FROM users WHERE id = 'aaaaaaaa-0000-0000-0000-000000000001';" >/dev/null

psql -c "INSERT INTO users (id, email_id, username)
         VALUES ('aaaaaaaa-0000-0000-0000-000000000004','alice@example.com','alice');" >/dev/null \
  || fail "the address was still reserved after a hard delete — the fix does not work"
echo "    ok: alice can sign up again"

# ---------------------------------------------------------------------------
echo "==> 5. THE DANGEROUS ONE: a CASCADE child is destroyed, not protected"
# ---------------------------------------------------------------------------
psql -c "INSERT INTO agent_skills (created_by)
         VALUES ('aaaaaaaa-0000-0000-0000-000000000004');" >/dev/null

psql -c "DELETE FROM users WHERE id = 'aaaaaaaa-0000-0000-0000-000000000004';" >/dev/null \
  || fail "expected the delete to be ALLOWED and to cascade; if it is refused, revisit the warning in models.HardDeleteUser"

remaining=$(psql -c "SELECT count(*) FROM agent_skills;")
[ "$remaining" = "0" ] \
  || fail "expected the cascade to remove the child row, got $remaining remaining"
echo "    ok: it cascaded and took the skill with it — this is why it is window-only"

# ---------------------------------------------------------------------------
echo "==> 6. a RESTRICT child refuses it outright"
# ---------------------------------------------------------------------------
psql <<'SQL' >/dev/null
INSERT INTO users (id, email_id, username)
VALUES ('bbbbbbbb-0000-0000-0000-000000000001', 'bob@example.com', 'bob');
INSERT INTO users_channel_notification (user_id)
VALUES ('bbbbbbbb-0000-0000-0000-000000000001');
SQL

if psql -c "DELETE FROM users WHERE id = 'bbbbbbbb-0000-0000-0000-000000000001';" >/dev/null 2>&1; then
  fail "a RESTRICT child did not block the delete"
fi
echo "    ok: refused — so with children the outcome depends on which table has rows"

echo
echo "PASS: user compensation SQL behaves as the fix assumes, including the cascade hazard"
