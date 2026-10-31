-- Migration 933 (a placeholder; the integrator numbers it): linear-05, a
-- project's estimate scale. A project that estimates in points has one row:
-- Linear's exponential, Fibonacci, linear or T-shirt scale, with the two
-- larger sizes (extended), zero as a size, and whether a task with no points
-- counts as one. No row, the project estimates in hours, as before. Values
-- and their names live in business/EstimateScale.
CREATE TABLE IF NOT EXISTS project_estimate_scales (
    "project_uuid"      uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    "kind"              varchar(16) NOT NULL CHECK (kind IN ('exponential', 'fibonacci', 'linear', 'tshirt')),
    "extended"          boolean NOT NULL DEFAULT false,
    "allow_zero"        boolean NOT NULL DEFAULT false,
    "count_unestimated" boolean NOT NULL DEFAULT true,
    "set_by"            uuid REFERENCES users(id) ON DELETE SET NULL,
    "created_at"        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    "updated_at"        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
-- Every foreign key has an index leading with it (deleting a user looks here).
CREATE INDEX IF NOT EXISTS idx_fk_project_estimate_scales_set_by ON project_estimate_scales (set_by) WHERE set_by IS NOT NULL;

-- A cycle's capacity set by hand in points, for a project that estimates in
-- points (capacity_minutes is the one in hours). The bound is business/Cycle's
-- CapacityMostPoints.
ALTER TABLE project_cycles ADD COLUMN IF NOT EXISTS capacity_points int
    CHECK (capacity_points IS NULL OR capacity_points BETWEEN 1 AND 100000);
