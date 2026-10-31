-- Miro joins the live-API import providers (miro-37, the whiteboard's W-2): a
-- team's boards come over drawn. It is connected by a token the admin pastes
-- (owner decision O3), stored like the others, so the token table's provider
-- CHECK must admit it. Airtable is listed too (the tables lane's own provider
-- migration rewrites this same CHECK), so whichever of the two lands last
-- keeps both.
ALTER TABLE import_oauth_tokens DROP CONSTRAINT IF EXISTS import_oauth_tokens_provider_check;
ALTER TABLE import_oauth_tokens ADD CONSTRAINT import_oauth_tokens_provider_check
    CHECK (provider IN ('asana','jira','trello','notion','todoist','linear','clickup','monday','basecamp','airtable','miro'));
