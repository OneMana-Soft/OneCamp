-- github_import_jobs is the durable, resumable record of a "Import
-- Issues" or "Import PRs" run from the admin panel. Before this
-- table, imports ran synchronously inside the HTTP handler and
-- routinely timed out for repos with more than ~200 open issues
-- (each issue creates a task in PG + Dgraph + activity log, easily
-- 5+ DB roundtrips per item).
--
-- The handler now creates a row here and returns the job id; a
-- background worker drains the queue. Progress is published over
-- MQTT so the admin UI can render a live counter.
--
-- Rows are kept for 30 days for operator visibility, then cleaned
-- by the same scheduler that handles webhook log retention.
CREATE TABLE IF NOT EXISTS github_import_jobs (
    id              uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
    link_id         uuid NOT NULL REFERENCES github_links(id) ON DELETE CASCADE,
    -- import_kind: 'issues' or 'prs'. Closed enum at the application
    -- layer; we don't bother with a SQL enum because we want to add
    -- 'discussions' / 'projects' in the future without a migration.
    import_kind     varchar NOT NULL,
    triggered_by    uuid REFERENCES users(id),
    -- status: 'pending' | 'running' | 'completed' | 'failed'.
    -- Workers claim a 'pending' row with FOR UPDATE SKIP LOCKED so
    -- multiple application replicas never run the same job twice.
    status          varchar NOT NULL DEFAULT 'pending',
    -- Progress counters, updated periodically by the worker so the
    -- admin UI can show "237/512 imported" without hammering GitHub.
    items_total     int NOT NULL DEFAULT 0,
    items_imported  int NOT NULL DEFAULT 0,
    items_skipped   int NOT NULL DEFAULT 0,
    items_failed    int NOT NULL DEFAULT 0,
    error_message   text,
    started_at      timestamptz,
    completed_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT NOW(),
    updated_at      timestamptz NOT NULL DEFAULT NOW()
);

-- Workers drain by status order in claim order; one index does both.
CREATE INDEX IF NOT EXISTS idx_github_import_jobs_status_created
    ON github_import_jobs(status, created_at)
    WHERE status IN ('pending', 'running');

-- Per-link visibility for the admin panel (last 10 jobs for a repo).
CREATE INDEX IF NOT EXISTS idx_github_import_jobs_link_created
    ON github_import_jobs(link_id, created_at DESC);
