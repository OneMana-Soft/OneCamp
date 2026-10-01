-- Permanent removal of archived files and recordings.
--
-- Archiving sets deleted_at and keeps every byte; nothing ever freed the disk.
-- purge_after_days = 0 keeps that behaviour. A positive value removes files
-- and recordings from storage once they have been archived for that many
-- days, and counts what went so the admin page can say so.
ALTER TABLE archive_policies
    ADD COLUMN IF NOT EXISTS purge_after_days int NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS purged_count bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS purged_bytes bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_purged_at TIMESTAMP WITH TIME ZONE;
