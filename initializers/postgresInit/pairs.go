package postgresInit

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// errNoDatabase is a read with no database to read.
var errNoDatabase = errors.New("postgres: not connected")

// UUIDPairs runs query, which takes the ids as one array ($1) and answers two
// uuid columns, and answers the second column by the first: each saved view's
// owner, each cycle's project. No ids, no query; an id with no row is left
// out.
func UUIDPairs(ctx context.Context, query string, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := map[uuid.UUID]uuid.UUID{}
	if len(ids) == 0 {
		return out, nil
	}
	if DBConn == nil || DBConn.SqlDB == nil {
		return nil, errNoDatabase
	}
	ctx, cancel := context.WithTimeout(ctx, DBConn.DBTimeout)
	defer cancel()
	rows, err := DBConn.SqlDB.QueryContext(ctx, query, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value uuid.UUID
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}
