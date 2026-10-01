-- DEPENDS ON pending-action-claim.sql, which creates the schema and the users row.
-- The verify script runs them in order against one database; this file starts by
-- clearing the rows that one left behind.
\echo '=== SETUP: one row per terminal state, all sharing nothing but shape ==='
DELETE FROM ai_pending_actions;
INSERT INTO ai_pending_actions
  (id, requested_by, surface_type, tool_name, description, status, idempotency_key, expires_at, created_at, result, error, resolved_at, resolved_by)
VALUES
 ('10000000-0000-0000-0000-000000000001','11111111-1111-1111-1111-111111111111','channel','send_message','d','failed',   'K-FAILED',    now()+interval '15 min', now()-interval '2 hours', 'r','e', now(), '11111111-1111-1111-1111-111111111111'),
 ('10000000-0000-0000-0000-000000000002','11111111-1111-1111-1111-111111111111','channel','send_message','d','expired',  'K-EXPIRED',   now()+interval '15 min', now()-interval '2 hours', NULL,NULL, now(), NULL),
 ('10000000-0000-0000-0000-000000000003','11111111-1111-1111-1111-111111111111','channel','send_message','d','executed', 'K-EXECUTED',  now()+interval '15 min', now()-interval '2 hours', 'r',NULL, now(), '11111111-1111-1111-1111-111111111111'),
 ('10000000-0000-0000-0000-000000000004','11111111-1111-1111-1111-111111111111','channel','send_message','d','rejected', 'K-REJECTED',  now()+interval '15 min', now()-interval '2 hours', NULL,NULL, now(), '11111111-1111-1111-1111-111111111111'),
 ('10000000-0000-0000-0000-000000000005','11111111-1111-1111-1111-111111111111','channel','send_message','d','executing','K-EXECUTING', now()+interval '15 min', now(),                    NULL,NULL, NULL, NULL),
 ('10000000-0000-0000-0000-000000000006','11111111-1111-1111-1111-111111111111','channel','send_message','d','pending',  'K-PENDING',   now()+interval '15 min', now(),                    NULL,NULL, NULL, NULL);

\echo '=== THE FIX: retake must succeed for failed and expired, and ONLY those ==='
\echo '--- failed: expect 1 row returned ---'
UPDATE ai_pending_actions
  SET status='executing', requested_by='11111111-1111-1111-1111-111111111111', surface_type='channel', surface_id='',
      tool_name='send_message', params='{}'::jsonb, description='d2', expires_at=now()+interval '15 min',
      created_at=NOW(), result=NULL, error=NULL, resolved_at=NULL, resolved_by=NULL
  WHERE idempotency_key='K-FAILED' AND status IN ('failed','expired') RETURNING id::text AS retook_failed;

\echo '--- expired: expect 1 row returned ---'
UPDATE ai_pending_actions
  SET status='executing', created_at=NOW(), result=NULL, error=NULL, resolved_at=NULL, resolved_by=NULL
  WHERE idempotency_key='K-EXPIRED' AND status IN ('failed','expired') RETURNING id::text AS retook_expired;

\echo '--- executed: expect NO rows (it happened; a retry must be told so) ---'
UPDATE ai_pending_actions SET status='executing', created_at=NOW()
  WHERE idempotency_key='K-EXECUTED' AND status IN ('failed','expired') RETURNING id::text AS retook_executed;

\echo '--- rejected: expect NO rows (a human said no) ---'
UPDATE ai_pending_actions SET status='executing', created_at=NOW()
  WHERE idempotency_key='K-REJECTED' AND status IN ('failed','expired') RETURNING id::text AS retook_rejected;

\echo '--- executing: expect NO rows (someone is doing it now) ---'
UPDATE ai_pending_actions SET status='executing', created_at=NOW()
  WHERE idempotency_key='K-EXECUTING' AND status IN ('failed','expired') RETURNING id::text AS retook_executing;

\echo '--- pending: expect NO rows (waiting on a person) ---'
UPDATE ai_pending_actions SET status='executing', created_at=NOW()
  WHERE idempotency_key='K-PENDING' AND status IN ('failed','expired') RETURNING id::text AS retook_pending;

\echo '=== STATE AFTER: only the two non-applied ones moved ==='
SELECT idempotency_key, status, error IS NULL AS err_cleared, resolved_at IS NULL AS resolved_cleared
  FROM ai_pending_actions ORDER BY idempotency_key;

\echo '=== created_at MUST have been reset, or the reclaimer frees it mid-execution ==='
UPDATE ai_pending_actions
  SET status='failed', error='abandoned'
  WHERE status='executing' AND created_at <= NOW() - '30m0s'::interval;
SELECT idempotency_key, status AS after_reclaimer_sweep FROM ai_pending_actions
  WHERE idempotency_key IN ('K-FAILED','K-EXPIRED') ORDER BY idempotency_key;

\echo '=== CONCURRENCY: two retries both retake — exactly one must win ==='
UPDATE ai_pending_actions SET status='failed', created_at=now()-interval '1 hour' WHERE idempotency_key='K-FAILED';
BEGIN;
  UPDATE ai_pending_actions SET status='executing', created_at=NOW()
    WHERE idempotency_key='K-FAILED' AND status IN ('failed','expired') RETURNING id::text AS winner;
  -- second attempt inside the same visibility window sees the new status and gets nothing
  UPDATE ai_pending_actions SET status='executing', created_at=NOW()
    WHERE idempotency_key='K-FAILED' AND status IN ('failed','expired') RETURNING id::text AS loser;
COMMIT;

\echo '=== FULL RETRY CYCLE: claim -> fail -> retake -> succeed ==='
DELETE FROM ai_pending_actions;
INSERT INTO ai_pending_actions (id, requested_by, surface_type, tool_name, description, status, idempotency_key, expires_at)
  VALUES ('20000000-0000-0000-0000-000000000001','11111111-1111-1111-1111-111111111111','channel','send_message','d','executing','K-CYCLE', now()+interval '15 min')
  ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING;
-- handler fails -> settle as failed
UPDATE ai_pending_actions SET status='failed', error='the tool did not complete', resolved_at=NOW(), resolved_by='11111111-1111-1111-1111-111111111111'
  WHERE id='20000000-0000-0000-0000-000000000001' AND status='executing';
-- retry: insert conflicts...
INSERT INTO ai_pending_actions (id, requested_by, surface_type, tool_name, description, status, idempotency_key, expires_at)
  VALUES ('20000000-0000-0000-0000-000000000002','11111111-1111-1111-1111-111111111111','channel','send_message','d','executing','K-CYCLE', now()+interval '15 min')
  ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING;
-- ...so it retakes
UPDATE ai_pending_actions SET status='executing', created_at=NOW(), error=NULL, resolved_at=NULL, resolved_by=NULL
  WHERE idempotency_key='K-CYCLE' AND status IN ('failed','expired') RETURNING id::text AS retook_on_retry;
-- and succeeds
UPDATE ai_pending_actions SET status='executed', result='ok', resolved_at=NOW(), resolved_by='11111111-1111-1111-1111-111111111111'
  WHERE idempotency_key='K-CYCLE' AND status='executing';
SELECT count(*) AS cycle_rows, string_agg(status,',') AS cycle_final FROM ai_pending_actions WHERE idempotency_key='K-CYCLE';
