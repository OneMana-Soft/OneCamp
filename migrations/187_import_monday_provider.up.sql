-- monday.com joins the live-API import providers. Its personal API token is
-- stored like the others, so the token table's provider CHECK must admit it.
-- The constraint was declared inline in migration 60, so Postgres named it
-- import_oauth_tokens_provider_check.
ALTER TABLE import_oauth_tokens DROP CONSTRAINT IF EXISTS import_oauth_tokens_provider_check;
ALTER TABLE import_oauth_tokens ADD CONSTRAINT import_oauth_tokens_provider_check
    CHECK (provider IN ('asana','jira','trello','notion','todoist','linear','clickup','monday'));
