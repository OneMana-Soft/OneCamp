package business

import (
	"context"

	domain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// LiveOnly is the live ones among the projects, in their order, for a list
// someone picks a project from when it comes from a record that names their
// projects without saying which are archived (the auth middleware's, which
// /user/profile returns). It is one read of the graph through the shared
// zero-epoch filter (domain LiveProjects). It narrows what is offered, never
// what anyone may reach, so a failed read keeps the list as it was, logged.
func LiveOnly(ctx context.Context, projects []*dgraphStruct.DgraphProject) []*dgraphStruct.DgraphProject {
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		if p != nil && p.Uuid != "" {
			ids = append(ids, p.Uuid)
		}
	}
	live, err := domain.LiveProjects(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/LiveOnly: which projects are live: %v", err)
		return projects
	}
	out := make([]*dgraphStruct.DgraphProject, 0, len(ids))
	for _, p := range projects {
		if p != nil && live[p.Uuid] {
			out = append(out, p)
		}
	}
	return out
}
