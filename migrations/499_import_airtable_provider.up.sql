-- Airtable joins the live-API import providers (tables T-AT): its personal
-- access token is stored like the others, so the token table's provider
-- CHECK must admit it (as 187 did for monday.com). Basecamp's migration and
-- the whiteboard's Miro one (W-2) rewrite this same CHECK on their own
-- branches, so each lists all three: whichever lands last keeps every
-- provider. business/Import/providers TestTheTokenCheckAdmitsEveryProvider
-- holds the last one to every registered provider.
ALTER TABLE import_oauth_tokens DROP CONSTRAINT IF EXISTS import_oauth_tokens_provider_check;
ALTER TABLE import_oauth_tokens ADD CONSTRAINT import_oauth_tokens_provider_check
    CHECK (provider IN ('asana','jira','trello','notion','todoist','linear','clickup','monday','basecamp','airtable','miro'));
