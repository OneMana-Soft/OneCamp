package business

import (
	"context"

	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// importFixes are the importer fixes whose moment an install records: when it
// first started a server that has the fix. An import whose first run started
// before its fix's moment came in under the old rule, so an admin may want to
// look it over, and what it brought can't be trusted to keep its source's
// restrictions. A later release reads each record by its key, so the keys are
// pinned (tests/integration import_fixes_claimed_test.go).
var importFixes = []struct{ key, what string }{
	// Todoist and Notion make a project's members the people it is shared
	// with and no one else.
	{"imports.membership_fixed", "Todoist and Notion imports add only the people each project is shared with"},
	// Jira keeps an issue with a security level to the people the level
	// lets in, and leaves comments it keeps to some people in Jira.
	{"imports.jira_restrictions_fixed", "Jira imports keep issue security levels, and leave comments Jira keeps to some people in Jira"},
	// Basecamp makes no client a member.
	{"imports.basecamp_clients_fixed", "Basecamp imports add no client as a member"},
	// Linear brings a non-public team's issues in restricted.
	{"imports.linear_private_teams_fixed", "Linear imports bring non-public teams' issues in restricted"},
}

// ClaimImportFixes records each fix's moment once per install, as a server
// starts and before it runs an import. Later starts leave the first standing,
// and its time is the row's own (the database's clock, as a job's started_at).
func ClaimImportFixes(ctx context.Context) error {
	for _, f := range importFixes {
		if _, err := configModel.Claim(ctx, f.key, f.what); err != nil {
			return err
		}
	}
	return nil
}
