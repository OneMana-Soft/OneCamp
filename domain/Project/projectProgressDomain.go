package Domain

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// ProjectProgress is a project as a goal counts it: how much of its work is
// done, and the reader's place in it, since a goal names a project only to
// the people in it.
type ProjectProgress struct {
	UUID      string     `json:"project_uuid"`
	Name      string     `json:"project_name"`
	Open      int        `json:"open"`
	Done      int        `json:"done"`
	Overdue   int        `json:"overdue"`
	IsMember  int        `json:"is_member"`
	IsAdmin   int        `json:"is_admin"`
	DeletedAt *time.Time `json:"project_deleted_at,omitempty"`
}

// Visible reports whether the reader is in the project.
func (p ProjectProgress) Visible() bool { return p.IsMember > 0 || p.IsAdmin > 0 }

// Archived reports whether the project was archived (deleted).
func (p ProjectProgress) Archived() bool {
	return p.DeletedAt != nil && p.DeletedAt.After(time.Unix(0, 0))
}

// dqlUUIDList is ids as a DQL list literal: ["…", "…"]. It takes parsed
// UUIDs only, so nothing from a request reaches the query text unchecked. Pure.
func dqlUUIDList(ids []uuid.UUID) string {
	parts := make([]string, 0, len(ids))
	seen := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		if id == uuid.Nil || seen[id] {
			continue
		}
		seen[id] = true
		parts = append(parts, `"`+id.String()+`"`)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// GetDgraphProjectsProgress reads the given projects' task counts (open, done,
// overdue as of today) and the reader's place in each, by project uuid, in one
// query. A project that no longer exists is absent; an archived one carries
// DeletedAt. Counts are of live tasks, as the projects overview counts them.
func GetDgraphProjectsProgress(ctx context.Context, ids []uuid.UUID, readerUID string, today time.Time) (map[string]ProjectProgress, error) {
	out := map[string]ProjectProgress{}
	list := dqlUUIDList(ids)
	if list == "[]" {
		return out, nil
	}
	const live = dgraphStruct.TASK_LIVE_FILTER
	open := live + ` AND ` + dgraphStruct.TASK_OPEN_FILTER
	query := `query Progress($user: string, $today: string){
			p(func: eq(project_uuid, ` + list + `)) {
				project_uuid
				project_name
				project_deleted_at
				is_member: count(project_members @filter(uid($user)))
				is_admin: count(project_admins @filter(uid($user)))
				open: count(project_tasks @filter(` + open + `))
				done: count(project_tasks @filter(` + live + ` AND eq(task_status, "` + dgraphStruct.TASK_STATUS_DONE + `")))
				overdue: count(project_tasks @filter(` + open + ` AND lt(task_due_date, $today) AND gt(task_due_date, "1970-01-01T00:00:00Z")))
			}
		}`
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, query, map[string]string{
		"$user": readerUID, "$today": today.UTC().Format(time.RFC3339),
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphProjectsProgress err: %+v", err)
		return nil, err
	}
	var res struct {
		P []ProjectProgress `json:"p"`
	}
	if err := json.Unmarshal(resp.Json, &res); err != nil {
		return nil, err
	}
	for _, p := range res.P {
		if p.UUID != "" {
			out[p.UUID] = p
		}
	}
	return out, nil
}
