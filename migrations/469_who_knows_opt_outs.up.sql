-- Who knows (DG9): the people who asked not to be suggested.
--
-- "Who knows about X" names the people whose work on X the asker can read:
-- what they wrote, what they're assigned, where they commented. Everyone is in
-- by default (O-DG3, ruled 18 Oct 2026); a row here takes one person out of
-- every answer, in both editions and through the assistant alike. Removing
-- the row puts them back. Keyed by the person's uuid (users.id, the graph's
-- user_uuid), so the graph's answer is filtered without a join.
CREATE TABLE IF NOT EXISTS who_knows_opt_outs (
    "user_uuid"  uuid PRIMARY KEY,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
