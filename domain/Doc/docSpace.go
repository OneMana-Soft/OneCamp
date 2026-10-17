package domain

// A project's or a team's space, as the docs in it are shared: everyone in
// it, its members and its admins while it isn't deleted, reaches each of them
// (the access rule's space, read here from the space's side). One read serves
// a doc's own space (GetDocACL) and a space named by its uuid (GetSpace).

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// ErrNoSuchSpace is a space that isn't there: no live project or team with
// that uuid, or a kind of space that isn't a project's or a team's.
var ErrNoSuchSpace = errors.New("there's no such project or team")

// spaceBlocks reads the live project and the live team whose uuid is match
// (a query variable, or the val() of one): each one's name, and its members
// and admins.
func spaceBlocks(match string) string {
	return `
		project(func: eq(project_uuid, ` + match + `)) @filter(` + liveProjectFilter + `) {
			project_name
			project_members { user_uuid }
			project_admins { user_uuid }
		}
		team(func: eq(team_uuid, ` + match + `)) @filter(` + liveTeamFilter + `) {
			team_name
			team_members { user_uuid }
			team_admins { user_uuid }
		}`
}

// spaceRead is spaceBlocks' answer.
type spaceRead struct {
	Project []*dgraphStruct.DgraphProject `json:"project"`
	Team    []*dgraphStruct.DgraphTeam    `json:"team"`
}

// of is the space of kind ("project" or "team") in the answer: its name and
// who is in it (PeopleOf). ok is false when there's none.
func (r spaceRead) of(kind string) (name string, people []string, ok bool) {
	switch kind {
	case "project":
		if len(r.Project) > 0 && r.Project[0] != nil {
			return r.Project[0].Name, PeopleOf(r.Project[0].Members, r.Project[0].Admins), true
		}
	case "team":
		if len(r.Team) > 0 && r.Team[0] != nil {
			return r.Team[0].Name, PeopleOf(r.Team[0].Members, r.Team[0].Admins), true
		}
	}
	return "", nil, false
}

// GetSpace reads a live project's or team's space (kind "project" or "team"):
// its name, and who is in it (PeopleOf). ErrNoSuchSpace when there's none.
func GetSpace(ctx context.Context, kind, spaceUUID string) (name string, people []string, err error) {
	if (kind != "project" && kind != "team") || !helpers.IsCanonicalUUID(spaceUUID) {
		return "", nil, ErrNoSuchSpace
	}
	var out spaceRead
	q := `query space($id: string) {` + spaceBlocks("$id") + `
	}`
	if err := dgraphInit.ReadInto(ctx, q, map[string]string{"$id": spaceUUID}, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetSpace %s:%s err: %+v", kind, spaceUUID, err)
		return "", nil, err
	}
	name, people, ok := out.of(kind)
	if !ok {
		return "", nil, ErrNoSuchSpace
	}
	return name, people, nil
}

// PeopleOf is the uuids of the people on the lists, each once, in lower case
// and sorted: who is in a project or a team (its members and its admins).
func PeopleOf(lists ...[]*dgraphStruct.DgraphUser) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, u := range list {
			if u == nil || u.Uuid == "" {
				continue
			}
			if id := strings.ToLower(u.Uuid); !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}
