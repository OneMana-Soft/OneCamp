-- Migration 201: an invoice that has been sent keeps its number.
--   first_sent_at  the first time it was sent; never cleared. Taking a sent
--                  invoice back to draft cleared sent_at, after which it could
--                  be renumbered or deleted and its number given to another.
--                  Now its number stays, and it is voided, never deleted.
ALTER TABLE invoices ADD COLUMN IF NOT EXISTS "first_sent_at" TIMESTAMP WITH TIME ZONE;

UPDATE invoices
   SET first_sent_at = COALESCE(sent_at, paid_at, updated_at)
 WHERE first_sent_at IS NULL
   AND (sent_at IS NOT NULL OR status IN ('sent', 'paid'));
