// Package business (Release) is Releases
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #98 and D-PJ-16; B10 builds
// it): a release with its tasks, a target date, stages reported from CI,
// notes drafted from its tasks [AI], and a changelog page that is off unless
// an admin publishes it.
//
// DAY 0. There are no releases: List answers none and Get ErrNotBuilt.
// Nothing calls Releases yet.
package business

import (
	"context"
	"errors"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/google/uuid"
)

// Release is one release of a project's work.
type Release struct {
	ID        uuid.UUID  `json:"id"`
	ProjectID uuid.UUID  `json:"project_id"`
	Name      string     `json:"name"`
	Target    *time.Time `json:"target,omitempty"`
	Stage     string     `json:"stage,omitempty"`
	TaskIDs   []string   `json:"task_ids,omitempty"`
}

// Book keeps releases.
type Book interface {
	List(ctx context.Context, who accessBusiness.Subject, projectID uuid.UUID) ([]Release, error)
	Get(ctx context.Context, who accessBusiness.Subject, id uuid.UUID) (*Release, error)
}

// ErrNotBuilt is releases before B10.
var ErrNotBuilt = errors.New("releases: that isn't built yet")

// Releases is the book everything asks.
var Releases Book = noReleases{}

type noReleases struct{}

func (noReleases) List(context.Context, accessBusiness.Subject, uuid.UUID) ([]Release, error) {
	return nil, nil
}

func (noReleases) Get(context.Context, accessBusiness.Subject, uuid.UUID) (*Release, error) {
	return nil, ErrNotBuilt
}
