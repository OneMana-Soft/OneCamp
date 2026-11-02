ALTER TABLE data_tables
    DROP COLUMN IF EXISTS purge_by,
    DROP COLUMN IF EXISTS purge_at;
