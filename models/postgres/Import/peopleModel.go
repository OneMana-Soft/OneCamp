package models

// The people an import met, and the imports whose outcome their admin has not
// seen yet. Reads for business/Import/people.go and outcomes.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// ImportedPerson is one OneCamp user an import resolved a source person to,
// with what decides whether they can be invited.
type ImportedPerson struct {
	UserID      uuid.UUID
	Email       string
	Name        string
	IsExternal  bool
	IsBot       bool
	Deactivated bool
	// SourceBot and SourceLeft are what the import recorded about them in the
	// source (a bot account; someone deactivated there).
	SourceBot  bool
	SourceLeft bool
	// Invited is whether a LIVE invitation to their address exists
	// (userModels.InvitationLiveSQL): someone whose invitation expired, or was
	// used by an account since gone, is offered again.
	Invited bool
}

// ListImportedPeople is every person the import resolved, once per OneCamp
// user, in name order. Several source accounts can resolve to one user (two
// Slack accounts with the same address); they are one person.
func ListImportedPeople(ctx context.Context, importId uuid.UUID) ([]ImportedPerson, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT u.id, u.email_id,
		       COALESCE(NULLIF(u.display_name, ''), NULLIF(u.username, ''), '') AS name,
		       u.is_external, u.is_bot, u.deleted_at IS NOT NULL,
		       bool_or(m.metadata->>'is_bot' = 'true') AS source_bot,
		       bool_or(m.metadata->>'deleted' = 'true') AS source_left,
		       EXISTS (SELECT 1 FROM invitations i WHERE lower(i.email) = lower(u.email_id)
		               AND `+userModels.InvitationLiveSQL("i")+`) AS invited
		FROM import_id_map m
		JOIN users u ON u.id = m.onecamp_uuid
		WHERE m.import_id = $1 AND m.entity_type = $2
		GROUP BY u.id
		ORDER BY lower(COALESCE(NULLIF(u.display_name, ''), NULLIF(u.username, ''), u.email_id))`,
		importId, EntityUser)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ImportedPerson, 0, 32)
	for rows.Next() {
		var p ImportedPerson
		var sourceBot, sourceLeft sql.NullBool
		if err := rows.Scan(&p.UserID, &p.Email, &p.Name, &p.IsExternal, &p.IsBot, &p.Deactivated,
			&sourceBot, &sourceLeft, &p.Invited); err != nil {
			return nil, err
		}
		p.SourceBot, p.SourceLeft = sourceBot.Bool, sourceLeft.Bool
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListUnseenOutcomes is the imports someone started that finished or failed
// after since and whose outcome they have not dismissed since it happened,
// newest first. Only jobs that ran: one refused before it started told its
// admin then and there.
//
// The dismissal is progress.outcome_seen_at, compared with completed_at, which
// every terminal transition sets afresh: a job planned again after it failed
// and failing a second time is news again.
func ListUnseenOutcomes(ctx context.Context, userId uuid.UUID, since time.Time, limit int) ([]*Job, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT `+JobSelectColumns+` FROM import_jobs
		WHERE triggered_by = $1
		  AND status IN ('completed', 'failed')
		  AND started_at IS NOT NULL
		  AND completed_at > $2
		  AND (progress->>'outcome_seen_at' IS NULL
		       OR (progress->>'outcome_seen_at')::timestamptz < completed_at)
		ORDER BY completed_at DESC
		LIMIT $3`, userId, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Job, 0, limit)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// MarkOutcomeSeen records that the job's admin has seen how it ended. Reports
// whether the job is theirs: nobody dismisses someone else's news.
func MarkOutcomeSeen(ctx context.Context, jobId, userId uuid.UUID, at time.Time) (bool, error) {
	patch, _ := json.Marshal(map[string]string{"outcome_seen_at": at.UTC().Format(time.RFC3339Nano)})
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE import_jobs
		SET progress = COALESCE(progress, '{}'::jsonb) || $3::jsonb
		WHERE id = $1 AND triggered_by = $2`, jobId, userId, patch)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
