-- Migration 149: skill revision history.
--
-- A skill is one instruction module attached to many agents, and editing it
-- changes every one of them on its next run. That is the point of skills and
-- also their danger: today an edit overwrites the text with no record of what
-- it said before, who changed it, or why. If an agent starts behaving
-- differently, nothing connects that to the skill someone rewrote on Tuesday.
--
-- Every version is stored, including the one created with the skill, so the
-- history is a list of what the skill HAS SAID rather than a list of diffs to
-- reconstruct. Reverting writes a new revision rather than deleting later ones:
-- a rollback is a thing that happened, not an erasure of what it undid.
--
-- The note is why. An audit trail that records what changed and not why answers
-- the easy half of the question.
CREATE TABLE IF NOT EXISTS ai_agent_skill_revisions (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "skill_id"     uuid NOT NULL REFERENCES ai_agent_skills(id) ON DELETE CASCADE,
    "name"         varchar NOT NULL,
    "instructions" text NOT NULL,
    -- Why this version exists. Free text, capped in the business layer.
    "note"         text NOT NULL DEFAULT '',
    -- Nullable: a revision outlives the account that wrote it, and losing the
    -- history when someone leaves is the opposite of what an audit trail is for.
    "edited_by"    uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ai_agent_skill_revisions_skill
    ON ai_agent_skill_revisions (skill_id, created_at DESC);
