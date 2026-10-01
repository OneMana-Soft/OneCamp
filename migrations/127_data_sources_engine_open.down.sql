-- Rollback migration 127: restore the postgres-only engine CHECK.
-- (Only valid if no rows use an engine outside this set.)
ALTER TABLE data_sources
    ADD CONSTRAINT data_sources_engine_check CHECK (engine IN ('postgres', 'mysql'));
