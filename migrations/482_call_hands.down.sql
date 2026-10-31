DROP INDEX IF EXISTS idx_call_attendance_hands;
ALTER TABLE call_attendance DROP CONSTRAINT IF EXISTS call_attendance_feedback_kind;
ALTER TABLE call_attendance DROP COLUMN IF EXISTS feedback;
ALTER TABLE call_attendance DROP COLUMN IF EXISTS hand_raised_at;
