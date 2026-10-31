package Domain

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// MembersAmong is which of these people (user ids, matched in any case and
// answered as given) are in the project, as a member or an admin, and still
// active: one read of the graph, where asking HasMember of each was a read
// apiece.
func MembersAmong(ctx context.Context, projectUUID string, userUUIDs []string) (map[string]bool, error) {
	in, err := MembersAmongEach(ctx, map[string][]string{projectUUID: userUUIDs})
	if err != nil {
		return nil, err
	}
	return in[projectUUID], nil
}

// MembersAmongEach is MembersAmong for many projects at once: for each
// project asked about (keyed as asked), which of the people asked about it
// are in it. A read of the graph for each MembershipsPage projects, where
// MembersAmong per project was a read apiece.
func MembersAmongEach(ctx context.Context, asked map[string][]string) (map[string]map[string]bool, error) {
	out := make(map[string]map[string]bool, len(asked))
	projects := make([]string, 0, len(asked))
	for p, ids := range asked {
		out[p] = make(map[string]bool, len(ids))
		if len(ids) > 0 {
			projects = append(projects, strings.ToLower(p))
		}
	}
	people, err := helpers.LoadMap(ctx, projects, MembershipsPage, projectsPeople, func(p projectPeople) string { return p.project })
	if err != nil {
		return nil, err
	}
	for p, ids := range asked {
		in := people[strings.ToLower(p)].people
		for _, id := range ids {
			if in[strings.ToLower(strings.TrimSpace(id))] {
				out[p][id] = true
			}
		}
	}
	return out, nil
}

// projectPeople is a project's members and admins still active, by user id in
// lower case.
type projectPeople struct {
	project string
	people  map[string]bool
}

// projectsPeople reads the people of some projects (ids in lower case) in one
// query. Ids that aren't uuids are left out, and so is a project that doesn't
// exist.
func projectsPeople(ctx context.Context, projects []string) ([]projectPeople, error) {
	list := dgraphquery.UUIDList(projects)
	if list == "" {
		return nil, nil
	}
	type person struct {
		UUID string `json:"user_uuid"`
	}
	rows, err := dgexec.List[struct {
		UUID    string   `json:"project_uuid"`
		Members []person `json:"project_members"`
		Admins  []person `json:"project_admins"`
	}](ctx, `{
		p(func: eq(project_uuid, [`+list+`])) {
			project_uuid
			project_members @filter(`+dgraphquery.Live("user_deleted_at")+`) { user_uuid }
			project_admins @filter(`+dgraphquery.Live("user_deleted_at")+`) { user_uuid }
		}
	}`, nil, "p")
	if err != nil {
		return nil, err
	}
	byProject := make(map[string]map[string]bool, len(rows))
	for _, r := range rows {
		p := strings.ToLower(r.UUID)
		if byProject[p] == nil {
			byProject[p] = map[string]bool{}
		}
		for _, m := range append(r.Members, r.Admins...) {
			byProject[p][strings.ToLower(m.UUID)] = true
		}
	}
	out := make([]projectPeople, 0, len(byProject))
	for p, people := range byProject {
		out = append(out, projectPeople{project: p, people: people})
	}
	return out, nil
}
