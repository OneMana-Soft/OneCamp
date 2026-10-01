-- Migration 91: Tables — a first-class, Notion-style structured-data entity.
--
-- Per the design, a table is NOT stored inside a doc's body: it is its own
-- entity (own rows/fields/views/permissions) that docs can EMBED a live view of
-- via a Tiptap node holding only a {table_id, view_id} reference. Storing rows
-- in a doc would trap the data, break queries/relations/agent-tool access, and
-- bloat the CRDT.
--
-- Named data_table* (not "tables") to avoid any ambiguity with the SQL keyword
-- and information_schema.
--
-- Permission model (kept simple but real, mirroring Notion's workspace/private
-- pages): a table has an owner (created_by) and a visibility:
--   * private   — only the owner and system admins can see/edit it.
--   * workspace — every member can view AND edit rows (a shared team table);
--                 structure changes (fields/views/delete) stay owner/admin only.
-- A finer per-user ACL can be layered on later without changing this shape.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS data_tables (
    "id"          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "name"        varchar NOT NULL,
    "description" text,
    "icon"        varchar,                          -- optional emoji/icon key
    "visibility"  varchar NOT NULL DEFAULT 'workspace'
        CHECK (visibility IN ('private', 'workspace')),
    "created_by"  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "created_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at"  TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_data_tables_created_by
    ON data_tables (created_by)
    WHERE deleted_at IS NULL;

-- Columns. type is an open varchar + CHECK so adding a kind is a one-line
-- migration. config holds kind-specific settings (select options, number
-- format, date format, …) as jsonb.
CREATE TABLE IF NOT EXISTS data_table_fields (
    "id"        uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "table_id"  uuid NOT NULL REFERENCES data_tables(id) ON DELETE CASCADE,
    "name"      varchar NOT NULL,
    "type"      varchar NOT NULL DEFAULT 'text'
        CHECK (type IN ('text','number','select','multi_select','date','checkbox','person','url','email')),
    "config"    jsonb NOT NULL DEFAULT '{}'::jsonb,
    "position"  double precision NOT NULL DEFAULT 0,  -- fractional ordering
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_data_table_fields_table
    ON data_table_fields (table_id, position)
    WHERE deleted_at IS NULL;

-- Rows. values is a jsonb object keyed by FIELD ID → cell value, so adding or
-- removing a field never rewrites existing rows.
CREATE TABLE IF NOT EXISTS data_table_rows (
    "id"        uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "table_id"  uuid NOT NULL REFERENCES data_tables(id) ON DELETE CASCADE,
    "values"    jsonb NOT NULL DEFAULT '{}'::jsonb,
    "position"  double precision NOT NULL DEFAULT 0,
    "created_by" uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_data_table_rows_table
    ON data_table_rows (table_id, position)
    WHERE deleted_at IS NULL;

-- Saved views (grid / board / calendar). config holds filters, sorts,
-- group_by field, the calendar date field, visible field ids, etc.
CREATE TABLE IF NOT EXISTS data_table_views (
    "id"        uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "table_id"  uuid NOT NULL REFERENCES data_tables(id) ON DELETE CASCADE,
    "name"      varchar NOT NULL DEFAULT 'Grid',
    "type"      varchar NOT NULL DEFAULT 'grid'
        CHECK (type IN ('grid','board','calendar')),
    "config"    jsonb NOT NULL DEFAULT '{}'::jsonb,
    "position"  double precision NOT NULL DEFAULT 0,
    "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_data_table_views_table
    ON data_table_views (table_id, position)
    WHERE deleted_at IS NULL;
