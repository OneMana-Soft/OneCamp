-- Revert github_task_activities activity_type CHECK constraint to original values
ALTER TABLE github_task_activities DROP CONSTRAINT IF EXISTS github_task_activities_activity_type_check;
ALTER TABLE github_task_activities ADD CONSTRAINT github_task_activities_activity_type_check CHECK (activity_type IN ('comment','reaction','pr_opened','pr_closed','pr_merged','issue_opened','issue_closed','issue_reopened','branch_created','commit_pushed','status_synced','assignee_synced','label_synced'));
