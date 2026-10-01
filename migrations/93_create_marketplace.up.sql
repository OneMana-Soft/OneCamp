-- Migration 93: Marketplace — shareable templates for agents, automations
-- (workflows), and tables, with reviewable install (Phase 5.3).
--
-- A template captures a reusable configuration (the create-input for its kind)
-- as a JSON payload. Any member can publish a template from something they
-- built; anyone in the workspace can browse, install (which instantiates a
-- fresh copy in the installer's workspace as themselves), and review it.
--
-- install_count is a denormalized counter bumped on each install so listing can
-- sort by popularity without aggregating the (not separately tracked) installs.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS marketplace_templates (
    "id"            uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "kind"          varchar NOT NULL,                  -- agent | workflow | table
    "name"          varchar NOT NULL,
    "description"   text,
    "icon"          varchar,
    "payload"       jsonb NOT NULL DEFAULT '{}'::jsonb, -- stable create-input for the kind
    "install_count" integer NOT NULL DEFAULT 0,
    "created_by"    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "created_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"    TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "deleted_at"    TIMESTAMP WITH TIME ZONE            -- NULL = listed
);

CREATE INDEX IF NOT EXISTS idx_marketplace_templates_listed
    ON marketplace_templates (kind, created_at DESC)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_marketplace_templates_author
    ON marketplace_templates (created_by)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS marketplace_reviews (
    "id"          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "template_id" uuid NOT NULL REFERENCES marketplace_templates(id) ON DELETE CASCADE,
    "created_by"  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    "rating"      smallint NOT NULL CHECK (rating BETWEEN 1 AND 5),
    "comment"     text,
    "created_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    "updated_at"  TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    -- One review per user per template; re-reviewing updates in place.
    UNIQUE (template_id, created_by)
);

CREATE INDEX IF NOT EXISTS idx_marketplace_reviews_template
    ON marketplace_reviews (template_id);
