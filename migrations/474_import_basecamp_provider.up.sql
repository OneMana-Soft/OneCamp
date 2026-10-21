-- Basecamp joins the live-API import providers (basecamp-46). It is connected
-- by signing in through Launchpad, and its token is stored like the others, so
-- the token table's provider CHECK must admit it.
ALTER TABLE import_oauth_tokens DROP CONSTRAINT IF EXISTS import_oauth_tokens_provider_check;
ALTER TABLE import_oauth_tokens ADD CONSTRAINT import_oauth_tokens_provider_check
    CHECK (provider IN ('asana','jira','trello','notion','todoist','linear','clickup','monday','basecamp'));
