package models

// The repair of what old imports let every member of their projects read
// (import_access_reviews and import_access_changes, migration 511): one review
// per import, project and gap, and each change a re-check made, with what was
// there before, so an admin can undo it. Each move of a review is one
// statement guarded by the states it leaves, so of two servers, or an admin
// and the re-check, at once, one moves it.

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
)

// The gaps a review is of: what an import brought before its provider's fix
// that the source keeps to some people.
const (
	GapJiraSecurity = "jira_security" // issues with a security level
	GapJiraComments = "jira_comments" // comments kept to a role or group, and internal notes
	GapLinearTeams  = "linear_teams"  // issues of teams Linear doesn't show everyone
)

// Where a review stands.
const (
	StateRechecking = "rechecking" // the source is being read again
	StateRechecked  = "rechecked"  // read again, and narrowed where the source keeps things to some people
	StateNeedsAdmin = "needs_admin"
	StateKept       = "kept"      // an admin keeps it as it is
	StateContained  = "contained" // an admin keeps its tasks to the project's admins until it is re-checked
	StateUndone     = "undone"    // an admin undid its re-check
)

// What a re-check changed.
const (
	ChangeRestricted    = "restricted"
	ChangeUnassigned    = "unassigned"
	ChangeCommentHidden = "comment_hidden"
)

// AccessCandidate is one live project of an import that ran before its
// provider's fix, and why its review needs an admin from the start ("" when
// the import's connection can be tried: owner_gone when whoever ran it can't
// act any more, no_connection when they have no stored connection).
type AccessCandidate struct {
	ImportID    uuid.UUID
	Provider    string
	ProjectUUID uuid.UUID
	Reason      string
}

// AccessCandidates reads the provider's imports whose first run started
// before firstRunBefore (nil counts every one), leaving out those rolled back,
// each with each project it made or mapped, archived ones too: a restore
// brings their tasks back as the re-check left them (ruling G).
func AccessCandidates(ctx context.Context, provider string, firstRunBefore *time.Time) ([]AccessCandidate, error) {
	return pgexec.Query(ctx, `
		SELECT DISTINCT j.id, j.provider, pm.onecamp_uuid,
			CASE WHEN u.id IS NULL OR u.deleted_at IS NOT NULL THEN 'owner_gone'
			     WHEN t.owner_user_id IS NULL THEN 'no_connection'
			     ELSE '' END
		FROM import_jobs j
		JOIN import_id_map pm ON pm.import_id = j.id AND pm.entity_type = 'project'
		JOIN projects p ON p.id = pm.onecamp_uuid
		LEFT JOIN users u ON u.id = j.triggered_by
		LEFT JOIN import_oauth_tokens t ON t.provider = j.provider AND t.owner_user_id = j.triggered_by
		WHERE j.provider = $1 AND j.status <> 'rolled_back'
		  AND ($2::timestamptz IS NULL OR COALESCE(j.started_at, j.created_at) < $2)
		ORDER BY j.id, pm.onecamp_uuid`,
		func(r pgexec.Row) (AccessCandidate, error) {
			var c AccessCandidate
			err := r.Scan(&c.ImportID, &c.Provider, &c.ProjectUUID, &c.Reason)
			return c, err
		}, provider, firstRunBefore)
}

// accessBatch is how many reviews one INSERT writes: six binds a review.
const accessBatch = 1000

// AddAccessReviews records a review of each gap for each candidate, once:
// rechecking, or needs_admin with the candidate's reason. A second start, or
// a second server at once, adds nothing. It answers how many were new.
func AddAccessReviews(ctx context.Context, cs []AccessCandidate, gaps []string) (int64, error) {
	type row struct {
		c   AccessCandidate
		gap string
	}
	var rows []row
	for _, c := range cs {
		for _, g := range gaps {
			rows = append(rows, row{c, g})
		}
	}
	var added int64
	for batch := range slices.Chunk(rows, accessBatch) {
		args := make([]any, 0, len(batch)*6)
		for _, r := range batch {
			state, summary := StateRechecking, []byte(`{}`)
			if r.c.Reason != "" {
				state = StateNeedsAdmin
				var err error
				if summary, err = json.Marshal(map[string]string{"reason": r.c.Reason}); err != nil {
					return added, err
				}
			}
			args = append(args, r.c.ImportID, r.c.Provider, r.c.ProjectUUID, r.gap, state, summary)
		}
		n, err := pgexec.Exec(ctx, `INSERT INTO import_access_reviews (import_id, provider, project_uuid, gap, state, summary) VALUES `+
			helpers.RowPlaceholders(len(batch), 6)+` ON CONFLICT (import_id, project_uuid, gap) DO NOTHING`, args...)
		added += n
		if err != nil {
			return added, err
		}
	}
	return added, nil
}

// AccessReview is one review: an import's project and gap, where it stands,
// and its counts (Summary, the re-check's).
type AccessReview struct {
	ID          uuid.UUID       `json:"id"`
	ImportID    uuid.UUID       `json:"import_id"`
	Provider    string          `json:"provider"`
	ProjectUUID uuid.UUID       `json:"project_uuid"`
	Gap         string          `json:"gap"`
	State       string          `json:"state"`
	Summary     json.RawMessage `json:"summary"`
	DecidedBy   *uuid.UUID      `json:"decided_by,omitempty"`
	DecidedAt   *time.Time      `json:"decided_at,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// AccessReviewsOf reads an import's reviews, by project and gap.
func AccessReviewsOf(ctx context.Context, importID uuid.UUID) ([]AccessReview, error) {
	return pgexec.Query(ctx, `
		SELECT id, import_id, provider, project_uuid, gap, state, summary, decided_by, decided_at, updated_at
		FROM import_access_reviews WHERE import_id = $1 ORDER BY project_uuid, gap`,
		func(r pgexec.Row) (AccessReview, error) {
			var a AccessReview
			err := r.Scan(&a.ID, &a.ImportID, &a.Provider, &a.ProjectUUID, &a.Gap, &a.State, &a.Summary,
				&a.DecidedBy, &a.DecidedAt, &a.UpdatedAt)
			return a, err
		}, importID)
}

// ImportsInState reads the imports of the providers with a review in state,
// oldest review first.
func ImportsInState(ctx context.Context, providers []string, state string) ([]uuid.UUID, error) {
	return pgexec.Query(ctx, `
		SELECT import_id FROM import_access_reviews WHERE provider = ANY($1) AND state = $2
		GROUP BY import_id ORDER BY MIN(created_at), import_id`,
		func(r pgexec.Row) (uuid.UUID, error) {
			var id uuid.UUID
			err := r.Scan(&id)
			return id, err
		}, pq.Array(providers), state)
}

// MoveAccessReviews moves an import's reviews that are in one of from to
// state to, adding patch to their summaries, and answers how many moved. by
// is the admin who decided it, nil for the re-check.
func MoveAccessReviews(ctx context.Context, importID uuid.UUID, from []string, to string, patch map[string]any, by *uuid.UUID) (int64, error) {
	raw := []byte("{}")
	if patch != nil {
		var err error
		if raw, err = json.Marshal(patch); err != nil {
			return 0, err
		}
	}
	return pgexec.Exec(ctx, `
		UPDATE import_access_reviews
		SET state = $3, summary = summary || $4::jsonb, updated_at = NOW(),
			decided_by = COALESCE($5, decided_by), decided_at = CASE WHEN $5::uuid IS NULL THEN decided_at ELSE NOW() END
		WHERE import_id = $1 AND state = ANY($2)`, importID, pq.Array(from), to, raw, by)
}

// SetAccessSummaries writes reviews' counts (summaries, by review) in one
// statement, to each review still being re-checked.
func SetAccessSummaries(ctx context.Context, summaries map[uuid.UUID]any) error {
	raw, err := json.Marshal(summaries)
	if err != nil {
		return err
	}
	_, err = pgexec.Exec(ctx, `UPDATE import_access_reviews SET summary = $1::jsonb -> id::text, updated_at = NOW()
		WHERE id = ANY($2::uuid[]) AND state = $3`, raw, pq.Array(helpers.UUIDStrings(slices.Collect(maps.Keys(summaries)))), StateRechecking)
	return err
}

// AccessChange is one change a re-check, or an admin's containment, made: of
// what kind, to which task or comment, what was there before and what after,
// and when.
type AccessChange struct {
	ID        uuid.UUID       `json:"id"`
	ReviewID  uuid.UUID       `json:"review_id"`
	Kind      string          `json:"kind"`
	Entity    uuid.UUID       `json:"entity_uuid"`
	Before    json.RawMessage `json:"before"`
	After     json.RawMessage `json:"after"`
	CreatedAt time.Time       `json:"created_at"`
}

// AddAccessChange records a change before it is made, once per review, kind
// and task or comment, and answers whether this call recorded it: whoever
// records it makes it, so of two at once one does.
func AddAccessChange(ctx context.Context, reviewID uuid.UUID, kind string, entity uuid.UUID, before, after any) (bool, error) {
	b, err := json.Marshal(before)
	if err != nil {
		return false, err
	}
	a, err := json.Marshal(after)
	if err != nil {
		return false, err
	}
	n, err := pgexec.Exec(ctx, `
		INSERT INTO import_access_changes (review_id, kind, entity_uuid, before, after) VALUES ($1, $2, $3, $4::jsonb, $5::jsonb)
		ON CONFLICT (review_id, kind, entity_uuid) DO NOTHING`, reviewID, kind, entity, b, a)
	return n == 1, err
}

// SetAccessChangeAfter writes what a change leaves now that a later re-check
// lets people into a task an earlier one, or an admin's containment, kept.
func SetAccessChangeAfter(ctx context.Context, changeID uuid.UUID, after any) error {
	a, err := json.Marshal(after)
	if err != nil {
		return err
	}
	_, err = pgexec.Exec(ctx, `UPDATE import_access_changes SET after = $2::jsonb WHERE id = $1 AND undone_at IS NULL`, changeID, a)
	return err
}

// SettleAccessChange records that the review's change of kind to entity,
// recorded pending before it was made, is made.
func SettleAccessChange(ctx context.Context, reviewID uuid.UUID, kind string, entity uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE import_access_changes SET after = after - 'pending'
		WHERE review_id = $1 AND kind = $2 AND entity_uuid = $3 AND undone_at IS NULL AND after ? 'pending'`, reviewID, kind, entity)
	return err
}

// AccessChangesOf reads the changes made for an import's reviews that
// nobody has undone, oldest first.
func AccessChangesOf(ctx context.Context, importID uuid.UUID) ([]AccessChange, error) {
	return pgexec.Query(ctx, `
		SELECT c.id, c.review_id, c.kind, c.entity_uuid, COALESCE(c.before, 'null'), COALESCE(c.after, 'null'), c.created_at
		FROM import_access_changes c JOIN import_access_reviews r ON r.id = c.review_id
		WHERE r.import_id = $1 AND c.undone_at IS NULL
		ORDER BY c.created_at, c.id`,
		func(r pgexec.Row) (AccessChange, error) {
			var c AccessChange
			err := r.Scan(&c.ID, &c.ReviewID, &c.Kind, &c.Entity, &c.Before, &c.After, &c.CreatedAt)
			return c, err
		}, importID)
}

// UndoAccessChanges records that changes were put back, and answers how
// many this call recorded: of two undoes at once, one records each.
func UndoAccessChanges(ctx context.Context, changeIDs []uuid.UUID) (int64, error) {
	if len(changeIDs) == 0 {
		return 0, nil
	}
	ids := make([]string, len(changeIDs))
	for i, id := range changeIDs {
		ids[i] = id.String()
	}
	return pgexec.Exec(ctx, `UPDATE import_access_changes SET undone_at = NOW() WHERE id = ANY($1::uuid[]) AND undone_at IS NULL`, pq.Array(ids))
}
