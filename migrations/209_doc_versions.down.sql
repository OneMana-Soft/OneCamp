-- Rollback of the doc versions migration. Versions taken for the new reasons
-- keep their rows, under the nearest old reason: 'idle' and 'saved' were
-- 'interval', and 'before_write' was 'manual' (taken before a body was replaced).
DROP INDEX IF EXISTS idx_doc_snapshots_run;

UPDATE doc_snapshots SET reason = 'interval' WHERE reason IN ('idle', 'saved');
UPDATE doc_snapshots SET reason = 'manual' WHERE reason = 'before_write';

ALTER TABLE doc_snapshots DROP CONSTRAINT IF EXISTS doc_snapshots_name_check;
ALTER TABLE doc_snapshots DROP CONSTRAINT IF EXISTS doc_snapshots_kind_check;
ALTER TABLE doc_snapshots DROP CONSTRAINT IF EXISTS doc_snapshots_reason_check;
ALTER TABLE doc_snapshots ADD CONSTRAINT doc_snapshots_reason_check
    CHECK (reason IN ('interval', 'mass_delete', 'manual'));

ALTER TABLE doc_snapshots
    DROP COLUMN IF EXISTS stored_bytes,
    DROP COLUMN IF EXISTS lineage,
    DROP COLUMN IF EXISTS run_id,
    DROP COLUMN IF EXISTS kind,
    DROP COLUMN IF EXISTS contributor_meta,
    DROP COLUMN IF EXISTS pinned,
    DROP COLUMN IF EXISTS name;
