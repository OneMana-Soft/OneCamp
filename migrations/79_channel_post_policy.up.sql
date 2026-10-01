-- Migration 79: Announcement channels (channel post policy).
--
-- Adds a per-channel posting policy so a channel can be made
-- announcement-only: everyone reads, but only channel moderators/admins post.
-- This is the governance complement to workflow moderation — it prevents noise
-- structurally instead of cleaning it up reactively. Slack/Teams parity.
--
--   'everyone'    (default) — any member may post (today's behaviour).
--   'admins_only'           — only channel moderators/admins may post.
--
-- Enforced at the single post-create choke point; reads are unaffected. The
-- automation bot posts through a separate internal path, so workflow/webhook
-- announcements still work in an admins_only channel.

ALTER TABLE channels ADD COLUMN IF NOT EXISTS post_policy varchar NOT NULL DEFAULT 'everyone'
    CHECK (post_policy IN ('everyone', 'admins_only'));
