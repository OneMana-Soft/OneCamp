-- Migration 301 (tables A2, design/tables-vs-airtable/PROGRAM.md §3.7): the
-- row ledger.
--
-- Every write to a table's rows becomes a change set (who, how, when, which
-- rows and fields) with each cell's value before and after, so a row has a
-- history, a person can undo what they did, and a deleted row can come back
-- from the table's trash with its links.
--
-- A row gains a version (every write adds one), who last changed it, who
-- deleted it, and a place that needs no renumbering: position_key, a
-- fractional key (business/Tree/position's scheme, compared byte by byte, so
-- "C" collation), between its neighbours' keys. Every table's rows are given
-- keys in the order they are read in today (position, then created_at, then
-- id), deleted rows among them, so nothing moves: "d" and four base-62
-- digits, a key the scheme makes, room for 14,776,336 rows a table. Rows are
-- read in key order through the new index; the float position stays for the
-- API and the web released before A2, which read it.
--
-- A deleted field or table says who deleted it, for the trash.

ALTER TABLE data_table_rows ADD COLUMN IF NOT EXISTS version      bigint NOT NULL DEFAULT 1;
ALTER TABLE data_table_rows ADD COLUMN IF NOT EXISTS updated_by   uuid REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE data_table_rows ADD COLUMN IF NOT EXISTS deleted_by   uuid REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE data_table_rows ADD COLUMN IF NOT EXISTS position_key text COLLATE "C";

UPDATE data_table_rows r SET position_key = 'd'
    || substr(k.digits, ((k.n / 238328) % 62)::int + 1, 1)
    || substr(k.digits, ((k.n / 3844) % 62)::int + 1, 1)
    || substr(k.digits, ((k.n / 62) % 62)::int + 1, 1)
    || substr(k.digits, (k.n % 62)::int + 1, 1)
FROM (
    SELECT id, row_number() OVER (PARTITION BY table_id ORDER BY position, created_at, id) - 1 AS n,
        '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz'::text AS digits
    FROM data_table_rows
) k
WHERE r.id = k.id AND r.position_key IS NULL;

-- The order rows are read in, page after page; and a table's trash, newest first.
CREATE INDEX IF NOT EXISTS idx_data_table_rows_order
    ON data_table_rows (table_id, position_key, id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_data_table_rows_trash
    ON data_table_rows (table_id, deleted_at DESC) WHERE deleted_at IS NOT NULL;
-- Rows made without a place, by a server from before the keys between this
-- migration and the new server starting: the new server finds them by it, at
-- its start, and gives each one (business/DataTable linkUpkeep.go).
CREATE INDEX IF NOT EXISTS idx_data_table_rows_unkeyed
    ON data_table_rows (table_id) WHERE position_key IS NULL;

ALTER TABLE data_tables       ADD COLUMN IF NOT EXISTS deleted_by uuid REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE data_table_fields ADD COLUMN IF NOT EXISTS deleted_by uuid REFERENCES users(id) ON DELETE SET NULL;
-- A table's fields in the trash, newest first: the trash, and every whole-row
-- write, which keeps their cells as stored for their restore.
CREATE INDEX IF NOT EXISTS idx_data_table_fields_trash
    ON data_table_fields (table_id, deleted_at DESC) WHERE deleted_at IS NOT NULL;

-- A change set is one write: who made it (actor; agent when an agent acted for
-- them), how (actor_kind), what kind of write it was, how many rows and which
-- fields it changed, when; and an undo's change set names the one it undoes,
-- which names the undo back (undone_by).
CREATE TABLE IF NOT EXISTS data_table_change_sets (
    id         uuid PRIMARY KEY,
    table_id   uuid NOT NULL REFERENCES data_tables(id) ON DELETE CASCADE,
    actor      uuid REFERENCES users(id) ON DELETE SET NULL,
    agent      uuid,
    actor_kind text NOT NULL,
    kind       text NOT NULL,
    rows       int NOT NULL DEFAULT 0,
    fields     text[] NOT NULL DEFAULT '{}',
    undoes     uuid REFERENCES data_table_change_sets(id) ON DELETE SET NULL,
    undone_by  uuid REFERENCES data_table_change_sets(id) ON DELETE SET NULL,
    at         timestamptz NOT NULL DEFAULT NOW()
);

-- A person's change sets on a table, newest first: their undo and redo.
CREATE INDEX IF NOT EXISTS idx_data_table_change_sets_actor
    ON data_table_change_sets (table_id, actor, at DESC);
-- By age, for tables.history.purge.
CREATE INDEX IF NOT EXISTS idx_data_table_change_sets_at
    ON data_table_change_sets (at);

-- A change is one cell's value before and after (JSON as stored; NULL for no
-- cell), or a whole row's (field ''), a move ('#position') or a relation
-- field's links ('#links:<field id>'), in the order the change set made them.
CREATE TABLE IF NOT EXISTS data_table_changes (
    change_set uuid NOT NULL REFERENCES data_table_change_sets(id) ON DELETE CASCADE,
    seq        int NOT NULL,
    table_id   uuid NOT NULL,
    row_id     uuid NOT NULL,
    field      text NOT NULL,
    before     jsonb,
    after      jsonb,
    at         timestamptz NOT NULL DEFAULT NOW(),
    PRIMARY KEY (change_set, seq)
);

-- A row's history, newest first.
CREATE INDEX IF NOT EXISTS idx_data_table_changes_row
    ON data_table_changes (row_id, at DESC);
