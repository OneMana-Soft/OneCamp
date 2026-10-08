// Package models (TaskField) stores a project's custom task fields and each
// task's values for them (migration 192). Field queries are scoped to a
// project; values are keyed by task and field.
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Option is one choice of a select or multi-select field. Values hold its id,
// so a rename shows everywhere.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Color string `json:"color"`
}

// The field types, as the app names them (migration 192 says what a value of
// each is). Here rather than in business/TaskField so an importer can name them
// without that package's dependencies.
const (
	TypeText        = "text"
	TypeNumber      = "number"
	TypeMoney       = "money"
	TypeDate        = "date"
	TypeSelect      = "select"
	TypeMultiSelect = "multi_select"
	TypePerson      = "person"
	TypeCheckbox    = "checkbox"
	TypeURL         = "url"
)

// Field is one of a project's custom fields.
type Field struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Options   []Option  `json:"options"`
	Currency  string    `json:"currency,omitempty"`
	OnCard    bool      `json:"on_card"`
	Position  int       `json:"position"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var (
	ErrNotFound  = errors.New("task field not found")
	ErrNameTaken = errors.New("a field with that name already exists in this project")
)

const columns = `id, project_id, name, type, options, COALESCE(currency, ''), on_card, position, created_at, updated_at`

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

func scan(row interface{ Scan(...any) error }) (*Field, error) {
	var f Field
	var options []byte
	if err := row.Scan(&f.ID, &f.ProjectID, &f.Name, &f.Type, &options, &f.Currency, &f.OnCard, &f.Position, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return nil, err
	}
	f.Options = []Option{}
	if len(options) > 0 {
		if err := json.Unmarshal(options, &f.Options); err != nil {
			return nil, err
		}
	}
	return &f, nil
}

// isUniqueViolation recognises Postgres error 23505 from either driver.
func isUniqueViolation(err error) bool {
	var withState interface{ SQLState() string }
	if errors.As(err, &withState) {
		return withState.SQLState() == "23505"
	}
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

func nullCurrency(c string) any {
	if c == "" {
		return nil
	}
	return c
}

// List is a project's fields in their saved order.
func List(ctx context.Context, projectID uuid.UUID) ([]*Field, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT `+columns+` FROM task_fields WHERE project_id = $1 ORDER BY position, created_at`, projectID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskField List err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	out := []*Field{}
	for rows.Next() {
		f, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Get is one of a project's fields.
func Get(ctx context.Context, projectID, id uuid.UUID) (*Field, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	f, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT `+columns+` FROM task_fields WHERE id = $1 AND project_id = $2`, id, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

// Count is how many fields a project has.
func Count(ctx context.Context, projectID uuid.UUID) (int, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `SELECT COUNT(*) FROM task_fields WHERE project_id = $1`, projectID).Scan(&n)
	return n, err
}

// Create adds a field at the end of the project's order.
func Create(ctx context.Context, f *Field, createdBy uuid.UUID) (*Field, error) {
	options, err := json.Marshal(f.Options)
	if err != nil {
		return nil, err
	}
	c, cancel := withTimeout(ctx)
	defer cancel()
	out, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		INSERT INTO task_fields (id, project_id, name, type, options, currency, on_card, position, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE((SELECT MAX(position) + 1 FROM task_fields WHERE project_id = $2), 0), $8)
		RETURNING `+columns, uuid.New(), f.ProjectID, f.Name, f.Type, options, nullCurrency(f.Currency), f.OnCard, createdBy))
	if isUniqueViolation(err) {
		return nil, ErrNameTaken
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskField Create err: %+v", err)
	}
	return out, err
}

// Update changes a field's name, options, currency and whether cards show it,
// and takes options it no longer has off its tasks, in one transaction with the
// field locked, so no option is ever gone from the field and left on a task. Its
// type never changes: values of one type mean nothing as another. It returns
// the tasks whose values changed.
func Update(ctx context.Context, f *Field) (*Field, []string, error) {
	options, err := json.Marshal(f.Options)
	if err != nil {
		return nil, nil, err
	}
	c, cancel := withTimeout(ctx)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(c, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	var before []byte
	err = tx.QueryRowContext(c, `SELECT options FROM task_fields WHERE id = $1 AND project_id = $2 FOR UPDATE`, f.ID, f.ProjectID).Scan(&before)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	out, err := scan(tx.QueryRowContext(c, `
		UPDATE task_fields SET name = $3, options = $4, currency = $5, on_card = $6, updated_at = NOW()
		WHERE id = $1 AND project_id = $2 RETURNING `+columns, f.ID, f.ProjectID, f.Name, options, nullCurrency(f.Currency), f.OnCard))
	if isUniqueViolation(err) {
		return nil, nil, ErrNameTaken
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskField Update err: %+v", err)
		return nil, nil, err
	}
	var old []Option
	if err := json.Unmarshal(before, &old); err != nil {
		return nil, nil, err
	}
	kept := map[string]bool{}
	for _, o := range f.Options {
		kept[o.ID] = true
	}
	var removed []string
	for _, o := range old {
		if !kept[o.ID] {
			removed = append(removed, o.ID)
		}
	}
	changed, err := dropOptionValues(c, tx, f.ID, removed)
	if err != nil {
		return nil, nil, err
	}
	return out, changed, tx.Commit()
}

// AddOptions gives a field the options more returns for the ones it has. The
// field is locked between reading and writing, so options two writers add at
// once both stay; options are only ever added here, so no task loses a value.
func AddOptions(ctx context.Context, projectID, id uuid.UUID, more func(have []Option) []Option) (*Field, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(c, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	var raw []byte
	err = tx.QueryRowContext(c, `SELECT options FROM task_fields WHERE id = $1 AND project_id = $2 FOR UPDATE`, id, projectID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var have []Option
	if err := json.Unmarshal(raw, &have); err != nil {
		return nil, err
	}
	options, err := json.Marshal(append(have, more(have)...))
	if err != nil {
		return nil, err
	}
	out, err := scan(tx.QueryRowContext(c, `
		UPDATE task_fields SET options = $3, updated_at = CASE WHEN options = $3::jsonb THEN updated_at ELSE NOW() END
		WHERE id = $1 AND project_id = $2 RETURNING `+columns, id, projectID, options))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskField AddOptions err: %+v", err)
		return nil, err
	}
	return out, tx.Commit()
}

// Reorder sets positions from the order of ids. Ids not of this project are
// ignored; fields left out keep their place after the ones given.
func Reorder(ctx context.Context, projectID uuid.UUID, ids []uuid.UUID) error {
	c, cancel := withTimeout(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		UPDATE task_fields t SET position = o.ord, updated_at = NOW()
		FROM unnest($2::uuid[]) WITH ORDINALITY AS o(id, ord)
		WHERE t.id = o.id AND t.project_id = $1`, projectID, pq.Array(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskField Reorder err: %+v", err)
	}
	return err
}

// Delete removes one of a project's fields, and with it every task's value.
func Delete(ctx context.Context, projectID, id uuid.UUID) error {
	c, cancel := withTimeout(ctx)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(c, `DELETE FROM task_fields WHERE id = $1 AND project_id = $2`, id, projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// dropOptionValues takes options a field no longer has off its tasks, in tx: a
// select's value goes, and a multi-select keeps its other options (none left
// is no value). It returns the tasks it changed.
func dropOptionValues(c context.Context, tx *sql.Tx, fieldID uuid.UUID, removed []string) ([]string, error) {
	if len(removed) == 0 {
		return nil, nil
	}
	var changed []string
	collect := func(rows *sql.Rows, err error) error {
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t uuid.UUID
			if err := rows.Scan(&t); err != nil {
				return err
			}
			changed = append(changed, t.String())
		}
		return rows.Err()
	}
	if err := collect(tx.QueryContext(c, `
		DELETE FROM task_field_values WHERE field_id = $1 AND jsonb_typeof(value) = 'string' AND value #>> '{}' = ANY($2)
		RETURNING task_uuid`, fieldID, pq.Array(removed))); err != nil {
		return nil, err
	}
	if err := collect(tx.QueryContext(c, `
		UPDATE task_field_values v SET value = COALESCE((
			SELECT jsonb_agg(e) FROM jsonb_array_elements(v.value) e WHERE NOT (e #>> '{}' = ANY($2))), '[]'::jsonb), updated_at = NOW()
		WHERE field_id = $1 AND jsonb_typeof(value) = 'array' AND value ?| $2
		RETURNING task_uuid`, fieldID, pq.Array(removed))); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(c, `DELETE FROM task_field_values WHERE field_id = $1 AND value = '[]'::jsonb`, fieldID); err != nil {
		return nil, err
	}
	return changed, nil
}

// Values is each task's values: task id, then field id, then the value.
type Values map[string]map[string]json.RawMessage

// ValuesFor reads the values of the tasks listed.
func ValuesFor(ctx context.Context, taskIDs []uuid.UUID) (Values, error) {
	out := Values{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	c, cancel := withTimeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT task_uuid, field_id, value FROM task_field_values WHERE task_uuid = ANY($1)`, pq.Array(taskIDs))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskField ValuesFor err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var task, field uuid.UUID
		var value []byte
		if err := rows.Scan(&task, &field, &value); err != nil {
			return nil, err
		}
		t := task.String()
		if out[t] == nil {
			out[t] = map[string]json.RawMessage{}
		}
		out[t][field.String()] = value
	}
	return out, rows.Err()
}

// ValueOf is a task's value of a field, or nil when it has none.
func ValueOf(ctx context.Context, taskID, fieldID uuid.UUID) (json.RawMessage, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	var value []byte
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT value FROM task_field_values WHERE task_uuid = $1 AND field_id = $2`, taskID, fieldID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return value, err
}

// SetValue gives a task a value of a field, or takes it off (value nil).
func SetValue(ctx context.Context, taskID, fieldID uuid.UUID, value json.RawMessage, by uuid.UUID) error {
	c, cancel := withTimeout(ctx)
	defer cancel()
	var err error
	if value == nil {
		_, err = postgresInit.DBConn.SqlDB.ExecContext(c, `DELETE FROM task_field_values WHERE task_uuid = $1 AND field_id = $2`, taskID, fieldID)
	} else {
		_, err = postgresInit.DBConn.SqlDB.ExecContext(c, `
			INSERT INTO task_field_values (task_uuid, field_id, value, updated_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (task_uuid, field_id) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = NOW()`,
			taskID, fieldID, []byte(value), by)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskField SetValue err: %+v", err)
	}
	return err
}

// CopyValues gives to the values from has, for the fields to doesn't have a
// value of yet: a repeating task's next copy, made in the same project.
func CopyValues(ctx context.Context, from, to uuid.UUID, by uuid.UUID) error {
	c, cancel := withTimeout(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		INSERT INTO task_field_values (task_uuid, field_id, value, updated_by)
		SELECT $2, field_id, value, $3 FROM task_field_values WHERE task_uuid = $1
		ON CONFLICT (task_uuid, field_id) DO NOTHING`, from, to, by)
	return err
}

// Match is what a filter on a field asks for: tasks whose value is one of
// Values (an option id, a user id, or "true"), and, with Any, tasks with any
// value at all.
type Match struct {
	Values []string
	Any    bool
}

// TasksMatching is the tasks whose value of the field matches: a select's or
// person's value equal to one asked for, a multi-select holding one of them,
// a ticked checkbox for "true", or any value at all when m.Any.
func TasksMatching(ctx context.Context, fieldID uuid.UUID, m Match) ([]string, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT task_uuid FROM task_field_values WHERE field_id = $1 AND (
			$3
			OR (jsonb_typeof(value) = 'string' AND value #>> '{}' = ANY($2))
			OR (jsonb_typeof(value) = 'array' AND value ?| $2)
			OR (jsonb_typeof(value) = 'boolean' AND value #>> '{}' = ANY($2)))`,
		fieldID, pq.Array(m.Values), m.Any)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskField TasksMatching err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t uuid.UUID
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t.String())
	}
	return out, rows.Err()
}
