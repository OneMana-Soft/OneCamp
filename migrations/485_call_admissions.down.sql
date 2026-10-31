UPDATE call_attendance SET role = 'participant' WHERE role = 'invitee';
ALTER TABLE call_attendance DROP CONSTRAINT IF EXISTS call_attendance_role_check;
ALTER TABLE call_attendance ADD CONSTRAINT call_attendance_role_check
    CHECK (role IN ('host', 'cohost', 'participant', 'guest'));
DROP TABLE IF EXISTS call_admissions;
