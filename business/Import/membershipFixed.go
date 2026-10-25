package business

import (
	"context"

	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// membershipFixedKey records when this install first started a server whose
// Todoist and Notion importers make a project's members the people it is
// shared with and no one else. An import whose first run started before then
// is one an admin may want to look over.
const membershipFixedKey = "imports.membership_fixed"

// ClaimMembershipFixed records that moment once per install, as a server
// starts and before it runs an import. Later starts leave the first standing,
// and its time is the row's own (the database's clock, as a job's started_at).
func ClaimMembershipFixed(ctx context.Context) error {
	_, err := configModel.Claim(ctx, membershipFixedKey, "Todoist and Notion imports add only the people each project is shared with")
	return err
}
