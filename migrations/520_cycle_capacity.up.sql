-- Migration 926 (a placeholder; the integrator numbers it): a capacity a
-- person sets for one cycle, in minutes (linear-13). NULL is the project's
-- own, what its recent cycles finished (business/Cycle/backlog.go). The
-- bound is business/Cycle's CapacityMost, 100,000 hours.
ALTER TABLE project_cycles ADD COLUMN IF NOT EXISTS capacity_minutes int
    CHECK (capacity_minutes IS NULL OR capacity_minutes BETWEEN 1 AND 6000000);
