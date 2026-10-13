package models

import (
	"context"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// QueryBool runs a query that answers one boolean (domain/Webhook scope.go).
func QueryBool(query string, args ...any) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	var ok bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, args...).Scan(&ok)
	return ok, err
}
