-- V-121: a table in the trash being deleted for good, by a person or past its
-- retention, is claimed as the delete starts: purge_at is when, purge_by who
-- (NULL: the server, for the retention). From then on it is no longer in the
-- trash (the trash's lists and a restore leave it out), and the trash's hourly
-- sweep finishes a delete a stopped server left part way, in the name of whoever
-- started it. Nullable with no default, so PG12 adds them without rewriting the
-- table; no index, since only tables in the trash are ever claimed.
ALTER TABLE data_tables
    ADD COLUMN IF NOT EXISTS purge_at timestamptz,
    ADD COLUMN IF NOT EXISTS purge_by uuid;
