-- Dropping this loses the record of what each month's pack said at the time it
-- was taken. Nothing else stores it, and it cannot be reconstructed afterwards
-- once retention has redacted the rows the receipts were computed over.
DROP INDEX IF EXISTS idx_evidence_receipts_period;
DROP INDEX IF EXISTS uniq_evidence_receipt_period;
DROP TABLE IF EXISTS evidence_receipts;
