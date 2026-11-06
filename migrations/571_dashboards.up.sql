-- Migration 571: dashboards (B9-b2: jira-08, asana-08, linear-32;
-- business/Dashboard). A dashboard is a page of widgets kept for one person
-- (mine_of: never shared), for a project (project_id) by its admins, or, with
-- neither, for the workspace by its admins. A widget keeps a COPY of its query
-- (its projects, a view's filters and search, its weeks and what it measures),
-- taken when it was added and never a pointer to a saved view, and its place on
-- a 12-column grid. Nothing a widget shows is kept: each read works it out as
-- the reader. Deleting either is soft (deleted_at, deleted_by). Both tables are
-- new, so their indexes build here, on no rows. Ids are made in Go.
CREATE TABLE IF NOT EXISTS dashboards (
    "id"         uuid PRIMARY KEY,
    "project_id" uuid,
    "mine_of"    uuid,
    "name"       varchar(80) NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    "shared"     boolean NOT NULL DEFAULT false,
    "created_by" uuid NOT NULL,
    "created_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_by" uuid,
    "updated_at" TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at" TIMESTAMP WITH TIME ZONE,
    "deleted_by" uuid,
    CONSTRAINT dashboards_one_scope CHECK (project_id IS NULL OR mine_of IS NULL),
    CONSTRAINT dashboards_mine_unshared CHECK (mine_of IS NULL OR NOT shared)
);
-- A project's list, and a person's own, read these; the workspace's is the
-- few rows with neither.
CREATE INDEX IF NOT EXISTS dashboards_project_live ON dashboards (project_id)
    WHERE deleted_at IS NULL AND project_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS dashboards_mine_live ON dashboards (mine_of)
    WHERE deleted_at IS NULL AND mine_of IS NOT NULL;

CREATE TABLE IF NOT EXISTS dashboard_widgets (
    "id"           uuid PRIMARY KEY,
    "dashboard_id" uuid NOT NULL REFERENCES dashboards (id),
    "kind"         varchar(32) NOT NULL CHECK (char_length(kind) BETWEEN 1 AND 32),
    "title"        varchar(80) NOT NULL DEFAULT '',
    "query"        jsonb NOT NULL,
    "x"            smallint NOT NULL CHECK (x BETWEEN 0 AND 11),
    "y"            smallint NOT NULL CHECK (y BETWEEN 0 AND 999),
    "w"            smallint NOT NULL CHECK (w BETWEEN 1 AND 12),
    "h"            smallint NOT NULL CHECK (h BETWEEN 1 AND 12),
    "created_by"   uuid NOT NULL,
    "created_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_by"   uuid,
    "updated_at"   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "deleted_at"   TIMESTAMP WITH TIME ZONE,
    "deleted_by"   uuid,
    CONSTRAINT dashboard_widgets_on_grid CHECK (x + w <= 12)
);
-- The foreign key's index, and a dashboard's widgets in reading order.
CREATE INDEX IF NOT EXISTS dashboard_widgets_dashboard ON dashboard_widgets (dashboard_id, y, x);
