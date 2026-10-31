-- An undo that has begun and not finished (business/Archive UndoArchiveJob):
-- part of the run may be back. It is never "completed" again, and Undo
-- finishes it.
ALTER TABLE archive_jobs DROP CONSTRAINT IF EXISTS archive_jobs_status_check;
ALTER TABLE archive_jobs ADD CONSTRAINT archive_jobs_status_check
    CHECK (status IN ('pending', 'running', 'completed', 'failed', 'cancelled', 'partial'));
-- When the undo under way began. A partial run whose undo began less than
-- the undo's deadline ago is being undone, and a second undo waits; one that
-- stopped (it failed, or its server went down) has none, or an older one.
ALTER TABLE archive_jobs ADD COLUMN IF NOT EXISTS undo_started_at TIMESTAMPTZ;
