-- Migration 213 (docs programme B9): "AI activity on this doc".
--
-- Doc AI records its rows in the hash-chained log with the doc's uuid in the
-- metadata and an action under "ai.doc.". Reading one doc's history would
-- otherwise walk the whole log; this partial index holds only those rows.
CREATE INDEX IF NOT EXISTS idx_admin_audit_log_doc_ai
    ON admin_audit_log ((metadata ->> 'doc_uuid'), seq DESC)
    WHERE action LIKE 'ai.doc.%';
