-- The daily note a member's AI teammate leaves in their DM the first time
-- they open OneCamp each day: what needs them, and an offer to help.
--
-- One row per member. last_day is the member's own calendar day (sent by
-- their browser, so no time zone is stored), which makes delivery once a day
-- and race-free across tabs and instances. opted_out is their choice to stop.
CREATE TABLE IF NOT EXISTS agent_notes (
    "user_id"    uuid PRIMARY KEY,
    "last_day"   date,
    "opted_out"  boolean NOT NULL DEFAULT false,
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
