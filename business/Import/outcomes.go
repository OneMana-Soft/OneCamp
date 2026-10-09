package business

// Telling an admin how their import ended.
//
// An import runs for minutes or hours after the admin pressed Start, and it
// used to end silently: the only way to learn it had finished, or had stopped
// on an expired token halfway through, was to go back to the import screen and
// look. The admin banner asks here for the imports this admin started that
// finished or failed since they last looked, and offers the people who came
// across in the same breath. Dismissing one is remembered on the job, so it is
// dismissed on every device the admin uses.

import (
	"context"
	"time"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// outcomeWindow is how long an unseen outcome stays news. An admin back from
// two weeks away has no use for "your import finished" about one they have
// long since worked with.
const outcomeWindow = 14 * 24 * time.Hour

// maxOutcomes bounds the banner's list.
const maxOutcomes = 5

// ImportOutcome is how one import ended, for the admin who started it.
type ImportOutcome struct {
	JobID         uuid.UUID `json:"job_id"`
	Provider      string    `json:"provider"`
	Label         string    `json:"label"`
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	ItemsImported int       `json:"items_imported"`
	FinishedAt    time.Time `json:"finished_at"`
	// PeopleToInvite is how many people came across who can be invited now;
	// zero for an import that failed (it is offered once the import finishes).
	PeopleToInvite int `json:"people_to_invite"`
}

// OutcomesFor is how this admin's recent imports ended, the ones they have not
// dismissed, newest first.
func OutcomesFor(ctx context.Context, userId uuid.UUID, now time.Time) ([]ImportOutcome, error) {
	jobs, err := importModels.ListUnseenOutcomes(ctx, userId, now.Add(-outcomeWindow), maxOutcomes)
	if err != nil {
		return nil, err
	}
	out := make([]ImportOutcome, 0, len(jobs))
	for _, j := range jobs {
		o := ImportOutcome{JobID: j.Id, Provider: j.Provider, Label: j.SourceWorkspaceName, Status: j.Status}
		if j.CompletedAt != nil {
			o.FinishedAt = *j.CompletedAt
		}
		if j.ErrorMessage != nil {
			o.Error = *j.ErrorMessage
		}
		o.ItemsImported, _ = importModels.SumItemsImported(ctx, j.Id)
		if j.Status == importModels.StatusCompleted {
			// A failed count leaves the offer at zero rather than losing the
			// news that the import finished.
			if people, err := PeopleToInvite(ctx, j.Id); err == nil {
				o.PeopleToInvite = len(people.People)
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// MarkOutcomeSeen dismisses how a job ended, for the admin who started it.
// Reports false when the job is not theirs (or is gone).
func MarkOutcomeSeen(ctx context.Context, jobId, userId uuid.UUID, now time.Time) (bool, error) {
	return importModels.MarkOutcomeSeen(ctx, jobId, userId, now)
}
