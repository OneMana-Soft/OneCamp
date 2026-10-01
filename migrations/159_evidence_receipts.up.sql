-- What the evidence pack said, at the time it said it.
--
-- THE PROBLEM WITH A PACK GENERATED ON DEMAND. It is assembled from rows that
-- still exist. Retention redacts row content once it passes the window, and the
-- verification counts redacted rows separately precisely because their content
-- can no longer be recomputed from. So a pack built in March covering January is
-- not the same document as one built in February covering January, and the
-- difference is silent: both say "verified", and the later one verified less.
--
-- A receipt is taken while the rows are intact. It stores no content -- only what
-- the pack was: the window, the fingerprint, the manifest's per-section digests,
-- and the verification counts. Small enough to keep forever, and enough to make
-- a later regeneration checkable: rebuild the pack for that window, recompute the
-- fingerprint, and a mismatch is a fact worth knowing rather than a silence.
--
-- IT IS NOT A BACKUP AND MUST NOT BE READ AS ONE. It cannot reconstruct a single
-- row. What it can do is tell you that the document you are holding today is, or
-- is not, the one this system produced then.
--
-- ONE ROW PER WINDOW, enforced rather than left to the caller: the job that
-- writes these runs on a timer and will retry, and a retry must not leave two
-- receipts claiming the same month with different fingerprints.
CREATE TABLE IF NOT EXISTS evidence_receipts (
    "id"              uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "period_start"    timestamptz NOT NULL,
    "period_end"      timestamptz NOT NULL,
    "generated_at"    timestamptz NOT NULL DEFAULT now(),
    "pack_fingerprint" varchar NOT NULL,
    -- The manifest array as the pack emitted it: section, rows, sha256, describes.
    -- Stored whole because a per-section digest is what lets a reader check one
    -- part of a regenerated pack rather than only the document as a whole.
    "manifest"        jsonb NOT NULL DEFAULT '[]'::jsonb,
    "chain_ok"        boolean NOT NULL,
    "chain_checked"   integer NOT NULL DEFAULT 0,
    -- Rows the chain took at their word because retention had cleared them.
    -- Separate from checked for the same reason the verifier keeps them separate.
    "chain_redacted"  integer NOT NULL DEFAULT 0,
    "created_at"      timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uniq_evidence_receipt_period
    ON evidence_receipts ("period_start", "period_end");

-- The list is always "newest first", which is the only order anyone reads it in.
CREATE INDEX IF NOT EXISTS idx_evidence_receipts_period
    ON evidence_receipts ("period_start" DESC);
