ALTER TABLE archive_policies
    DROP COLUMN IF EXISTS purge_after_days,
    DROP COLUMN IF EXISTS purged_count,
    DROP COLUMN IF EXISTS purged_bytes,
    DROP COLUMN IF EXISTS last_purged_at;
