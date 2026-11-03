-- no-transaction
-- V-184 head 4b: a model is on the AI allowlist once per provider among live
-- entries only, so a model authorized again after it was revoked (kept,
-- marked) is a new entry. Authorizing a live one again still finds it
-- (ON CONFLICT (provider_id, model) WHERE deleted_at IS NULL infers this
-- index). It replaces UNIQUE (provider_id, model), which 560 drops.
-- CONCURRENTLY: built without blocking writes; one statement per file,
-- outside a transaction (sqlx reads the first line). A build that fails
-- leaves an INVALID index: drop it and run the migration again.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_ai_authorized_models_live ON ai_authorized_models (provider_id, model) WHERE deleted_at IS NULL;
