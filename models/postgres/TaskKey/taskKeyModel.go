// Package models (TaskKey) keeps task keys and types (migration 471): each
// project's key and the number its next task takes, the keys a project used
// before, each task's number, and the types a task can have. business/TaskKeys
// and business/WorkTaxonomy decide; this reads and writes.
package models

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// ErrNoDatabase is a read or write with no database.
var ErrNoDatabase = errors.New("task keys: not connected")

// ErrKeyTaken is a key another project uses, or used before.
var ErrKeyTaken = errors.New("task keys: that key is taken")

// ErrNoKey is a project with no key yet.
var ErrNoKey = errors.New("task keys: the project has no key yet")

// ErrNoTask is a key that names no task.
var ErrNoTask = errors.New("task keys: no task has that key")

// ErrTypeNameTaken is a type name the project already has.
var ErrTypeNameTaken = errors.New("task types: the project already has a type of that name")

// ErrNoType is a type that isn't the project's or built in, or is archived.
var ErrNoType = errors.New("task types: there's no such type here")

func connected() bool {
	return postgresInit.DBConn != nil && postgresInit.DBConn.SqlDB != nil
}

func timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

// Type is a kind of task: built in (Builtin set, no project) or a project's
// own.
type Type struct {
	ID        uuid.UUID  `json:"id"`
	Builtin   string     `json:"builtin,omitempty"`
	Name      string     `json:"name"`
	Icon      string     `json:"icon"`
	Position  int        `json:"position"`
	ProjectID *uuid.UUID `json:"-"`
	Archived  bool       `json:"archived,omitempty"`
}

// Facts is what Postgres holds about a task beside the graph: its key, made
// of its project's key and its number ("" until both exist), and its type
// (nil for a plain Task).
type Facts struct {
	Key  string
	Type *Type
}

// FactsFor reads the facts of each of tasks that has a row, in one read.
// With no database at all (a unit test) nothing has facts.
func FactsFor(ctx context.Context, tasks []uuid.UUID) (map[uuid.UUID]Facts, error) {
	out := make(map[uuid.UUID]Facts, len(tasks))
	if len(tasks) == 0 || !connected() {
		return out, nil
	}
	c, cancel := timeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT t.id, k.key, t.task_number,
		       tt.id, tt.builtin, tt.name, tt.icon, tt.position, tt.project_id, tt.archived_at IS NOT NULL
		FROM tasks t
		LEFT JOIN project_keys k ON k.project_id = t.project_id
		LEFT JOIN task_types tt ON tt.id = t.task_type_id
		WHERE t.id = ANY($1)`, tasks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id                  uuid.UUID
			key                 sql.NullString
			number              sql.NullInt64
			typeID              uuid.NullUUID
			builtin, name, icon sql.NullString
			position            sql.NullInt64
			typeProject         uuid.NullUUID
			archived            sql.NullBool
		)
		if err := rows.Scan(&id, &key, &number, &typeID, &builtin, &name, &icon, &position, &typeProject, &archived); err != nil {
			return nil, err
		}
		var f Facts
		if key.Valid && number.Valid {
			f.Key = KeyOf(key.String, int(number.Int64))
		}
		if typeID.Valid && builtin.String != "task" {
			t := &Type{ID: typeID.UUID, Builtin: builtin.String, Name: name.String, Icon: icon.String, Position: int(position.Int64), Archived: archived.Bool}
			if typeProject.Valid {
				p := typeProject.UUID
				t.ProjectID = &p
			}
			f.Type = t
		}
		out[id] = f
	}
	return out, rows.Err()
}

// KeyOf is a task's key from its project's key and its number: "Q4-12".
func KeyOf(projectKey string, number int) string {
	return projectKey + "-" + strconv.Itoa(number)
}

// InsertTask makes a task's row and gives it the next number in its project,
// in one statement: concurrent tasks in a project take the row lock on its
// key in turn, so no two get the same number and none is skipped. A project
// with no key yet gives no number (0); the projects.keys sweep numbers it
// once the key exists.
func InsertTask(ctx context.Context, taskID, projectID, createdBy uuid.UUID, createdAt time.Time) (int, error) {
	if !connected() {
		return 0, ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	var number sql.NullInt64
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		WITH k AS (
			UPDATE project_keys SET next_number = next_number + 1
			WHERE project_id = $2
			RETURNING next_number - 1 AS n
		)
		INSERT INTO tasks (id, project_id, created_by, created_at, updated_at, task_number)
		VALUES ($1, $2, $3, $4, $4, (SELECT n FROM k))
		RETURNING task_number`, taskID, projectID, createdBy, createdAt).Scan(&number)
	if err != nil {
		return 0, err
	}
	return int(number.Int64), nil
}

// Key is a project's key and the number its next task takes; ErrNoKey when
// it has none yet.
func Key(ctx context.Context, projectID uuid.UUID) (string, int, error) {
	if !connected() {
		return "", 0, ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	var key string
	var next int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT key, next_number FROM project_keys WHERE project_id = $1`, projectID).Scan(&key, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, ErrNoKey
	}
	return key, next, err
}

// EnsureKey gives a project the first of candidates no other project uses or
// used before, numbering from after its highest numbered task, and answers
// the key it has: one it already had wins over every candidate. ErrKeyTaken
// when every candidate is taken; nothing when the project doesn't exist
// (ErrNoKey).
func EnsureKey(ctx context.Context, projectID uuid.UUID, candidates []string) (string, error) {
	if !connected() {
		return "", ErrNoDatabase
	}
	for _, candidate := range candidates {
		c, cancel := timeout(ctx)
		var got string
		err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `
			INSERT INTO project_keys (project_id, key, next_number)
			SELECT $1::uuid, $2::varchar, s.n
			FROM (SELECT COALESCE(MAX(task_number), 0) + 1 AS n FROM tasks WHERE project_id = $1::uuid) s
			WHERE EXISTS (SELECT 1 FROM projects WHERE id = $1::uuid)
			  AND NOT EXISTS (SELECT 1 FROM project_key_aliases a WHERE a.key = $2::varchar AND a.project_id <> $1::uuid)
			ON CONFLICT DO NOTHING
			RETURNING key`, projectID, candidate).Scan(&got)
		cancel()
		if err == nil {
			return got, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		// Nothing written: the project has a key already, the candidate is
		// another's, or there is no such project.
		key, _, err := Key(ctx, projectID)
		if err == nil {
			return key, nil
		}
		if !errors.Is(err, ErrNoKey) {
			return "", err
		}
		if !projectExists(ctx, projectID) {
			return "", ErrNoKey
		}
	}
	return "", ErrKeyTaken
}

// Taken is which of keys a project other than projectID uses, or used before.
func Taken(ctx context.Context, projectID uuid.UUID, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(keys) == 0 {
		return out, nil
	}
	if !connected() {
		return nil, ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT key FROM project_keys WHERE key = ANY($1) AND project_id <> $2
		UNION
		SELECT key FROM project_key_aliases WHERE key = ANY($1) AND project_id <> $2`, keys, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// ProjectName is a project's name; "" when there's no such project.
func ProjectName(ctx context.Context, projectID uuid.UUID) (string, error) {
	if !connected() {
		return "", ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	var name string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `SELECT project_name FROM projects WHERE id = $1`, projectID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

func projectExists(ctx context.Context, projectID uuid.UUID) bool {
	c, cancel := timeout(ctx)
	defer cancel()
	var one int
	return postgresInit.DBConn.SqlDB.QueryRowContext(c, `SELECT 1 FROM projects WHERE id = $1`, projectID).Scan(&one) == nil
}

// SetKey changes a project's key, by whom, and keeps the one it had as an
// alias, so links and messages that name its tasks still find them. A key
// another project uses or used is ErrKeyTaken; a project's own old alias can
// be taken back. Answers the key it had.
func SetKey(ctx context.Context, projectID uuid.UUID, key string, by uuid.UUID) (string, error) {
	if !connected() {
		return "", ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(c, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var old string
	err = tx.QueryRowContext(c, `SELECT key FROM project_keys WHERE project_id = $1 FOR UPDATE`, projectID).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoKey
	}
	if err != nil {
		return "", err
	}
	if old == key {
		return old, nil
	}
	var taken bool
	if err := tx.QueryRowContext(c, `
		SELECT EXISTS (SELECT 1 FROM project_keys WHERE key = $1 AND project_id <> $2)
		    OR EXISTS (SELECT 1 FROM project_key_aliases WHERE key = $1 AND project_id <> $2)`, key, projectID).Scan(&taken); err != nil {
		return "", err
	}
	if taken {
		return "", ErrKeyTaken
	}
	var setBy any
	if by != uuid.Nil {
		setBy = by
	}
	steps := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM project_key_aliases WHERE key = $1 AND project_id = $2`, []any{key, projectID}},
		{`INSERT INTO project_key_aliases (key, project_id) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`, []any{old, projectID}},
		{`UPDATE project_keys SET key = $1, set_by = $2, set_at = NOW() WHERE project_id = $3`, []any{key, setBy, projectID}},
	}
	for _, s := range steps {
		if _, err := tx.ExecContext(c, s.q, s.args...); err != nil {
			if helpers.IsUniqueViolation(err) {
				return "", ErrKeyTaken
			}
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		if helpers.IsUniqueViolation(err) {
			return "", ErrKeyTaken
		}
		return "", err
	}
	return old, nil
}

// TakeKey gives a project key, taking it from whichever project has it or had
// it, and drops the key the project had (it never takes an alias: nothing
// was ever named by it). The project that loses it gets a new key from the
// projects.keys sweep, its tasks keeping their numbers. For a project made to
// replace another (the demo's nightly refresh), never for people.
func TakeKey(ctx context.Context, projectID uuid.UUID, key string) error {
	if !connected() {
		return ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM project_key_aliases WHERE key = $2 AND project_id <> $1`,
		`DELETE FROM project_keys WHERE key = $2 AND project_id <> $1`,
		`DELETE FROM project_key_aliases WHERE key = $2 AND project_id = $1`,
	} {
		if _, err := tx.ExecContext(c, q, projectID, key); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(c, `UPDATE project_keys SET key = $2, set_at = NOW() WHERE project_id = $1`, projectID, key)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNoKey
	}
	return tx.Commit()
}

// Aliases is the keys a project used before, newest first.
func Aliases(ctx context.Context, projectID uuid.UUID) ([]string, error) {
	if !connected() {
		return nil, ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT key FROM project_key_aliases WHERE project_id = $1 ORDER BY retired_at DESC, key`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Resolve is the task a project key, current or old, and a number name.
func Resolve(ctx context.Context, projectKey string, number int) (uuid.UUID, error) {
	if !connected() {
		return uuid.Nil, ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	var id uuid.UUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		SELECT t.id FROM tasks t
		WHERE t.task_number = $2 AND t.project_id = COALESCE(
			(SELECT project_id FROM project_keys WHERE key = $1),
			(SELECT project_id FROM project_key_aliases WHERE key = $1))`, projectKey, number).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, ErrNoTask
	}
	return id, err
}

// Unkeyed is a project with no key yet: its id and name.
type Unkeyed struct {
	ID   uuid.UUID
	Name string
}

// ProjectsWithoutKeys is up to limit projects that have no key yet, oldest
// first.
func ProjectsWithoutKeys(ctx context.Context, limit int) ([]Unkeyed, error) {
	if !connected() {
		return nil, ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT p.id, p.project_name FROM projects p
		WHERE NOT EXISTS (SELECT 1 FROM project_keys k WHERE k.project_id = p.id)
		ORDER BY p.created_at, p.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Unkeyed
	for rows.Next() {
		var u Unkeyed
		if err := rows.Scan(&u.ID, &u.Name); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ProjectsWithUnnumberedTasks is up to limit projects that have a key and a
// task without a number.
func ProjectsWithUnnumberedTasks(ctx context.Context, limit int) ([]uuid.UUID, error) {
	if !connected() {
		return nil, ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT DISTINCT t.project_id FROM tasks t
		JOIN project_keys k ON k.project_id = t.project_id
		WHERE t.task_number IS NULL AND t.project_id IS NOT NULL
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// NumberUnnumbered gives a project's tasks without a number the next ones,
// oldest first, holding its key's row so a task made meanwhile waits its
// turn. Answers how many it numbered.
func NumberUnnumbered(ctx context.Context, projectID uuid.UUID) (int, error) {
	if !connected() {
		return 0, ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(c, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var next int
	err = tx.QueryRowContext(c, `SELECT next_number FROM project_keys WHERE project_id = $1 FOR UPDATE`, projectID).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoKey
	}
	if err != nil {
		return 0, err
	}
	// Never below a number already given.
	var highest int
	if err := tx.QueryRowContext(c, `SELECT COALESCE(MAX(task_number), 0) FROM tasks WHERE project_id = $1`, projectID).Scan(&highest); err != nil {
		return 0, err
	}
	if highest >= next {
		next = highest + 1
	}
	res, err := tx.ExecContext(c, `
		UPDATE tasks AS t SET task_number = $2 - 1 + u.rn
		FROM (SELECT id, ROW_NUMBER() OVER (ORDER BY created_at, id) AS rn
		      FROM tasks WHERE project_id = $1 AND task_number IS NULL) AS u
		WHERE t.id = u.id`, projectID, next)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(c, `UPDATE project_keys SET next_number = $2 WHERE project_id = $1`, projectID, next+int(n)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// Types is the types a task in project may have: the built-in ones, then the
// project's own in their order, archived ones left out unless withArchived.
func Types(ctx context.Context, projectID uuid.UUID, withArchived bool) ([]Type, error) {
	if !connected() {
		return nil, ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT id, COALESCE(builtin, ''), name, icon, position, project_id, archived_at IS NOT NULL
		FROM task_types
		WHERE (project_id IS NULL OR project_id = $1) AND ($2 OR archived_at IS NULL)
		ORDER BY project_id IS NOT NULL, position, lower(name)`, projectID, withArchived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Type
	for rows.Next() {
		var t Type
		var p uuid.NullUUID
		if err := rows.Scan(&t.ID, &t.Builtin, &t.Name, &t.Icon, &t.Position, &p, &t.Archived); err != nil {
			return nil, err
		}
		if p.Valid {
			id := p.UUID
			t.ProjectID = &id
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateType adds a type to a project, after its others.
func CreateType(ctx context.Context, projectID uuid.UUID, name, icon string, by uuid.UUID) (Type, error) {
	if !connected() {
		return Type{}, ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	t := Type{ID: uuid.New(), Name: name, Icon: icon, ProjectID: &projectID}
	var createdBy any
	if by != uuid.Nil {
		createdBy = by
	}
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		INSERT INTO task_types (id, project_id, name, icon, position, created_by)
		SELECT $1::uuid, $2::uuid, $3::varchar, $4::varchar, COALESCE(MAX(position), -1) + 1, $5::uuid FROM task_types WHERE project_id = $2::uuid
		RETURNING position`, t.ID, projectID, name, icon, createdBy).Scan(&t.Position)
	if helpers.IsUniqueViolation(err) {
		return Type{}, ErrTypeNameTaken
	}
	if err != nil {
		return Type{}, err
	}
	return t, nil
}

// UpdateType renames a project's type and changes its icon.
func UpdateType(ctx context.Context, projectID, typeID uuid.UUID, name, icon string) error {
	if !connected() {
		return ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`UPDATE task_types SET name = $3, icon = $4 WHERE id = $2 AND project_id = $1 AND archived_at IS NULL`,
		projectID, typeID, name, icon)
	if helpers.IsUniqueViolation(err) {
		return ErrTypeNameTaken
	}
	return rowsOrNoType(res, err)
}

// ArchiveType takes a project's type out of the pickers. The tasks that have
// it keep it, and still show it.
func ArchiveType(ctx context.Context, projectID, typeID uuid.UUID) error {
	if !connected() {
		return ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`UPDATE task_types SET archived_at = NOW() WHERE id = $2 AND project_id = $1 AND archived_at IS NULL`,
		projectID, typeID)
	return rowsOrNoType(res, err)
}

func rowsOrNoType(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNoType
	}
	return nil
}

// SetTaskType gives a task a type, or none (a Task) when typeID is nil: one
// built in or its project's own, never archived. Answers the type it had and
// the one it has now (nil for a Task).
func SetTaskType(ctx context.Context, taskID uuid.UUID, typeID *uuid.UUID) (before, after *Type, err error) {
	if !connected() {
		return nil, nil, ErrNoDatabase
	}
	c, cancel := timeout(ctx)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(c, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var projectID uuid.NullUUID
	var current uuid.NullUUID
	err = tx.QueryRowContext(c, `SELECT project_id, task_type_id FROM tasks WHERE id = $1 FOR UPDATE`, taskID).Scan(&projectID, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNoTask
	}
	if err != nil {
		return nil, nil, err
	}
	if current.Valid {
		if before, err = typeIn(c, tx, current.UUID); err != nil {
			return nil, nil, err
		}
	}
	var next any
	if typeID != nil {
		t, err := typeIn(c, tx, *typeID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNoType
		}
		if err != nil {
			return nil, nil, err
		}
		if t.Archived || (t.ProjectID != nil && (!projectID.Valid || *t.ProjectID != projectID.UUID)) {
			return nil, nil, ErrNoType
		}
		if t.Builtin != "task" {
			after = t
			next = t.ID
		}
	}
	if _, err := tx.ExecContext(c, `UPDATE tasks SET task_type_id = $2, updated_at = NOW() WHERE id = $1`, taskID, next); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	if before != nil && before.Builtin == "task" {
		before = nil
	}
	return before, after, nil
}

func typeIn(ctx context.Context, tx *sql.Tx, id uuid.UUID) (*Type, error) {
	var t Type
	var p uuid.NullUUID
	err := tx.QueryRowContext(ctx,
		`SELECT id, COALESCE(builtin, ''), name, icon, position, project_id, archived_at IS NOT NULL FROM task_types WHERE id = $1`, id).
		Scan(&t.ID, &t.Builtin, &t.Name, &t.Icon, &t.Position, &p, &t.Archived)
	if err != nil {
		return nil, err
	}
	if p.Valid {
		pid := p.UUID
		t.ProjectID = &pid
	}
	return &t, nil
}

// Describe is a type as a history line names it.
func (t *Type) Describe() string {
	if t == nil {
		return "Task"
	}
	return t.Name
}
