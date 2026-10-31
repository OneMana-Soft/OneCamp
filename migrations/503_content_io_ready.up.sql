-- A content IO job may wait, ready, for its person to choose what to make of
-- its file: a spreadsheet's preview (tables S1) shows its sheets, its
-- columns' kinds and its first rows before any table is made. The cleanup
-- fails one left unconfirmed, so its partial index takes ready rows too.
ALTER TABLE content_io_jobs DROP CONSTRAINT IF EXISTS content_io_jobs_status_check;
ALTER TABLE content_io_jobs ADD CONSTRAINT content_io_jobs_status_check
    CHECK (status IN ('uploading', 'queued', 'running', 'ready', 'done', 'failed'));

DROP INDEX IF EXISTS idx_content_io_jobs_unfinished;
CREATE INDEX IF NOT EXISTS idx_content_io_jobs_unfinished
    ON content_io_jobs (updated_at)
    WHERE status IN ('uploading', 'running', 'ready');
