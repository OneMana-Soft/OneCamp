-- Focus time: while one of your own events marked focus is running, your
-- notifications are paused (domain/UserFCMToken/pushGate.go and the email
-- dispatcher read it). Computed from the event, so moving or deleting the event
-- moves or ends the pause with nothing to reschedule.
ALTER TABLE calendar_events ADD COLUMN IF NOT EXISTS is_focus boolean NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS calendar_events_focus_idx
    ON calendar_events (created_by, start_time, end_time)
    WHERE is_focus AND deleted_at IS NULL;
