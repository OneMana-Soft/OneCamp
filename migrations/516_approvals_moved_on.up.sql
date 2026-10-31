-- A request closes when its task goes on without it: the task moved by
-- another path, or was deleted (business/Approvals, WentOn). A late answer
-- to it is refused, so it can't pull the task back.
ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_state_check;
ALTER TABLE approvals ADD CONSTRAINT approvals_state_check
    CHECK (state IN ('pending', 'approved', 'changes', 'rejected', 'withdrawn', 'moved_on'));
