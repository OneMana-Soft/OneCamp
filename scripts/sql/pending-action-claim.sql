-- Schema copied from migrations/101_create_ai_pending_actions.up.sql, plus the
-- minimum users table its FKs need. Nothing here is invented: if these exact
-- statements run, the ones in the Go code run.
CREATE TABLE users (id uuid PRIMARY KEY);
INSERT INTO users (id) VALUES ('11111111-1111-1111-1111-111111111111');

CREATE TABLE IF NOT EXISTS ai_pending_actions (
    "id"              uuid PRIMARY KEY NOT NULL,
    "requested_by"    uuid NOT NULL REFERENCES users(id),
    "surface_type"    varchar NOT NULL,
    "surface_id"      varchar NOT NULL DEFAULT '',
    "tool_name"       varchar NOT NULL,
    "params"          jsonb NOT NULL DEFAULT '{}'::jsonb,
    "description"     text NOT NULL DEFAULT '',
    "status"          varchar NOT NULL DEFAULT 'pending',
    "idempotency_key" varchar,
    "result"          text,
    "error"           text,
    "expires_at"      timestamptz NOT NULL,
    "created_at"      timestamptz NOT NULL DEFAULT now(),
    "resolved_at"     timestamptz,
    "resolved_by"     uuid REFERENCES users(id)
);
CREATE INDEX IF NOT EXISTS idx_ai_pending_actions_open
    ON ai_pending_actions (requested_by, created_at DESC)
    WHERE status = 'pending';
CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_pending_actions_idem
    ON ai_pending_actions (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

\echo '=== CASE 1: first claim must insert exactly 1 row, landing in executing ==='
INSERT INTO ai_pending_actions
  (id, requested_by, surface_type, surface_id, tool_name, params, description, status, idempotency_key, expires_at)
  VALUES ('aaaaaaaa-0000-0000-0000-000000000001','11111111-1111-1111-1111-111111111111','channel','ch-1','send_message','{}'::jsonb,'d','executing','KEY-A', now() + interval '15 minutes')
  ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING;
SELECT status AS case1_status, (SELECT count(*) FROM ai_pending_actions WHERE idempotency_key='KEY-A') AS case1_rows
  FROM ai_pending_actions WHERE idempotency_key='KEY-A';

\echo '=== CASE 2: retry with same key must insert 0 rows and raise no error ==='
INSERT INTO ai_pending_actions
  (id, requested_by, surface_type, surface_id, tool_name, params, description, status, idempotency_key, expires_at)
  VALUES ('aaaaaaaa-0000-0000-0000-000000000002','11111111-1111-1111-1111-111111111111','channel','ch-1','send_message','{}'::jsonb,'d','executing','KEY-A', now() + interval '15 minutes')
  ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING;
SELECT count(*) AS case2_rows_for_key FROM ai_pending_actions WHERE idempotency_key='KEY-A';

\echo '=== CASE 3: a DIFFERENT key must still insert ==='
INSERT INTO ai_pending_actions
  (id, requested_by, surface_type, surface_id, tool_name, params, description, status, idempotency_key, expires_at)
  VALUES ('aaaaaaaa-0000-0000-0000-000000000003','11111111-1111-1111-1111-111111111111','channel','ch-1','send_message','{}'::jsonb,'d','executing','KEY-B', now() + interval '15 minutes')
  ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING;
SELECT count(*) AS case3_total FROM ai_pending_actions;

\echo '=== CASE 4: NULL keys are exempt from the index — many rows must coexist ==='
INSERT INTO ai_pending_actions
  (id, requested_by, surface_type, tool_name, description, status, idempotency_key, expires_at)
  VALUES ('bbbbbbbb-0000-0000-0000-000000000001','11111111-1111-1111-1111-111111111111','channel','x','d','pending',NULL, now() + interval '1 hour'),
         ('bbbbbbbb-0000-0000-0000-000000000002','11111111-1111-1111-1111-111111111111','channel','x','d','pending',NULL, now() + interval '1 hour')
  ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING;
SELECT count(*) AS case4_null_key_rows FROM ai_pending_actions WHERE idempotency_key IS NULL;

\echo '=== CASE 5: Finalize as executed — guarded on status=executing, must affect 1 ==='
UPDATE ai_pending_actions
  SET status='executed', result='ok', error=NULL, resolved_at=NOW(), resolved_by='11111111-1111-1111-1111-111111111111'
  WHERE id='aaaaaaaa-0000-0000-0000-000000000001' AND status='executing';
SELECT status AS case5_status FROM ai_pending_actions WHERE id='aaaaaaaa-0000-0000-0000-000000000001';

\echo '=== CASE 6: a second Finalize on the same row must affect 0 (idempotent) ==='
UPDATE ai_pending_actions
  SET status='executed', result='ok', resolved_at=NOW(), resolved_by='11111111-1111-1111-1111-111111111111'
  WHERE id='aaaaaaaa-0000-0000-0000-000000000001' AND status='executing';

\echo '=== CASE 7: a settled key is STILL claimed — a later retry must not insert ==='
INSERT INTO ai_pending_actions
  (id, requested_by, surface_type, tool_name, description, status, idempotency_key, expires_at)
  VALUES ('cccccccc-0000-0000-0000-000000000001','11111111-1111-1111-1111-111111111111','channel','x','d','executing','KEY-A', now() + interval '15 minutes')
  ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING;
SELECT count(*) AS case7_rows_for_key FROM ai_pending_actions WHERE idempotency_key='KEY-A';

\echo '=== CASE 8: reclaimer must NOT touch a fresh executing row ==='
UPDATE ai_pending_actions
  SET status='failed', resolved_at=NOW(), error='execution abandoned: the server that claimed this action did not finish it'
  WHERE status='executing' AND created_at <= NOW() - '30m0s'::interval;
SELECT status AS case8_fresh_still_executing FROM ai_pending_actions WHERE idempotency_key='KEY-B';

\echo '=== CASE 9: reclaimer MUST free an abandoned executing row ==='
UPDATE ai_pending_actions SET created_at = NOW() - interval '2 hours' WHERE idempotency_key='KEY-B';
UPDATE ai_pending_actions
  SET status='failed', resolved_at=NOW(), error='execution abandoned: the server that claimed this action did not finish it'
  WHERE status='executing' AND created_at <= NOW() - '30m0s'::interval;
SELECT status AS case9_status, error IS NOT NULL AS case9_has_error
  FROM ai_pending_actions WHERE idempotency_key='KEY-B';

\echo '=== CASE 10: ExpireStale touches only overdue pending rows ==='
UPDATE ai_pending_actions SET expires_at = NOW() - interval '1 minute' WHERE id='bbbbbbbb-0000-0000-0000-000000000001';
UPDATE ai_pending_actions SET status='expired', resolved_at=NOW()
  WHERE status='pending' AND expires_at <= NOW();
SELECT id::text AS case10_id, status AS case10_status FROM ai_pending_actions
  WHERE id IN ('bbbbbbbb-0000-0000-0000-000000000001','bbbbbbbb-0000-0000-0000-000000000002') ORDER BY id;

\echo '=== CASE 11: the open-actions query must NOT surface a claim row ==='
SELECT count(*) AS case11_open_cards FROM ai_pending_actions
  WHERE requested_by='11111111-1111-1111-1111-111111111111' AND status='pending' AND expires_at > NOW();

\echo '=== CASE 12: a reclaimed (failed) key must be re-claimable after cleanup? ==='
-- The key stays claimed by design: failed reads back as "no prior call" for
-- PlanWrite, but the ROW still holds the unique key. Prove what actually happens
-- so the behaviour is known rather than assumed.
INSERT INTO ai_pending_actions
  (id, requested_by, surface_type, tool_name, description, status, idempotency_key, expires_at)
  VALUES ('dddddddd-0000-0000-0000-000000000001','11111111-1111-1111-1111-111111111111','channel','x','d','executing','KEY-B', now() + interval '15 minutes')
  ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING;
SELECT count(*) AS case12_rows_for_key, string_agg(status, ',') AS case12_statuses
  FROM ai_pending_actions WHERE idempotency_key='KEY-B';
