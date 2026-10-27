// Package models keeps the reviews of people old Todoist and Notion imports
// made members of their projects (import_member_reviews, migration 510).
package models

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
)

// What an admin decided about a review.
const (
	OutcomeRemoved = "removed"
	OutcomeKept    = "kept"
)

// Candidate is a person an old import made a member of one of its projects,
// before anyone checks that they are still in it.
type Candidate struct {
	ImportID    uuid.UUID
	Provider    string
	ProjectUUID uuid.UUID
	UserID      uuid.UUID
}

// Review is one person still in one project an old import made them a member
// of, and what an admin decided ("" until one does).
type Review struct {
	ID          uuid.UUID  `json:"id"`
	ImportID    uuid.UUID  `json:"import_id"`
	Provider    string     `json:"provider"`
	ProjectUUID uuid.UUID  `json:"project_uuid"`
	ProjectName string     `json:"project_name"`
	UserID      uuid.UUID  `json:"user_uuid"`
	UserName    string     `json:"user_name"`
	Outcome     string     `json:"outcome,omitempty"`
	DecidedBy   *uuid.UUID `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Candidates reads, for the providers' imports, each person an import matched
// with each live project it made or mapped, leaving out whoever ran the
// import and the project's creator (who can't be taken off it). With
// firstRunBefore set, only the imports whose first run started before it:
// those the importers ran before their fix (one never run, by when it was
// made). Nil counts every import.
func Candidates(ctx context.Context, providers []string, firstRunBefore *time.Time) ([]Candidate, error) {
	return pgexec.Query(ctx, `
		SELECT DISTINCT j.id, j.provider, pm.onecamp_uuid, um.onecamp_uuid
		FROM import_jobs j
		JOIN import_id_map pm ON pm.import_id = j.id AND pm.entity_type = 'project'
		JOIN projects p ON p.id = pm.onecamp_uuid AND p.deleted_at IS NULL
		JOIN import_id_map um ON um.import_id = j.id AND um.entity_type = 'user'
		JOIN users u ON u.id = um.onecamp_uuid
		WHERE j.provider = ANY($1)
		  AND ($2::timestamptz IS NULL OR COALESCE(j.started_at, j.created_at) < $2)
		  AND um.onecamp_uuid IS DISTINCT FROM j.triggered_by
		  AND um.onecamp_uuid IS DISTINCT FROM p.created_by
		ORDER BY j.id, pm.onecamp_uuid, um.onecamp_uuid`,
		func(r pgexec.Row) (Candidate, error) {
			var c Candidate
			err := r.Scan(&c.ImportID, &c.Provider, &c.ProjectUUID, &c.UserID)
			return c, err
		}, pq.Array(providers), firstRunBefore)
}

// addBatch is how many reviews one INSERT writes: at four binds a review, a
// batch stays far under Postgres's 65,535 binds in a statement.
const addBatch = 1000

// Add records a review for each candidate, once per person and project, in
// batches: a second pass, or a second server starting at once, adds nothing.
// It answers how many were new.
func Add(ctx context.Context, cs []Candidate) (int64, error) {
	var added int64
	for batch := range slices.Chunk(cs, addBatch) {
		args := make([]any, 0, len(batch)*4)
		for _, c := range batch {
			args = append(args, c.ImportID, c.Provider, c.ProjectUUID, c.UserID)
		}
		n, err := pgexec.Exec(ctx, `INSERT INTO import_member_reviews (import_id, provider, project_uuid, user_id) VALUES `+
			helpers.RowPlaceholders(len(batch), 4)+` ON CONFLICT (project_uuid, user_id) DO NOTHING`, args...)
		added += n
		if err != nil {
			return added, err
		}
	}
	return added, nil
}

const reviewColumns = `r.id, r.import_id, r.provider, r.project_uuid, COALESCE(p.project_name, ''), r.user_id,
	COALESCE(NULLIF(u.display_name, ''), u.username, ''), COALESCE(r.outcome, ''), r.decided_by, r.decided_at, r.created_at
	FROM import_member_reviews r
	LEFT JOIN projects p ON p.id = r.project_uuid
	LEFT JOIN users u ON u.id = r.user_id`

func scanReview(row pgexec.Row) (Review, error) {
	var r Review
	err := row.Scan(&r.ID, &r.ImportID, &r.Provider, &r.ProjectUUID, &r.ProjectName, &r.UserID, &r.UserName,
		&r.Outcome, &r.DecidedBy, &r.DecidedAt, &r.CreatedAt)
	return r, err
}

// ProjectReviews is one project's reviews, counted by what admins decided.
type ProjectReviews struct {
	ProjectUUID uuid.UUID `json:"project_uuid"`
	ProjectName string    `json:"project_name"`
	Providers   []string  `json:"providers"`
	Undecided   int       `json:"undecided"`
	Removed     int       `json:"removed"`
	Kept        int       `json:"kept"`
}

// Projects reads each project with reviews and its counts: those with someone
// undecided first, then by name.
func Projects(ctx context.Context) ([]ProjectReviews, error) {
	return pgexec.Query(ctx, `
		SELECT r.project_uuid, COALESCE(p.project_name, ''), array_agg(DISTINCT r.provider ORDER BY r.provider),
			COUNT(*) FILTER (WHERE r.outcome IS NULL),
			COUNT(*) FILTER (WHERE r.outcome = 'removed'),
			COUNT(*) FILTER (WHERE r.outcome = 'kept')
		FROM import_member_reviews r
		LEFT JOIN projects p ON p.id = r.project_uuid
		GROUP BY r.project_uuid, p.project_name
		ORDER BY COUNT(*) FILTER (WHERE r.outcome IS NULL) = 0, lower(COALESCE(p.project_name, '')), r.project_uuid`,
		func(row pgexec.Row) (ProjectReviews, error) {
			var pr ProjectReviews
			err := row.Scan(&pr.ProjectUUID, &pr.ProjectName, pq.Array(&pr.Providers), &pr.Undecided, &pr.Removed, &pr.Kept)
			return pr, err
		})
}

// PeoplePage is how many of a project's people one read answers.
const PeoplePage = 50

// listedName is the name a person's review is listed under.
const listedName = `lower(COALESCE(NULLIF(u.display_name, ''), u.username, ''))`

// People reads a page of a project's reviews by name, from after the review
// the last page ended on (nil for the first), and whether more follow. The
// order holds while admins decide, so no one is skipped or repeated.
func People(ctx context.Context, projectUUID uuid.UUID, after *uuid.UUID) ([]Review, bool, error) {
	revs, err := pgexec.Query(ctx, `SELECT `+reviewColumns+`
		WHERE r.project_uuid = $1
		  AND ($2::uuid IS NULL OR (`+listedName+`, r.id) > (
			SELECT lower(COALESCE(NULLIF(lu.display_name, ''), lu.username, '')), l.id
			FROM import_member_reviews l LEFT JOIN users lu ON lu.id = l.user_id
			WHERE l.id = $2))
		ORDER BY `+listedName+`, r.id
		LIMIT $3`, scanReview, projectUUID, after, PeoplePage+1)
	if err != nil {
		return nil, false, err
	}
	if len(revs) > PeoplePage {
		return revs[:PeoplePage], true, nil
	}
	return revs, false, nil
}

// Get reads one review; found is false when there is none.
func Get(ctx context.Context, id uuid.UUID) (r Review, found bool, err error) {
	return pgexec.QueryOne(ctx, `SELECT `+reviewColumns+` WHERE r.id = $1`, scanReview, id)
}

// Undecided reads up to limit of a project's reviews that no admin has
// decided yet.
func Undecided(ctx context.Context, projectUUID uuid.UUID, limit int) ([]Review, error) {
	return pgexec.Query(ctx, `SELECT `+reviewColumns+`
		WHERE r.project_uuid = $1 AND r.outcome IS NULL ORDER BY r.id LIMIT $2`, scanReview, projectUUID, limit)
}

// UndecidedIn counts a project's reviews that no admin has decided yet.
func UndecidedIn(ctx context.Context, projectUUID uuid.UUID) (int, error) {
	return pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM import_member_reviews WHERE project_uuid = $1 AND outcome IS NULL`, projectUUID)
}

// Decide records an admin's outcome on a review that is still undecided, and
// answers whether it was: of two admins deciding at once, one does.
func Decide(ctx context.Context, id uuid.UUID, outcome string, by uuid.UUID) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE import_member_reviews SET outcome = $2, decided_by = $3, decided_at = NOW()
		WHERE id = $1 AND outcome IS NULL`, id, outcome, by)
	return n == 1, err
}

// Undecide puts back a removal an admin claimed (Decide) whose taking off
// then failed, so the review is still theirs, or anyone's, to decide.
func Undecide(ctx context.Context, id uuid.UUID, by uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE import_member_reviews SET outcome = NULL, decided_by = NULL, decided_at = NULL
		WHERE id = $1 AND outcome = $2 AND decided_by = $3`, id, OutcomeRemoved, by)
	return err
}

// KeepAll records that an admin keeps everyone still undecided in a project,
// and answers how many that was.
func KeepAll(ctx context.Context, projectUUID uuid.UUID, by uuid.UUID) (int64, error) {
	return pgexec.Exec(ctx, `UPDATE import_member_reviews SET outcome = $2, decided_by = $3, decided_at = NOW()
		WHERE project_uuid = $1 AND outcome IS NULL`, projectUUID, OutcomeKept, by)
}

// Pending counts the reviews no admin has decided yet.
func Pending(ctx context.Context) (int, error) {
	return pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM import_member_reviews WHERE outcome IS NULL`)
}
