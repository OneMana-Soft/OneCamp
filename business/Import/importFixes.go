package business

import (
	"context"
	"database/sql"
	"errors"
	"time"

	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// The keys an install records each importer fix's moment under. A later
// release reads each record by its key (FixedAt), so the keys are pinned
// (tests/integration import_fixes_claimed_test.go).
const (
	// MembershipFixed: Todoist and Notion make a project's members the people
	// it is shared with and no one else.
	MembershipFixed = "imports.membership_fixed"
	// JiraRestrictionsFixed: Jira keeps an issue with a security level to the
	// people the level lets in, and leaves comments it keeps to some people in
	// Jira.
	JiraRestrictionsFixed = "imports.jira_restrictions_fixed"
	// BasecampClientsFixed: Basecamp makes no client a member.
	BasecampClientsFixed = "imports.basecamp_clients_fixed"
	// LinearPrivateTeamsFixed: Linear brings a non-public team's issues in
	// restricted.
	LinearPrivateTeamsFixed = "imports.linear_private_teams_fixed"
)

// importFixes are the importer fixes whose moment an install records: when it
// first started a server that has the fix. An import whose first run started
// before its fix's moment came in under the old rule, so an admin may want to
// look it over, and what it brought can't be trusted to keep its source's
// restrictions.
var importFixes = []struct{ key, what string }{
	{MembershipFixed, "Todoist and Notion imports add only the people each project is shared with"},
	{JiraRestrictionsFixed, "Jira imports keep issue security levels, and leave comments Jira keeps to some people in Jira"},
	{BasecampClientsFixed, "Basecamp imports add no client as a member"},
	{LinearPrivateTeamsFixed, "Linear imports bring non-public teams' issues in restricted"},
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

// FixedAt is when ClaimImportFixes first recorded the fix under key on this
// install. ok is false while no server has claimed it: every import came
// before.
func FixedAt(key string) (at time.Time, ok bool, err error) {
	cfg, err := configModel.GetConfigByKey(key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, err
	}
	return cfg.UpdatedAt, true, nil
}

// FixedBefore is FixedAt's moment as the bound on imports that came before
// the fix: nil while no server has claimed it, when every import did.
func FixedBefore(key string) (*time.Time, error) {
	at, ok, err := FixedAt(key)
	if err != nil || !ok {
		return nil, err
	}
	return &at, nil
}
