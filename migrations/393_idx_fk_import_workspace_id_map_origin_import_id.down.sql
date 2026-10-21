-- A plain DROP, not CONCURRENTLY: sqlx-cli 0.8 runs a revert in a transaction
-- whatever its first line says, and dropping an index is quick.
DROP INDEX IF EXISTS idx_fk_import_workspace_id_map_origin_import_id;
