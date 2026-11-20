-- What was still waiting was never sent: it is dropped, as Retract drops a
-- held push, and 585's states come back. Nothing is deleted.
UPDATE notification_deliveries SET state = 'dropped' WHERE state = 'waiting';
DROP INDEX IF EXISTS notification_deliveries_waiting_idx;
ALTER TABLE notification_deliveries DROP CONSTRAINT IF EXISTS notification_deliveries_state_check;
ALTER TABLE notification_deliveries ADD CONSTRAINT notification_deliveries_state_check
    CHECK (state IN ('sent', 'held', 'dropped', 'retracted'));
