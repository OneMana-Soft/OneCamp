-- A saved monday.com token would violate the narrower constraint, so it goes first.
DELETE FROM import_oauth_tokens WHERE provider = 'monday';
ALTER TABLE import_oauth_tokens DROP CONSTRAINT IF EXISTS import_oauth_tokens_provider_check;
ALTER TABLE import_oauth_tokens ADD CONSTRAINT import_oauth_tokens_provider_check
    CHECK (provider IN ('asana','jira','trello','notion','todoist','linear','clickup'));
