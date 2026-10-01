// Discoverer is an optional interface a provider implements to expose a
// "list what this token can see" endpoint to the FE. The Operator-side
// flow becomes:
//
//  1. Operator connects (token saved encrypted).
//  2. FE calls /admin/import/{provider}/discover.
//  3. Provider lists Trello boards / Asana workspaces+projects / Jira
//     projects / Notion task databases / Todoist projects accessible
//     to that token.
//  4. Operator picks one. The picked id is passed as the
//     `board_id` / `workspace_gid` / `project_key` / `database_id`
//     option when creating the job.
//
// Providers that don't need discovery (e.g., Slack, where the
// workspace is fixed by the export ZIP) simply don't implement this
// interface; the controller returns 404 for the discover route.
package provider

import (
	"context"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// DiscoverItem is one resource returned by Discoverer.Discover.
// Shape is purposely flat so the FE can render a simple list.
type DiscoverItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	// Kind discriminates what `ID` is meant for: "board", "workspace",
	// "project", "database", etc. The FE renders a label per kind.
	Kind string `json:"kind"`
	// Meta carries any provider-specific extras (member counts, colours,
	// closed flag) the FE may surface but the BE doesn't otherwise use.
	Meta map[string]any `json:"meta,omitempty"`
}

// Discoverer is implemented by providers that can list resources for a
// connected admin without an existing job row. Optional: the controller
// reflects on this interface and returns 404 when not implemented.
type Discoverer interface {
	Discover(ctx context.Context, ownerUserId string, token *importModels.Token) ([]DiscoverItem, error)
}
