-- Hands and nonverbal feedback in a call (calls programme, CALLS-3a; 493 is a placeholder,
-- given the next free number at landing). They are the person's own, kept on their
-- attendance row, so a reconnect keeps a hand's place in the queue and the
-- webhook can put both back on their LiveKit attributes when they rejoin.
--   hand_raised_at  when their hand went up (NULL: down); the queue's order.
--   feedback        what stays beside their name until removed: yes, no,
--                   slow_down or away (NULL: none).
-- Both end with the call: an ended call's rows are only history.
ALTER TABLE call_attendance ADD COLUMN IF NOT EXISTS hand_raised_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE call_attendance ADD COLUMN IF NOT EXISTS feedback varchar(16);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'call_attendance_feedback_kind') THEN
        ALTER TABLE call_attendance ADD CONSTRAINT call_attendance_feedback_kind
            CHECK (feedback IS NULL OR feedback IN ('yes', 'no', 'slow_down', 'away'));
    END IF;
END $$;

-- The queue: the raised hands of one call, oldest first.
CREATE INDEX IF NOT EXISTS idx_call_attendance_hands ON call_attendance (session_id, hand_raised_at)
    WHERE hand_raised_at IS NOT NULL;
