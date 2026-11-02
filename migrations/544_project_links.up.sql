-- Migration 544: a project's links
-- (basecamp-16). The tools a project's people work in elsewhere, a Figma
-- file, a Drive folder, a GitHub repository, kept on the project's home as
-- cards this server read from the pages themselves (business/ProjectLink):
-- a title, a few words, the site, and its icon and picture, stored here so a
-- reader never loads them from the site. A delete is soft (V-78).
CREATE TABLE IF NOT EXISTS project_links (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "project_uuid" uuid NOT NULL,
    -- The address as business/Unfurl's Canonical writes it: http or https,
    -- without tracking parameters.
    "url"          varchar(2048) NOT NULL CHECK (url ~ '^https?://'),
    "title"        varchar(200) NOT NULL CHECK (btrim(title) <> ''),
    "description"  varchar(500) NOT NULL DEFAULT '',
    "site_name"    varchar(120) NOT NULL DEFAULT '',
    -- The stored icon's and picture's attachments (src_key 'projectLink'),
    -- served only by the link's own route.
    "icon_object"  uuid,
    "image_object" uuid,
    "position"     int NOT NULL DEFAULT 0,
    "created_by"   uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at"   TIMESTAMP WITH TIME ZONE,
    "deleted_by"   uuid
);
-- A project's live links, each address once: its list reads by the project,
-- and a restore whose address a live link has taken since is refused. The
-- address is indexed by its md5, never whole: a btree entry holds at most
-- 2,704 bytes.
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_links_live_url ON project_links (project_uuid, md5(url)) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_project_links_created_by ON project_links (created_by);
