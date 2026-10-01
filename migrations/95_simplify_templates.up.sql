-- Migration 95: simplify the template gallery (was "marketplace").
--
-- The reviewable, install-counted "marketplace" framing was over-engineered for
-- a single-workspace internal tool: star reviews and a popularity counter add
-- maintenance and noise without real value when the whole audience is one team.
-- This strips it back to a focused Templates gallery: publish something you
-- built, browse, and install a copy. The tables/package keep their original
-- names to avoid a churny rename; only the social layer is removed.
--
-- Drops the reviews table and the denormalized install counter. The template
-- catalog itself (marketplace_templates) is unchanged. Idempotent.

DROP TABLE IF EXISTS marketplace_reviews;

ALTER TABLE marketplace_templates DROP COLUMN IF EXISTS install_count;
