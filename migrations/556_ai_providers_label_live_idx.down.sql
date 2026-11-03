-- A plain DROP, not CONCURRENTLY: sqlx-cli 0.8 runs a revert in a transaction
-- whatever its first line says, and dropping an index is quick.
DROP INDEX IF EXISTS uq_ai_providers_label_live;
