-- Migration 80: @mention AI coworker toggle.
--
-- Adds the flag for the channel-scoped AI coworker: when a user @mentions
-- the automation bot in a channel, the bot replies in that channel with an
-- answer grounded only in that channel's recent messages (filtered by the
-- asker's permissions). Defaults ON because the coworker only ever acts on
-- an explicit mention, so there is no regression risk; an admin can still
-- disable it independently of AI chat.
-- Idempotent: safe to re-run.

ALTER TABLE ai_settings
    ADD COLUMN IF NOT EXISTS "coworker_enabled" boolean NOT NULL DEFAULT true;
