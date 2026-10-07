package business

import (
	"context"
	"sort"
	"strings"
	"time"

	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/ProjectUpdate"
	"github.com/google/uuid"
)

// Overview is one of a person's projects as the projects overview shows it:
// where its tasks stand, and the health its latest update gave.
type Overview struct {
	projectDomain.ProjectCounts
	Health    string     `json:"health,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Overviews is every project the person is in, by name. now is in their
// zone: "overdue" and "due this week" are counted in their days, as the line
// under a project's name counts them. A project's latest update gives its
// health; when updates can't be read, the projects still show, without one.
func Overviews(ctx context.Context, userDgraphUID string, now time.Time) ([]Overview, error) {
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	counts, err := projectDomain.GetDgraphProjectCounts(ctx, userDgraphUID, today, today.AddDate(0, 0, 7))
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(counts))
	for _, c := range counts {
		if id, err := uuid.Parse(c.UUID); err == nil {
			ids = append(ids, id)
		}
	}
	latest, err := model.Latest(ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ProjectUpdate/Overviews updates err: %+v", err)
	}
	out := make([]Overview, 0, len(counts))
	for _, c := range counts {
		o := Overview{ProjectCounts: c}
		if id, err := uuid.Parse(c.UUID); err == nil {
			if u, ok := latest[id]; ok {
				o.Health, o.UpdatedAt = u.Health, &u.CreatedAt
			}
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}
