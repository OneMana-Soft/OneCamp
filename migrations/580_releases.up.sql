-- Migration 580: releases (jira-04, linear-36; PROGRAM.md #98, D-PJ-16). A
-- release is a project's named shipment of work: a name, a target day, notes,
-- the tasks that go out in it, and whether it has gone out. A person makes one
-- by hand; a tag pushed to a linked repository makes or ships one (repo, tag
-- and tag_sha, P10-1b job 2), and builds and deploys report stages against it.
-- A task may sit in several releases (Jira's fix versions). A release is
-- deleted softly (models/postgres/SoftDelete kind "release"): its tasks and
-- stages stay with it and come back on a restore.
CREATE TABLE IF NOT EXISTS releases (
    "id"           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "project_uuid" uuid NOT NULL,
    -- 120 at most, so the name index's entries stay far below Postgres's
    -- btree entry limit (2,704 bytes).
    "name"         varchar(120) NOT NULL CHECK (btrim(name) <> ''),
    "state"        varchar(16) NOT NULL DEFAULT 'planned' CHECK (state IN ('planned', 'released')),
    "target_date"  date,
    "released_at"  TIMESTAMP WITH TIME ZONE,
    -- The repository a tag made or shipped it from, "owner/name" in lower
    -- case; NULL for a release made by hand.
    "repo"         varchar(200),
    "tag"          varchar(250),
    "tag_sha"      varchar(64),
    "notes"        text NOT NULL DEFAULT '' CHECK (char_length(notes) <= 20000),
    "created_by"   uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at"   TIMESTAMP WITH TIME ZONE,
    "deleted_by"   uuid
);
-- A name is taken among a project's live releases only: a deleted "v2.1"
-- leaves the name free, and restoring it while a new "v2.1" lives is refused
-- in words (the restore's unique violation).
CREATE UNIQUE INDEX IF NOT EXISTS releases_project_name_idx ON releases (project_uuid, lower(name)) WHERE deleted_at IS NULL;
-- One live release per project for a repository's tag, and the lookups a tag
-- or a deploy makes: by repository and tag, and by the tag's commit.
CREATE UNIQUE INDEX IF NOT EXISTS releases_project_repo_tag_idx ON releases (project_uuid, repo, tag) WHERE deleted_at IS NULL AND tag IS NOT NULL;
CREATE INDEX IF NOT EXISTS releases_repo_sha_idx ON releases (repo, tag_sha) WHERE deleted_at IS NULL AND tag_sha IS NOT NULL;

-- A release's tasks: put in by a person (manual) or named by the commits a tag
-- ships (git). Taking one out is soft too (deleted_at, deleted_by): its row
-- stays, out of every read, count and most, and putting the task back brings
-- that row back. So the key holds over every row, live or not: a task has one
-- row per release, ever, and a put-back is the one write that revives it.
CREATE TABLE IF NOT EXISTS release_tasks (
    "release_id" uuid NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    "task_uuid"  uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    "source"     varchar(8) NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'git')),
    "added_by"   uuid REFERENCES users(id) ON DELETE SET NULL,
    "added_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE,
    "deleted_by" uuid,
    PRIMARY KEY ("release_id", "task_uuid")
);
-- Whole, not live only: a task's hard delete (ON DELETE CASCADE) looks here
-- for every row, as TestEveryForeignKeyHasAnIndex wants.
CREATE INDEX IF NOT EXISTS idx_release_tasks_task ON release_tasks(task_uuid);
-- Every foreign key has an index leading with it: deleting a user looks for
-- the releases and the release tasks they made.
CREATE INDEX IF NOT EXISTS idx_fk_releases_created_by ON releases (created_by) WHERE created_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_fk_release_tasks_added_by ON release_tasks (added_by) WHERE added_by IS NOT NULL;

-- What a repository's builds and deploys last said about a release: one row
-- per provider, repository, kind and workflow or environment, kept at the
-- latest status reported.
CREATE TABLE IF NOT EXISTS release_stages (
    "id"          uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    "release_id"  uuid NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    "provider"    varchar(32) NOT NULL,
    "repo"        varchar(200) NOT NULL,
    "kind"        varchar(8) NOT NULL CHECK (kind IN ('build', 'deploy')),
    "name"        varchar(200) NOT NULL,
    "status"      varchar(32) NOT NULL,
    "sha"         varchar(64) NOT NULL DEFAULT '',
    "url"         varchar(2048) NOT NULL DEFAULT '',
    "delivery"    varchar(128) NOT NULL DEFAULT '',
    "reported_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE ("release_id", "provider", "repo", "kind", "name")
);
