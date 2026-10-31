-- Personal meeting rooms (calls programme, CALLS-2; 495 is a placeholder,
-- given the next free number at landing).
--
-- A personal room is a call_meetings row of kind 'personal': one open room
-- per person (business/CallRoom), reusable, until its owner closes it
-- (archived_at). Who may come in from outside is its standing guest grant,
-- published at the owner's address (guest_grants.slug), under the guest-access
-- switch and its "Personal meeting rooms" sub-option.
--
-- This index keeps it to one: two Opens at once find the same room, as the
-- second's insert fails and it reads the first. It also answers "the person's
-- room" (PersonalMeetingOf).
CREATE UNIQUE INDEX IF NOT EXISTS idx_call_meetings_personal_owner ON call_meetings (owner)
    WHERE kind = 'personal' AND archived_at IS NULL;
