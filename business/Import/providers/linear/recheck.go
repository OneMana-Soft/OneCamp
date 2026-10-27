package linear

import (
	"context"

	"github.com/google/uuid"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// recheckIssuesQuery reads issues by id with what a re-check needs of each:
// its team, whose visibility keeps it, its project and its people. Fifty a
// page stays far under Linear's complexity cap.
const recheckIssuesQuery = `
query ($ids: [ID!], $cursor: String) {
  issues(first: 50, after: $cursor, includeArchived: true, filter: { id: { in: $ids } }) {
    nodes { id identifier team { id } project { id } assignee { id } creator { id } }
    pageInfo { hasNextPage endCursor }
  }
}`

// Recheck reads each issue's team, project and people as Linear has them
// now, with owner's stored connection: the teams once (which Linear doesn't
// show everyone, and their members: keptTeams), then the issues by id. An
// issue Linear doesn't return (deleted, or in a team owner can't see) is
// gone. The rules are the import's own: issueToSourceTask and keptToTeam.
// Linear keeps no comment from some of an issue's readers, so none is read.
func (p *Provider) Recheck(ctx context.Context, owner uuid.UUID, tasks []string, _ map[string][]string) (map[string]importProvider.RecheckedTask, error) {
	tok, err := tokenFor(ctx, owner)
	if err != nil {
		return nil, err
	}
	return p.recheck(ctx, tok, tasks)
}

// recheck is Recheck with the connection's token read.
func (p *Provider) recheck(ctx context.Context, tok string, ids []string) (map[string]importProvider.RecheckedTask, error) {
	teams, err := p.fetchTeams(ctx, tok)
	if err != nil {
		return nil, err
	}
	kept := keptTeams(teams)
	out := make(map[string]importProvider.RecheckedTask, len(ids))
	for _, id := range ids {
		out[id] = importProvider.RecheckedTask{Gone: true}
	}
	for cursor, page := "", 0; page < maxPages; page++ {
		var resp struct {
			Issues struct {
				Nodes []struct {
					ID         string `json:"id"`
					Identifier string `json:"identifier"`
					Team       *ref   `json:"team"`
					Project    *ref   `json:"project"`
					Assignee   *ref   `json:"assignee"`
					Creator    *ref   `json:"creator"`
				} `json:"nodes"`
				Page pageInfo `json:"pageInfo"`
			} `json:"issues"`
		}
		vars := map[string]any{"ids": ids}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		if err := p.gql(ctx, tok, recheckIssuesQuery, vars, &resp); err != nil {
			return nil, err
		}
		for _, n := range resp.Issues.Nodes {
			iss := linearIssue{ID: n.ID, Identifier: n.Identifier, TeamID: n.Team.id(), ProjectID: n.Project.id(),
				AssigneeID: n.Assignee.id(), CreatorID: n.Creator.id()}
			// The project the import put it in: its own, or its team's inbox.
			project := iss.ProjectID
			if project == "" {
				project = inboxProjectIDPrefix + iss.TeamID
			}
			out[n.ID] = importProvider.RecheckedTask{Task: keptToTeam(p.issueToSourceTask(iss, project), kept, iss.TeamID)}
		}
		if !resp.Issues.Page.HasNextPage || resp.Issues.Page.EndCursor == "" {
			break
		}
		cursor = resp.Issues.Page.EndCursor
	}
	return out, nil
}

// ref is a node Linear names by its id alone.
type ref struct {
	ID string `json:"id"`
}

// id is the node's id, "" for none.
func (r *ref) id() string {
	if r == nil {
		return ""
	}
	return r.ID
}
