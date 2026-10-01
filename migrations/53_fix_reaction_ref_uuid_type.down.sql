-- Revert reaction_uuid back to uuid (data loss for non-UUID strings)
ALTER TABLE github_reaction_refs ALTER COLUMN reaction_uuid TYPE UUID USING NULL;
ALTER TABLE github_reaction_refs ALTER COLUMN reaction_uuid SET NOT NULL;
