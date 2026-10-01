-- Migration 86: attribute board snapshots to their contributors.
--
-- A debounced save can aggregate edits from several people, so a version's
-- "edited by" is a SET of users, not one. The collaboration service accumulates
-- the distinct editor uuids since the last persist (in Redis, so it is correct
-- across scaled nodes) and sends them with each save. We store that set here.
--
-- Nullable / defaulted to empty so existing rows and saves without attribution
-- (e.g. after a collab restart that lost the in-flight set) degrade gracefully.

ALTER TABLE board_snapshots
    ADD COLUMN IF NOT EXISTS contributor_uuids text[] NOT NULL DEFAULT '{}';
