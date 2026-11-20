//go:build integration

package postgresInit

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ConnectTracedForTest is ConnectPostgres with tr seeing every statement the
// new pool sends, next to spaceTracer: the store-budget harness's Postgres
// tap. Only the pool it opens is traced; the hook is unset again before it
// returns. It exists only in the integration build, so no product code can
// reach it.
func ConnectTracedForTest(ctx context.Context, dsn string, tr pgx.QueryTracer) error {
	testTracer = tr
	defer func() { testTracer = nil }()
	return ConnectPostgres(ctx, dsn)
}
