ALTER TABLE archive_jobs DROP COLUMN IF EXISTS undo_started_at;
-- Before this, a partly undone run reads as completed.
UPDATE archive_jobs SET status = 'completed' WHERE status = 'partial';
ALTER TABLE archive_jobs DROP CONSTRAINT IF EXISTS archive_jobs_status_check;
ALTER TABLE archive_jobs ADD CONSTRAINT archive_jobs_status_check
    CHECK (status IN ('pending', 'running', 'completed', 'failed', 'cancelled'));
