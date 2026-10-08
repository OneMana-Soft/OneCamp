// Package models (Goal) stores goals, the projects that serve them, and their
// check-ins (migration 188).
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// How a goal measures its progress.
const (
	MeasureProjects = "projects"
	MeasureSubgoals = "subgoals"
	MeasureNumber   = "number"
)

// Where a goal is: open, or closed as one of the three endings.
const (
	StatusOpen     = "open"
	StatusAchieved = "achieved"
	StatusMissed   = "missed"
	StatusDropped  = "dropped"
)

// DateLayout is how a goal's dates are written, in the database and the API.
const DateLayout = "2006-01-02"

// Goal is one goal.
type Goal struct {
	Id            uuid.UUID
	Title         string
	Description   string
	OwnerUUID     uuid.UUID
	CreatedBy     uuid.UUID
	ParentId      *uuid.UUID
	StartDate     *time.Time
	DueDate       time.Time
	Measure       string
	StartValue    *float64
	TargetValue   *float64
	CurrentValue  *float64
	Unit          string
	Status        string
	FinalProgress *float64
	ClosedAt      *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CheckIn is one check-in on a goal.
type CheckIn struct {
	Id         uuid.UUID `json:"id"`
	GoalId     uuid.UUID `json:"goal_id"`
	AuthorUUID uuid.UUID `json:"author_uuid"`
	Health     string    `json:"health"`
	Body       string    `json:"body"`
	Value      *float64  `json:"value,omitempty"`
	Progress   *float64  `json:"progress,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Node is a goal's place in the tree, for checking a parent change.
type Node struct {
	Id       uuid.UUID
	ParentId *uuid.UUID
}

var (
	// ErrTooManyProjects is a goal already served by the most projects allowed.
	ErrTooManyProjects = errors.New("goal has the most projects allowed")
	// ErrClosed is a check-in on a goal that closed before it landed.
	ErrClosed = errors.New("goal is closed")
)

// Limits on what one read returns.
const (
	// MaxGoals is the most live goals read at once: open ones first, then the
	// most recently closed. A goal's own page reads it by id however old it is.
	MaxGoals = 2000
	// MaxCheckIns is the most check-ins one read returns.
	MaxCheckIns = 50
)

const columns = `id, title, description, owner_uuid, created_by, parent_id, start_date, due_date, measure,
	start_value, target_value, current_value, unit, status, final_progress, closed_at, created_at, updated_at`

const checkInColumns = `id, goal_id, author_uuid, health, body, value, progress, created_at, updated_at`

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

type scanner interface{ Scan(...any) error }

func scan(row scanner) (*Goal, error) {
	var g Goal
	err := row.Scan(&g.Id, &g.Title, &g.Description, &g.OwnerUUID, &g.CreatedBy, &g.ParentId, &g.StartDate, &g.DueDate, &g.Measure,
		&g.StartValue, &g.TargetValue, &g.CurrentValue, &g.Unit, &g.Status, &g.FinalProgress, &g.ClosedAt, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &g, err
}

func scanCheckIn(row scanner) (*CheckIn, error) {
	var c CheckIn
	err := row.Scan(&c.Id, &c.GoalId, &c.AuthorUUID, &c.Health, &c.Body, &c.Value, &c.Progress, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// date is a goal date as Postgres reads it, or nil.
func date(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(DateLayout)
}

// treeLock serialises changes to the goal tree, so two parent changes made at
// once can't close a loop that neither saw.
const treeLock = `SELECT pg_advisory_xact_lock(hashtext('onecamp.goals.tree'))`

// tree is every live goal's place in the tree, read inside tx.
func tree(ctx context.Context, tx *sql.Tx) ([]Node, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, parent_id FROM goals WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Node{}
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.Id, &n.ParentId); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// inTree runs write in a transaction holding the tree lock, after check has
// approved the tree as it stands then. A nil check skips the lock.
func inTree(check func([]Node) error, write func(ctx context.Context, tx *sql.Tx) error) error {
	ctx, cancel := withTimeout()
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if check != nil {
		if _, err := tx.ExecContext(ctx, treeLock); err != nil {
			return err
		}
		nodes, err := tree(ctx, tx)
		if err != nil {
			return err
		}
		if err := check(nodes); err != nil {
			return err
		}
	}
	if err := write(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Create stores a new goal and links its first projects. check sees the tree
// (under the lock) when the goal has a parent.
func Create(g Goal, projects []uuid.UUID, check func([]Node) error) (*Goal, error) {
	var out *Goal
	if g.ParentId == nil {
		check = nil
	}
	err := inTree(check, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		out, err = scan(tx.QueryRowContext(ctx, `
			INSERT INTO goals (title, description, owner_uuid, created_by, parent_id, start_date, due_date, measure,
			                   start_value, target_value, current_value, unit)
			VALUES ($1, $2, $3, $4, $5, $6::date, $7::date, $8, $9, $10, $11, $12)
			RETURNING `+columns,
			g.Title, g.Description, g.OwnerUUID, g.CreatedBy, g.ParentId, date(g.StartDate), date(&g.DueDate), g.Measure,
			g.StartValue, g.TargetValue, g.CurrentValue, g.Unit))
		if err != nil {
			return err
		}
		for _, p := range projects {
			if _, err := tx.ExecContext(ctx, `INSERT INTO goal_projects (goal_id, project_uuid, added_by) VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING`, out.Id, p, g.CreatedBy); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// Update writes a goal's editable fields; nil when it is gone. The parent is
// written only when it changes, under the tree lock after check approves it,
// so an edit never undoes a move or a deletion that landed meanwhile.
func Update(g Goal, parentChanged bool, check func([]Node) error) (*Goal, error) {
	var out *Goal
	if !parentChanged {
		check = nil
	}
	err := inTree(check, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		out, err = scan(tx.QueryRowContext(ctx, `
			UPDATE goals SET title = $2, description = $3, owner_uuid = $4,
			       parent_id = CASE WHEN $13::boolean THEN $5::uuid ELSE parent_id END, start_date = $6::date,
			       due_date = $7::date, measure = $8, start_value = $9, target_value = $10, current_value = $11, unit = $12,
			       updated_at = NOW()
			 WHERE id = $1 AND deleted_at IS NULL
			RETURNING `+columns,
			g.Id, g.Title, g.Description, g.OwnerUUID, g.ParentId, date(g.StartDate), date(&g.DueDate), g.Measure,
			g.StartValue, g.TargetValue, g.CurrentValue, g.Unit, parentChanged))
		return err
	})
	return out, err
}

// Get is one live goal, or nil.
func Get(id uuid.UUID) (*Goal, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT `+columns+` FROM goals WHERE id = $1 AND deleted_at IS NULL`, id))
}

func list(query string, args ...any) ([]Goal, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Goal{}
	for rows.Next() {
		g, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// ListLive is the live goals, at most MaxGoals: every open one first, then
// the most recently closed, so years of closed goals never push out an open one.
func ListLive() ([]Goal, error) {
	return list(`SELECT `+columns+` FROM goals WHERE deleted_at IS NULL
		ORDER BY (status = 'open') DESC, COALESCE(closed_at, created_at) DESC LIMIT $1`, MaxGoals)
}

// Children is a goal's live sub-goals, at most MaxGoals.
func Children(parentID uuid.UUID) ([]Goal, error) {
	return list(`SELECT `+columns+` FROM goals WHERE parent_id = $1 AND deleted_at IS NULL
		ORDER BY created_at LIMIT $2`, parentID, MaxGoals)
}

// Delete removes a goal; its sub-goals move up to its parent, so none is left
// pointing at a goal nobody can see. False when it was already gone.
func Delete(id uuid.UUID) (bool, error) {
	ok := false
	err := inTree(func([]Node) error { return nil }, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE goals SET parent_id = (SELECT parent_id FROM goals WHERE id = $1), updated_at = NOW()
			 WHERE parent_id = $1 AND deleted_at IS NULL`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE goals SET deleted_at = NOW(), updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		ok = n > 0
		return err
	})
	return ok, err
}

// Reopen opens a closed goal again; false when it wasn't closed.
func Reopen(id uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE goals SET status = 'open', final_progress = NULL, closed_at = NULL, updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL AND status <> 'open'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// LinkProject adds a project to a goal, refusing more than max; added is
// false when it was there already.
func LinkProject(goalID, projectUUID, by uuid.UUID, max int) (added bool, err error) {
	ctx, cancel := withTimeout()
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	// The goal's row is the lock: two links at once can't both pass the count.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM goals WHERE id = $1 FOR UPDATE`, goalID).Scan(&one); err != nil {
		return false, err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_projects WHERE goal_id = $1`, goalID).Scan(&n); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO goal_projects (goal_id, project_uuid, added_by)
		SELECT $1, $2, $3 WHERE $4 > 0 ON CONFLICT DO NOTHING`, goalID, projectUUID, by, max-n)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM goal_projects WHERE goal_id = $1 AND project_uuid = $2)`,
			goalID, projectUUID).Scan(&exists); err != nil {
			return false, err
		}
		if !exists {
			return false, ErrTooManyProjects
		}
		return false, tx.Commit()
	}
	return true, tx.Commit()
}

// UnlinkProject removes a project from a goal; false when it wasn't there.
func UnlinkProject(goalID, projectUUID uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`DELETE FROM goal_projects WHERE goal_id = $1 AND project_uuid = $2`, goalID, projectUUID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Links is the projects of each of the goals, in the order they were added.
func Links(goalIDs []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	out := make(map[uuid.UUID][]uuid.UUID, len(goalIDs))
	if len(goalIDs) == 0 {
		return out, nil
	}
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT goal_id, project_uuid FROM goal_projects WHERE goal_id = ANY($1::uuid[]) ORDER BY added_at, project_uuid`,
		pq.Array(uuidStrings(goalIDs)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g, p uuid.UUID
		if err := rows.Scan(&g, &p); err != nil {
			return nil, err
		}
		out[g] = append(out[g], p)
	}
	return out, rows.Err()
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// AddCheckIn stores a check-in, posted now unless c.CreatedAt says when (an
// import or the demo's history). With value, the goal's current value moves
// to it; with closeAs, the goal closes as that ending, keeping progress as
// its final progress. Both in one transaction with the check-in. nil when
// the goal is gone.
func AddCheckIn(c CheckIn, value *float64, closeAs string) (*CheckIn, error) {
	var at any
	if !c.CreatedAt.IsZero() {
		at = c.CreatedAt
	}
	ctx, cancel := withTimeout()
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Only an open goal takes a check-in: of two closings at once, or a
	// check-in racing a closing, the second finds it closed.
	res, err := tx.ExecContext(ctx, `
		UPDATE goals SET current_value = COALESCE($2, current_value),
		       status = CASE WHEN $3 <> '' THEN $3 ELSE status END,
		       final_progress = CASE WHEN $3 <> '' THEN $4 ELSE final_progress END,
		       closed_at = CASE WHEN $3 <> '' THEN NOW() ELSE closed_at END,
		       updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL AND status = 'open'`, c.GoalId, value, closeAs, c.Progress)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return nil, err
		}
		var closed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM goals WHERE id = $1 AND deleted_at IS NULL)`, c.GoalId).Scan(&closed); err != nil {
			return nil, err
		}
		if closed {
			return nil, ErrClosed
		}
		return nil, nil
	}
	out, err := scanCheckIn(tx.QueryRowContext(ctx, `
		INSERT INTO goal_checkins (goal_id, author_uuid, health, body, value, progress, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7::timestamptz, NOW()), COALESCE($7::timestamptz, NOW()))
		RETURNING `+checkInColumns, c.GoalId, c.AuthorUUID, c.Health, c.Body, value, c.Progress, at))
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// GetCheckIn is one live check-in of a goal, or nil.
func GetCheckIn(id, goalID uuid.UUID) (*CheckIn, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scanCheckIn(postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT `+checkInColumns+` FROM goal_checkins WHERE id = $1 AND goal_id = $2 AND deleted_at IS NULL`, id, goalID))
}

// EditCheckIn changes a check-in's health and note; nil when it is gone.
func EditCheckIn(id, goalID uuid.UUID, health, body string) (*CheckIn, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scanCheckIn(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		UPDATE goal_checkins SET health = $3, body = $4, updated_at = NOW()
		 WHERE id = $1 AND goal_id = $2 AND deleted_at IS NULL
		RETURNING `+checkInColumns, id, goalID, health, body))
}

// DeleteCheckIn removes a check-in; false when it was already gone.
func DeleteCheckIn(id, goalID uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE goal_checkins SET deleted_at = NOW() WHERE id = $1 AND goal_id = $2 AND deleted_at IS NULL`, id, goalID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CheckIns is a goal's live check-ins, newest first, at most limit (capped at MaxCheckIns).
func CheckIns(goalID uuid.UUID, limit int) ([]CheckIn, error) {
	if limit <= 0 || limit > MaxCheckIns {
		limit = MaxCheckIns
	}
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT `+checkInColumns+` FROM goal_checkins WHERE goal_id = $1 AND deleted_at IS NULL
		 ORDER BY created_at DESC LIMIT $2`, goalID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckIn{}
	for rows.Next() {
		c, err := scanCheckIn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// LatestCheckIns is each goal's newest live check-in, in one query. A goal
// with none has no entry.
func LatestCheckIns(goalIDs []uuid.UUID) (map[uuid.UUID]CheckIn, error) {
	out := make(map[uuid.UUID]CheckIn, len(goalIDs))
	if len(goalIDs) == 0 {
		return out, nil
	}
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT DISTINCT ON (goal_id) `+checkInColumns+` FROM goal_checkins
		 WHERE goal_id = ANY($1::uuid[]) AND deleted_at IS NULL
		 ORDER BY goal_id, created_at DESC`, pq.Array(uuidStrings(goalIDs)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanCheckIn(rows)
		if err != nil {
			return nil, err
		}
		out[c.GoalId] = *c
	}
	return out, rows.Err()
}
