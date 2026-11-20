-- A4 H4 "The phone waits while you're in OneCamp" (business/Notify): a chat
-- push to someone in OneCamp waits their notify.push_delay as a row in state
-- 'waiting' until hold_until, and the Sweeper (notify.push.wait) sends it
-- then, unless they read the message meanwhile. 'held' keeps its meaning (a
-- pause, schedule or focus time: email then, push never). Ids only still: the
-- push's payload waits in Redis, never here.
--
-- The CHECK is added NOT VALID and then validated. The migration runs in one
-- transaction, so ADD CONSTRAINT's lock on the table is held through
-- VALIDATE's scan and the index build; both are short, as the table only
-- dates from migration 585. The partial index holds the waiting rows alone,
-- by when they're due, for the Sweeper's claim.
ALTER TABLE notification_deliveries DROP CONSTRAINT IF EXISTS notification_deliveries_state_check;
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_state_check
    CHECK (state IN ('sent', 'held', 'waiting', 'dropped', 'retracted')) NOT VALID;
ALTER TABLE notification_deliveries VALIDATE CONSTRAINT notification_deliveries_state_check;

CREATE INDEX IF NOT EXISTS notification_deliveries_waiting_idx
    ON notification_deliveries (hold_until) WHERE state = 'waiting';
