-- Google Sheets joins the live-API import providers (tables S1 slice 6): the
-- token an admin's Google sign-in gives is stored like the others, so the
-- token table's provider CHECK must admit it. The CHECK is rewritten whole,
-- naming every provider (business/Import/providers
-- TestTheTokenCheckAdmitsEveryProvider holds the last one to all of them).
ALTER TABLE import_oauth_tokens DROP CONSTRAINT IF EXISTS import_oauth_tokens_provider_check;
ALTER TABLE import_oauth_tokens ADD CONSTRAINT import_oauth_tokens_provider_check
    CHECK (provider IN ('asana','jira','trello','notion','todoist','linear','clickup','monday','basecamp','airtable','miro','gsheets'));
