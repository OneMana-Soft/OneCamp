package domain

// Who is in each DM and group, by its grouping id, read at once: the repair
// of replies a Slack import filed in another conversation's thread asks
// whether a reply's author is one of its conversation's people
// (business/SlackImport repair.go).

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// GroupingPeople is each grouping's people's uuids. A DM's are in its id
// (helpers.DMPeople); a group's are read from the graph.
func GroupingPeople(ctx context.Context, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	var groups []string
	for _, id := range ids {
		if a, b, dm := helpers.DMPeople(id); dm {
			out[id] = []string{a, b}
		} else {
			groups = append(groups, id)
		}
	}
	list, n := quotedList(groups, isGroupingID)
	if n == 0 {
		return out, nil
	}
	got, err := dgexec.Read[struct {
		Groups []struct {
			ID     string `json:"dm_grouping_id"`
			People []struct {
				UUID string `json:"user_uuid"`
			} `json:"dm_participants"`
		} `json:"groups"`
	}](ctx, `{ groups(func: eq(dm_grouping_id, `+list+`)) { dm_grouping_id dm_participants { user_uuid } } }`, nil)
	if err != nil {
		return nil, err
	}
	for _, g := range got.Groups {
		for _, p := range g.People {
			out[g.ID] = append(out[g.ID], p.UUID)
		}
	}
	return out, nil
}
