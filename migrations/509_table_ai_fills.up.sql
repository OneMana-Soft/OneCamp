-- An AI column's fill, from the request that starts it to its last
-- background turn (business/DataTable aiFillJob): how many cells it takes,
-- how far it has got, where it is in the table's order, and whether it runs,
-- was stopped or is done. Its writes are one change set (change_set), which
-- its person can undo whole. One fill runs per column at a time.
CREATE TABLE IF NOT EXISTS data_table_ai_fills (
    id          uuid PRIMARY KEY,
    table_id    uuid NOT NULL REFERENCES data_tables(id) ON DELETE CASCADE,
    field_id    uuid NOT NULL,
    started_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    change_set  uuid REFERENCES data_table_change_sets(id) ON DELETE SET NULL,
    total       int NOT NULL,
    asked       int NOT NULL DEFAULT 0,
    filled      int NOT NULL DEFAULT 0,
    cursor_key  text COLLATE "C",
    cursor_id   uuid,
    state       text NOT NULL DEFAULT 'running' CHECK (state IN ('running', 'stopped', 'done')),
    stopped_by  uuid REFERENCES users(id) ON DELETE SET NULL,
    started_at  timestamptz NOT NULL DEFAULT NOW(),
    updated_at  timestamptz NOT NULL DEFAULT NOW(),
    ended_at    timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_data_table_ai_fills_running
    ON data_table_ai_fills (table_id, field_id) WHERE state = 'running';
CREATE INDEX IF NOT EXISTS idx_data_table_ai_fills_table
    ON data_table_ai_fills (table_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_fk_data_table_ai_fills_change_set
    ON data_table_ai_fills (change_set);
CREATE INDEX IF NOT EXISTS idx_fk_data_table_ai_fills_started_by
    ON data_table_ai_fills (started_by);
CREATE INDEX IF NOT EXISTS idx_fk_data_table_ai_fills_stopped_by
    ON data_table_ai_fills (stopped_by);
