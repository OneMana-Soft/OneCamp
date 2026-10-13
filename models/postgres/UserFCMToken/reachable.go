package models

import (
	"context"
	"database/sql"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/lib/pq"
)

// ReachableRow is one person the query let through, with one of their
// devices' tokens, or "" when they have none.
type ReachableRow struct {
	UserID string
	Token  string
}

// GetReachableUsersAndTokens runs query (domain/UserFCMToken reachable*:
// $1 is the people asked about, as uuids) and reads its (user id, token)
// rows.
func GetReachableUsersAndTokens(ctx context.Context, query string, userIDs []string) (out []ReachableRow, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, pq.Array(userIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ReachableRow
		var token sql.NullString
		if err = rows.Scan(&r.UserID, &token); err != nil {
			return nil, err
		}
		r.Token = token.String
		out = append(out, r)
	}
	// A read that stops part way must not pass for a short list: that would
	// tell some people and quietly drop the rest.
	return out, rows.Err()
}
