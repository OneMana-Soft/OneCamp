package models

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// TaskView is a saved set of task-list filters, sort and columns (migration 176).
//
// A view its person deletes is kept, marked by them and when (V-184 head 5a,
// the owner's rule that every delete of a business record is soft). Every
// read here sees live views only, and a name is unique among a person's live
// views in a scope, so saving a deleted view's name again makes a new view.
type TaskView struct {
	Id        uuid.UUID       `json:"id"`
	Scope     string          `json:"scope"`
	Name      string          `json:"name"`
	State     json.RawMessage `json:"state"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

const viewColumns = `id, scope, name, state, updated_at`

func scanView(row pgexec.Row) (TaskView, error) {
	var v TaskView
	err := row.Scan(&v.Id, &v.Scope, &v.Name, &v.State, &v.UpdatedAt)
	return v, err
}

// List returns the person's views for a scope, by name.
func List(userID uuid.UUID, scope string) ([]TaskView, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	out, err := pgexec.Query(ctx, `
		SELECT `+viewColumns+` FROM task_views
		 WHERE user_id = $1 AND scope = $2 AND `+softDelete.Live("")+` ORDER BY lower(name)`, scanView, userID, scope)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []TaskView{}
	}
	return out, nil
}

// Count is how many views the person has in a scope.
func Count(userID uuid.UUID, scope string) (int, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return pgexec.Scalar[int](ctx,
		`SELECT COUNT(*) FROM task_views WHERE user_id = $1 AND scope = $2 AND `+softDelete.Live(""), userID, scope)
}

// Save stores a view, replacing the person's live view of the same name in
// the scope. The conflict names the unique among live views, so a deleted
// view of that name stays as it was deleted.
func Save(userID uuid.UUID, scope, name string, state json.RawMessage) (*TaskView, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	v, ok, err := pgexec.QueryOne(ctx, `
		INSERT INTO task_views (user_id, scope, name, state) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, scope, name) WHERE `+softDelete.Live("")+` DO UPDATE SET state = EXCLUDED.state, updated_at = NOW()
		RETURNING `+viewColumns, scanView, userID, scope, name, []byte(state))
	if err == nil && !ok {
		err = errors.New("task view: the save returned no row")
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Exists says whether the person already has a live view of this name in the scope.
func Exists(userID uuid.UUID, scope, name string) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return pgexec.Scalar[bool](ctx,
		`SELECT EXISTS (SELECT 1 FROM task_views WHERE user_id = $1 AND scope = $2 AND name = $3 AND `+softDelete.Live("")+`)`,
		userID, scope, name)
}

// Delete takes one of the person's views away: it is kept, marked by them and
// when. False when it wasn't theirs, or was taken away already.
func Delete(userID, id uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	n, err := pgexec.Exec(ctx, `
		UPDATE task_views SET deleted_at = NOW(), deleted_by = $3, updated_at = NOW()
		 WHERE id = $1 AND user_id = $2 AND `+softDelete.Live(""), id, userID, softDelete.By(userID))
	return n > 0, err
}

// Owner is a saved view's person, and whether they deleted it.
type Owner struct {
	Person  uuid.UUID
	Deleted bool
}

// OwnersOf is each saved view's owner, by the view's id, live or deleted: the
// access rules answer a deleted view as none with what it was, as they do a
// deleted doc or task (business/Access, I9). A view that isn't there is left
// out.
func OwnersOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Owner, error) {
	out := make(map[uuid.UUID]Owner, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	type owned struct {
		id    uuid.UUID
		owner Owner
	}
	rows, err := pgexec.Query(ctx, `SELECT id, user_id, NOT (`+softDelete.Live("")+`) FROM task_views WHERE id = ANY($1)`,
		func(r pgexec.Row) (owned, error) {
			var o owned
			err := r.Scan(&o.id, &o.owner.Person, &o.owner.Deleted)
			return o, err
		}, ids)
	if err != nil {
		return nil, err
	}
	for _, o := range rows {
		out[o.id] = o.owner
	}
	return out, nil
}

// LiveByID is each live view of ids, by its id, whoever's it is: callers read
// only the views the access rules let them (business/Dashboard reads a
// widget's view as its reader). A deleted view, and one that isn't there,
// is left out.
func LiveByID(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]TaskView, error) {
	out := make(map[uuid.UUID]TaskView, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := pgexec.Query(ctx, `SELECT `+viewColumns+` FROM task_views WHERE id = ANY($1) AND `+softDelete.Live(""), scanView, ids)
	if err != nil {
		return nil, err
	}
	for _, v := range rows {
		out[v.Id] = v
	}
	return out, nil
}
