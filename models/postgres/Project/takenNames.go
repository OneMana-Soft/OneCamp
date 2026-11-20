package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// TakenProjectNames runs the domain's TakenProjectNamesQuery: which of these
// names a project in the team has, as a set; none asked reads nothing.
func TakenProjectNames(ctx context.Context, query string, team uuid.UUID, names []string) (map[string]bool, error) {
	taken := map[string]bool{}
	if len(names) == 0 {
		return taken, nil
	}
	found, err := pgexec.Query(ctx, query,
		func(r pgexec.Row) (n string, err error) { err = r.Scan(&n); return }, team, pq.Array(names))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TakenProjectNames err: %+v", err)
		return nil, err
	}
	for _, n := range found {
		taken[n] = true
	}
	return taken, nil
}
