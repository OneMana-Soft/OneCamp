-- content_io_jobs: the docs programme's import and export jobs (A7). One row
-- per file imported (a Markdown, HTML, text or Word file, or a zip of them)
-- or per export made on the server, with where it stands, for the person who
-- started it.
--
-- The upload is kept in object storage (input_key) until the job is done, so
-- a job survives a restart between the upload and the work, and a worker on
-- another replica can take it. Workers claim a queued row with FOR UPDATE
-- SKIP LOCKED, so two never run the same job. Ids come from the application
-- (uuid.New), as Postgres 12 has no gen_random_uuid without pgcrypto.
CREATE TABLE IF NOT EXISTS content_io_jobs (
    id          uuid PRIMARY KEY NOT NULL,
    -- 'import' or 'export'.
    kind        varchar(16) NOT NULL CHECK (kind IN ('import', 'export')),
    -- The adapter: 'md', 'txt', 'html', 'docx', 'zip'; 'md', 'html', 'json' for an export.
    format      varchar(32) NOT NULL DEFAULT '',
    user_uuid   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- uploading: waiting for the file (a large one goes straight to storage);
    -- queued, running, then done or failed.
    status      varchar(16) NOT NULL DEFAULT 'queued'
                CHECK (status IN ('uploading', 'queued', 'running', 'done', 'failed')),
    progress    smallint NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    -- What the job was asked: the file's name and type, where its pages go.
    params      jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- What it did: the pages an import made, what it had to leave out.
    result      jsonb NOT NULL DEFAULT '{}'::jsonb,
    input_key   text,
    output_key  text,
    error       text,
    created_at  timestamptz NOT NULL DEFAULT NOW(),
    updated_at  timestamptz NOT NULL DEFAULT NOW(),
    finished_at timestamptz
);

-- The queue, oldest first; only rows that are waiting.
CREATE INDEX IF NOT EXISTS idx_content_io_jobs_queued
    ON content_io_jobs (created_at)
    WHERE status = 'queued';

-- A person's own jobs, newest first.
CREATE INDEX IF NOT EXISTS idx_content_io_jobs_user
    ON content_io_jobs (user_uuid, created_at DESC);

-- The cleanup: finished jobs by age, and jobs left running or uploading.
CREATE INDEX IF NOT EXISTS idx_content_io_jobs_finished
    ON content_io_jobs (finished_at)
    WHERE finished_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_content_io_jobs_unfinished
    ON content_io_jobs (updated_at)
    WHERE status IN ('uploading', 'running');
