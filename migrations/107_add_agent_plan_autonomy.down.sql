-- Migration 107 down: restore the autonomy CHECK to auto|approval.
-- Any agents set to 'plan' are reverted to 'approval' (the safe, gated mode)
-- before tightening the constraint so the down migration cannot fail.
UPDATE ai_agents SET "autonomy" = 'approval' WHERE "autonomy" = 'plan';
ALTER TABLE ai_agents DROP CONSTRAINT IF EXISTS ai_agents_autonomy_check;
ALTER TABLE ai_agents ADD CONSTRAINT ai_agents_autonomy_check
	CHECK ("autonomy" IN ('auto', 'approval'));
