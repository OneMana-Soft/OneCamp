package dgfixture

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// DgraphSink sends each batch to a live Dgraph as one committed mutation.
type DgraphSink struct{ Client *dgo.Dgraph }

// Mutate commits one batch, trying an aborted one again (Dgraph's "please
// retry": a batch can meet the server's own background work).
func (s DgraphSink) Mutate(ctx context.Context, nquads []byte) (map[string]string, error) {
	var err error
	for try := 0; try < 5; try++ {
		var resp *api.Response
		resp, err = s.Client.NewTxn().Mutate(ctx, &api.Mutation{SetNquads: nquads, CommitNow: true})
		if err == nil {
			return resp.GetUids(), nil
		}
		if !strings.Contains(err.Error(), "aborted") && !strings.Contains(err.Error(), "retry") {
			return nil, err
		}
		time.Sleep(time.Duration(try+1) * 200 * time.Millisecond)
	}
	return nil, err
}

// Write builds the tier's workspace in a live Dgraph (holding the real
// schema, and nothing of another fixture's) and writes the Postgres rows the
// measured reads look up beside it.
func Write(ctx context.Context, t Tier, seed uint64, dg *dgo.Dgraph, db *sql.DB) (*Fixture, error) {
	f, err := Generate(ctx, t, seed, DgraphSink{Client: dg})
	if err != nil {
		return nil, err
	}
	if db != nil {
		if err := f.WritePostgres(ctx, db); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// WritePostgres writes the rows the graph's reads are joined with in
// Postgres: a users row for every person (sign-in, admin and profile reads
// start there), and the board project's own statuses (a board reads them
// first, and each has a column of its own). The other tables those reads
// touch (doc grant end dates, GitHub links of tasks) are empty in a new
// workspace too, and stay empty here.
func (f *Fixture) WritePostgres(ctx context.Context, db *sql.DB) error {
	const users = 500
	for from := 0; from < len(f.People); from += users {
		to := min(from+users, len(f.People))
		var q strings.Builder
		q.WriteString(`INSERT INTO users (id, email_id, username, display_name, is_admin, created_at, updated_at) VALUES `)
		args := make([]any, 0, (to-from)*5)
		for i, p := range f.People[from:to] {
			if i > 0 {
				q.WriteString(", ")
			}
			n := len(args)
			fmt.Fprintf(&q, "($%d, $%d, $%d, $%d, $%d, NOW(), NOW())", n+1, n+2, n+3, n+4, n+5)
			args = append(args, p.UUID, p.Email, p.Name, p.Name, false) // the viewer is a member, not an admin
		}
		if _, err := db.ExecContext(ctx, q.String(), args...); err != nil {
			return fmt.Errorf("dgfixture: users: %w", err)
		}
	}
	for i, id := range f.CustomStatus {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_statuses (id, project_id, name, category, position, created_by) VALUES ($1, $2, $3, $4, $5, $6)`,
			id, f.BoardProject, fmt.Sprintf("Custom %d", i), statuses[i%4], i, f.Viewer.UUID); err != nil {
			return fmt.Errorf("dgfixture: task_statuses: %w", err)
		}
	}
	return nil
}
