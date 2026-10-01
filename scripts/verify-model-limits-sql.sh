#!/usr/bin/env bash
#
# Verify migration 140 (per-model token limits) against a real Postgres, in about ten
# seconds, with no application and no test harness.
#
# WHY THIS EXISTS. Go does not check SQL, and this migration is the kind that fails during
# somebody's upgrade rather than in CI. Three ways it can be wrong that only execution
# reveals:
#
#   1. ADD COLUMN ... NOT NULL DEFAULT 0 CONSTRAINT ... CHECK (...) either parses or it
#      does not, and if it does not, the failure lands mid-upgrade on a live database.
#   2. The CHECK must ACCEPT 0. Zero is the inherit sentinel AND the column default, so a
#      constraint that rejected it would fail against every existing row on the way in.
#   3. The SQL CHECK and the Go guard in SetAuthorizedModelLimits must agree. Looser Go
#      means a bad value reaches the driver and surfaces as a driver error instead of a
#      readable message; tighter Go means a value an admin is allowed to set is refused
#      before it gets there.
#
# Also asserts the down migration is a real inverse, and that re-authorizing a model — the
# idempotent upsert in CreateAuthorizedModel — does NOT reset limits an admin set. Naming
# a model again is not a statement about its context window.
#
# Usage:  ./scripts/verify-model-limits-sql.sh
# Needs:  docker
set -euo pipefail

CONTAINER="oc-model-limits-sql-check"
IMAGE="postgres:16-alpine"
DB="limitscheck"

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

echo "==> starting $IMAGE"
docker run -d --name "$CONTAINER" -e POSTGRES_PASSWORD=pw -e POSTGRES_DB="$DB" "$IMAGE" >/dev/null
for _ in $(seq 1 60); do
  docker exec "$CONTAINER" pg_isready -U postgres -d "$DB" >/dev/null 2>&1 && break
  sleep 0.5
done

psql() { docker exec -i "$CONTAINER" psql -v ON_ERROR_STOP=1 -U postgres -d "$DB" "$@"; }
apply() { docker exec -i "$CONTAINER" psql -v ON_ERROR_STOP=1 -U postgres -d "$DB" < "$1" >/dev/null; }

echo "==> minimal schema (only what migration 140 touches)"
psql -q <<'SQL'
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE TABLE ai_providers (
    id uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    kind varchar NOT NULL, label varchar NOT NULL,
    enabled boolean NOT NULL DEFAULT true, is_builtin boolean NOT NULL DEFAULT false
);
CREATE TABLE ai_authorized_models (
    id uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider_id uuid NOT NULL REFERENCES ai_providers(id) ON DELETE CASCADE,
    model varchar NOT NULL, label varchar NOT NULL DEFAULT '',
    enabled boolean NOT NULL DEFAULT true,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE (provider_id, model)
);
INSERT INTO ai_providers (kind, label) VALUES ('ollama', 'Local');
-- A row that already exists, so the migration has to cope with data present. This is the
-- case that breaks if the CHECK rejects the column default.
INSERT INTO ai_authorized_models (provider_id, model, label)
SELECT id, 'llama3.2:3b', 'Llama' FROM ai_providers LIMIT 1;
SQL

echo "==> applying migration 140 UP over existing data"
apply migrations/140_add_authorized_model_limits.up.sql
echo "    ok"

echo "==> the pre-existing row inherits (0 = use the workspace window)"
psql -qtAX -c "SELECT context_window_tokens || '/' || max_output_tokens FROM ai_authorized_models;" \
  | grep -qx '0/0' && echo "    ok: 0/0" || { echo "    FAIL: existing row did not default to 0/0"; exit 1; }

echo "==> re-running the migration is safe (idempotent)"
apply migrations/140_add_authorized_model_limits.up.sql
echo "    ok"

echo "==> accepted values match the Go guard's range"
for v in 0 2048 8192 131072 1000000 20000000; do
  psql -q -c "UPDATE ai_authorized_models SET context_window_tokens = $v;" \
    || { echo "    FAIL: window $v should be accepted"; exit 1; }
done
for v in 0 256 4096 64000 1000000; do
  psql -q -c "UPDATE ai_authorized_models SET max_output_tokens = $v;" \
    || { echo "    FAIL: output $v should be accepted"; exit 1; }
done
echo "    ok"

echo "==> refused values (a nonsense window must never reach the budgeter)"
for v in 1 2047 20000001 -1; do
  if psql -q -c "UPDATE ai_authorized_models SET context_window_tokens = $v;" >/dev/null 2>&1; then
    echo "    FAIL: window $v should have been refused by the CHECK"; exit 1
  fi
done
for v in 1 255 1000001 -5; do
  if psql -q -c "UPDATE ai_authorized_models SET max_output_tokens = $v;" >/dev/null 2>&1; then
    echo "    FAIL: output $v should have been refused by the CHECK"; exit 1
  fi
done
echo "    ok"

echo "==> re-authorizing a model preserves admin-set limits"
psql -q -c "UPDATE ai_authorized_models SET context_window_tokens = 131072, max_output_tokens = 8192;"
# Exactly the upsert CreateAuthorizedModel runs.
psql -q <<'SQL'
INSERT INTO ai_authorized_models (provider_id, model, label, enabled)
SELECT provider_id, model, 'Renamed', true FROM ai_authorized_models LIMIT 1
ON CONFLICT (provider_id, model)
DO UPDATE SET label = EXCLUDED.label, enabled = true, updated_at = NOW();
SQL
psql -qtAX -c "SELECT context_window_tokens || '/' || max_output_tokens FROM ai_authorized_models;" \
  | grep -qx '131072/8192' \
  && echo "    ok: limits survived the upsert" \
  || { echo "    FAIL: re-authorizing reset limits an admin had set"; exit 1; }

echo "==> down migration is a true inverse, and UP re-applies after it"
apply migrations/140_add_authorized_model_limits.down.sql
if psql -qtAX -c "SELECT context_window_tokens FROM ai_authorized_models;" >/dev/null 2>&1; then
  echo "    FAIL: column still present after down"; exit 1
fi
apply migrations/140_add_authorized_model_limits.up.sql
echo "    ok"

echo
echo "PASS: migration 140 applies over existing data, defaults to inherit, enforces the"
echo "      same range the Go guard states, survives re-authorization, and reverses cleanly."
