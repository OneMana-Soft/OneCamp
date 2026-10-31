ALTER TABLE call_attendance DROP CONSTRAINT IF EXISTS call_attendance_allowed_kind;
ALTER TABLE call_sessions DROP CONSTRAINT IF EXISTS call_sessions_held_kind;
ALTER TABLE call_attendance DROP COLUMN IF EXISTS allowed;
ALTER TABLE call_sessions DROP COLUMN IF EXISTS held;
