package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// MentionSync is what keeping a thing's mentions found.
type MentionSync struct {
	// Added is the people its body newly mentions: mentioned now and not at the
	// last keeping.
	Added []string
	// First says its mentions had never been kept before (resource_mention_syncs).
	First bool
	// Since is when its kind's mentions began to be kept
	// (resource_mention_tracking); zero when that isn't known.
	Since time.Time
}

// SyncMentions makes the stored set of people the thing's body mentions equal
// to now, and answers who of them is new, whether this is the thing's first
// keeping, and since when its kind is kept (migration 344). Ids that aren't
// uuids are left out. Two saves at once can't both call one person new, or both
// be the first: the insert that adds a row is the one that answers it.
func SyncMentions(ctx context.Context, kind, resourceUUID string, now []string) (sync MentionSync, err error) {
	ids := make([]string, 0, len(now))
	seen := map[string]bool{}
	for _, s := range now {
		u, perr := uuid.Parse(s)
		if perr != nil || seen[u.String()] {
			continue
		}
		seen[u.String()] = true
		ids = append(ids, u.String())
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(c, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow SyncMentions begin err: %+v", err)
		return MentionSync{}, err
	}
	defer func() { _ = tx.Rollback() }()
	first, err := tx.ExecContext(c,
		`INSERT INTO resource_mention_syncs (resource_kind, resource_uuid) VALUES ($1, $2)
		 ON CONFLICT (resource_kind, resource_uuid) DO NOTHING`, kind, resourceUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow SyncMentions first keeping err: %+v", err)
		return MentionSync{}, err
	}
	if n, nerr := first.RowsAffected(); nerr == nil && n == 1 {
		sync.First = true
	}
	var since sql.NullTime
	if err = tx.QueryRowContext(c, `SELECT since FROM resource_mention_tracking WHERE resource_kind = $1`, kind).Scan(&since); err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx, "models/Follow SyncMentions since err: %+v", err)
		return MentionSync{}, err
	}
	if since.Valid {
		sync.Since = since.Time
	}
	if _, err = tx.ExecContext(c,
		`DELETE FROM resource_mentions WHERE resource_kind = $1 AND resource_uuid = $2 AND NOT (user_uuid = ANY($3::uuid[]))`,
		kind, resourceUUID, ids); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow SyncMentions delete err: %+v", err)
		return MentionSync{}, err
	}
	if len(ids) > 0 {
		rows, qerr := tx.QueryContext(c,
			`INSERT INTO resource_mentions (resource_kind, resource_uuid, user_uuid, mentioned_at)
			 SELECT $1, $2, u, NOW() FROM unnest($3::uuid[]) AS u
			 ON CONFLICT (resource_kind, resource_uuid, user_uuid) DO NOTHING
			 RETURNING user_uuid`,
			kind, resourceUUID, ids)
		if qerr != nil {
			helpers.LogErrorWithContext(ctx, "models/Follow SyncMentions insert err: %+v", qerr)
			return MentionSync{}, qerr
		}
		for rows.Next() {
			var u uuid.UUID
			if err = rows.Scan(&u); err != nil {
				rows.Close()
				return MentionSync{}, err
			}
			sync.Added = append(sync.Added, u.String())
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return MentionSync{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Follow SyncMentions commit err: %+v", err)
		return MentionSync{}, err
	}
	return sync, nil
}
