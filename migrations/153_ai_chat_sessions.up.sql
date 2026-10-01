-- Migration 153: durable AI conversation sessions.
--
-- Conversations lived only in Redis, under a 30 minute TTL, and the session id
-- lived only in React state. Three consequences, and the third is the one people
-- notice:
--
--   1. A conversation evaporated 30 minutes after the last message.
--   2. Closing the panel or reloading the page lost the id, so even inside those
--      30 minutes there was no way back to it.
--   3. Nothing indexed a user's sessions, so a "previous conversations" list
--      could not be built at all. There was nothing to enumerate.
--
-- Every AI product a person uses in 2026 keeps their conversations. Ours forgot
-- them within the hour, which reads as the product not valuing what they said.
--
-- REDIS STAYS. It remains the hot path for the prompt's working history, where a
-- short TTL and a tight message cap are exactly right for token control. This is
-- the durable record beside it, which is a different job: Redis answers "what
-- should go in the next prompt", these tables answer "what did we talk about".
CREATE TABLE IF NOT EXISTS ai_chat_sessions (
    "id"         uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "user_id"    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Derived from the opening question rather than asked for, because nobody
    -- names a conversation before having it. Editable later.
    "title"      varchar NOT NULL DEFAULT '',

    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE
);

-- The list query: a person's sessions, newest activity first.
CREATE INDEX IF NOT EXISTS idx_ai_chat_sessions_user
    ON ai_chat_sessions (user_id, updated_at DESC)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS ai_chat_messages (
    "id"         uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "session_id" uuid NOT NULL REFERENCES ai_chat_sessions(id) ON DELETE CASCADE,

    -- 'user' or 'assistant'. Open varchar with a CHECK so adding a role later is
    -- a one-line migration rather than a type change.
    "role"       varchar NOT NULL CHECK (role IN ('user', 'assistant')),
    "content"    text NOT NULL,

    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- Replaying one conversation in order.
CREATE INDEX IF NOT EXISTS idx_ai_chat_messages_session
    ON ai_chat_messages (session_id, created_at);
