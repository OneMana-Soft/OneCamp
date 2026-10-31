-- Polls and Q&A in every call (calls package A: meet-10, zoom-02).
--
-- A call's poll is one of business/Poll's polls with a session and neither a
-- channel (the nil uuid, as a DM's poll) nor a post; its votes are poll_votes
-- rows like any poll's. Anonymous means its votes are keyed so that no one,
-- the database included, can tell whose they are (business/Poll's CallVoter).
ALTER TABLE polls
    ADD COLUMN IF NOT EXISTS session_id uuid REFERENCES call_sessions (id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS anonymous boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS results_hidden boolean NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS polls_session_idx ON polls (session_id, created_at) WHERE session_id IS NOT NULL;

-- Q&A's settings live with the call, as its holds do: whether people may ask,
-- whether they see every question or only the answered ones, and whether they
-- may ask anonymously. The report under the call's post is claimed (a lease,
-- so a writer that died is followed) and then marked written, once.
ALTER TABLE call_sessions
    ADD COLUMN IF NOT EXISTS qa_open boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS qa_shown text NOT NULL DEFAULT 'all' CHECK (qa_shown IN ('all', 'answered')),
    ADD COLUMN IF NOT EXISTS qa_anonymous boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS report_claimed_at timestamptz,
    ADD COLUMN IF NOT EXISTS report_done_at timestamptz;

-- A call's questions. asker is the asker's call identity, or for an anonymous
-- question a key no one can turn back into a person (business/CallQA); an
-- anonymous question never keeps a name. The text is plain and short, and is
-- never indexed.
CREATE TABLE IF NOT EXISTS call_questions (
    id         uuid PRIMARY KEY,
    session_id uuid        NOT NULL REFERENCES call_sessions (id) ON DELETE CASCADE,
    asker      text        NOT NULL,
    asker_name text        NOT NULL DEFAULT '',
    anonymous  boolean     NOT NULL DEFAULT false,
    body       text        NOT NULL CHECK (char_length(body) BETWEEN 1 AND 300),
    state      text        NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'answered', 'dismissed')),
    pinned     boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (NOT anonymous OR asker_name = '')
);

CREATE INDEX IF NOT EXISTS call_questions_session_idx ON call_questions (session_id, created_at);

-- One upvote a person a question, kept under a key made from their call
-- identity (business/CallQA), never the identity itself: no row says who
-- upvoted what.
CREATE TABLE IF NOT EXISTS call_question_votes (
    question_id uuid        NOT NULL REFERENCES call_questions (id) ON DELETE CASCADE,
    voter       text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (question_id, voter)
);
