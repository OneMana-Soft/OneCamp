package postgresInit

import (
	"context"
	"database/sql"
	"time"
)

// BulkTimeout is how long a transaction over a whole table may take: a
// column's cells converted or relabelled as the column changes. A statement
// for one row gets DBTimeout.
const BulkTimeout = time.Minute

// InTx runs fn in one transaction on the main pool within timeout, and
// commits it when fn returns nil; otherwise it rolls back.
func InTx(ctx context.Context, timeout time.Duration, fn func(ctx context.Context, tx *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tx, err := DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
