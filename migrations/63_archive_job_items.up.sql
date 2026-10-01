-- archive_job_items normalises the per-item id list that used to live
-- in archive_jobs.metadata as a JSONB blob.
--
-- Why we moved it
-- ---------------
-- A 5,000-item archive run produced a ~190 KB JSONB blob; a 100,000-
-- item run pushed that to ~4 MB. Every GetArchiveJobs list call
-- serialised that payload back through the API even though the FE
-- only ever needs the entity id list when the operator clicks "Undo".
--
-- The side table:
--   - keeps archive_jobs rows compact (just counters + status).
--   - lets us page through items if a job archived a very large set.
--   - lets us garbage-collect the items independently of the parent
--     job row when the operator no longer needs undo capability.
CREATE TABLE IF NOT EXISTS archive_job_items (
    job_id     uuid NOT NULL REFERENCES archive_jobs(id) ON DELETE CASCADE,
    -- entity_id is stored as text rather than uuid because some entity
    -- types (recordings) use opaque non-UUID identifiers.
    entity_id  text NOT NULL,
    PRIMARY KEY (job_id, entity_id)
);

-- Index used by UndoArchiveJob to pull a job's full id list at once.
CREATE INDEX IF NOT EXISTS idx_archive_job_items_job
    ON archive_job_items(job_id);

-- Backfill any existing JSONB blobs into the new table on a best-effort
-- basis. Failures are silent (safe: old code path still reads the
-- metadata blob until the next deploy).
INSERT INTO archive_job_items (job_id, entity_id)
SELECT
    j.id,
    elem.value
FROM archive_jobs AS j
CROSS JOIN LATERAL jsonb_array_elements_text(
    COALESCE(j.metadata::jsonb -> 'archived_ids', '[]'::jsonb)
) AS elem
WHERE j.metadata IS NOT NULL
ON CONFLICT (job_id, entity_id) DO NOTHING;
