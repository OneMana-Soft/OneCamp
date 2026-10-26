package Domain

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// MembersAmong is which of these people (user ids, matched in any case and
// answered as given) are in the project, as a member or an admin, and still
// active: one read of the graph, where asking HasMember of each was a read
// apiece.
func MembersAmong(ctx context.Context, projectUUID string, userUUIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(userUUIDs))
	if len(userUUIDs) == 0 {
		return out, nil
	}
	q := `query In($project: string){
		p(func: eq(project_uuid, $project)) {
			project_members @filter(` + dgraphquery.Live("user_deleted_at") + `) { user_uuid }
			project_admins @filter(` + dgraphquery.Live("user_deleted_at") + `) { user_uuid }
		}
	}`
	type person struct {
		UUID string `json:"user_uuid"`
	}
	res, err := dgexec.Read[struct {
		P []struct {
			Members []person `json:"project_members"`
			Admins  []person `json:"project_admins"`
		} `json:"p"`
	}](ctx, q, map[string]string{"$project": projectUUID})
	if err != nil {
		return nil, err
	}
	in := map[string]bool{}
	for _, p := range res.P {
		for _, m := range append(p.Members, p.Admins...) {
			in[strings.ToLower(m.UUID)] = true
		}
	}
	for _, id := range userUUIDs {
		if in[strings.ToLower(strings.TrimSpace(id))] {
			out[id] = true
		}
	}
	return out, nil
}
