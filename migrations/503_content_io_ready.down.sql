-- A job still ready can't be kept without the status: it fails, and its
-- person imports the file again.
UPDATE content_io_jobs
    SET status = 'failed', updated_at = NOW(), finished_at = NOW(),
        error = 'This import was waiting to be confirmed when imports changed. Import the file again.'
    WHERE status = 'ready';

ALTER TABLE content_io_jobs DROP CONSTRAINT IF EXISTS content_io_jobs_status_check;
ALTER TABLE content_io_jobs ADD CONSTRAINT content_io_jobs_status_check
    CHECK (status IN ('uploading', 'queued', 'running', 'done', 'failed'));

DROP INDEX IF EXISTS idx_content_io_jobs_unfinished;
CREATE INDEX IF NOT EXISTS idx_content_io_jobs_unfinished
    ON content_io_jobs (updated_at)
    WHERE status IN ('uploading', 'running');
