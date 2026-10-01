-- Migration 121: per-channel default AI model.
--
-- Lets an admin/channel-manager pin which model AI runs in a channel use (e.g.
-- a fast/cheap model for a routine-digest channel, a stronger model for a
-- customer-facing one) — OneCamp's answer to Claude Tag's per-channel default
-- model. NULL (the default) means "no channel override": runs fall back to the
-- agent's pinned model, then the workspace default, exactly as before.
--
-- ai_model_id references the admin allowlist (ai_authorized_models). It's a soft
-- reference validated at read time (the runner degrades to the default if the
-- model is later disabled/removed), so no hard FK — the allowlist can be pruned
-- without orphaning channel rows. Idempotent.

ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS ai_model_id uuid;
