-- Back to the states before moved_on: a request the task went on without
-- reads as taken back.
UPDATE approvals SET state = 'withdrawn' WHERE state = 'moved_on';
ALTER TABLE approvals DROP CONSTRAINT IF EXISTS approvals_state_check;
ALTER TABLE approvals ADD CONSTRAINT approvals_state_check
    CHECK (state IN ('pending', 'approved', 'changes', 'rejected', 'withdrawn'));
