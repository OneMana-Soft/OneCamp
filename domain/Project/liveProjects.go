package Domain

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// LiveProjects is which of the projects are there and not deleted, by id, in
// one read of the graph (the Cycle Scheduler leaves a deleted project's
// cycles alone).
func LiveProjects(ctx context.Context, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	got, err := dgexec.List[struct {
		UUID string `json:"project_uuid"`
	}](ctx, `{ p(func: eq(project_uuid, `+string(list)+`)) @filter(`+dgraphquery.Live("project_deleted_at")+`) { project_uuid } }`, nil, "p")
	if err != nil {
		return nil, err
	}
	for _, p := range got {
		out[p.UUID] = true
	}
	return out, nil
}
