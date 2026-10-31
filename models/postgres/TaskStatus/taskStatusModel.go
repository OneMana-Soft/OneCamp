// Package models (TaskStatus) stores a project's custom task statuses
// (migration 167). Every query is scoped to a project.
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// TaskStatus is one custom status.
type TaskStatus struct {
	ID        uuid.UUID `json:"id"`
	ProjectID uuid.UUID `json:"project_id"`
	Name      string    `json:"name"`
	Category  string    `json:"category"`
	Color     string    `json:"color"`
	Position  int       `json:"position"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var (
	ErrNotFound  = errors.New("task status not found")
	ErrNameTaken = errors.New("a status with that name already exists in this project")
)

const columns = `id, project_id, name, category, color, position, created_at, updated_at`

func scan(row interface{ Scan(...any) error }) (*TaskStatus, error) {
	var s TaskStatus
	if err := row.Scan(&s.ID, &s.ProjectID, &s.Name, &s.Category, &s.Color, &s.Position, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

// isUniqueViolation recognises Postgres error 23505 from either driver: pgx,
// which the server uses, reports it through SQLState(); lib/pq through Code.
func isUniqueViolation(err error) bool {
	var withState interface{ SQLState() string }
	if errors.As(err, &withState) {
		return withState.SQLState() == "23505"
	}
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// List returns a project's custom statuses in their saved order.
func List(ctx context.Context, projectID uuid.UUID) ([]*TaskStatus, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT `+columns+` FROM task_statuses WHERE project_id = $1 AND `+softDelete.Live("")+` ORDER BY position, created_at`, projectID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskStatus List err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	out := []*TaskStatus{}
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListForProjects returns the custom statuses of several projects at once,
// by project: a report reads every project a person is in.
func ListForProjects(ctx context.Context, projectIDs []uuid.UUID) (map[uuid.UUID][]*TaskStatus, error) {
	out := map[uuid.UUID][]*TaskStatus{}
	if len(projectIDs) == 0 {
		return out, nil
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT `+columns+` FROM task_statuses WHERE project_id = ANY($1::uuid[]) AND `+softDelete.Live("")+` ORDER BY position, created_at`, pq.Array(projectIDs))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskStatus ListForProjects err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out[s.ProjectID] = append(out[s.ProjectID], s)
	}
	return out, rows.Err()
}

// Get returns one of a project's custom statuses.
func Get(ctx context.Context, projectID, id uuid.UUID) (*TaskStatus, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	s, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT `+columns+` FROM task_statuses WHERE id = $1 AND project_id = $2 AND `+softDelete.Live(""), id, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// FindByName finds a project's custom status by name, ignoring case.
func FindByName(ctx context.Context, projectID uuid.UUID, name string) (*TaskStatus, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	s, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT `+columns+` FROM task_statuses WHERE project_id = $1 AND lower(name) = lower($2) AND `+softDelete.Live(""), projectID, strings.TrimSpace(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// IDsByName returns the ids of every project's custom status called name,
// ignoring case, for a filter that spans projects ("my tasks in QA").
func IDsByName(ctx context.Context, name string) ([]string, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c,
		`SELECT id FROM task_statuses WHERE lower(name) = lower($1) AND `+softDelete.Live(""), strings.TrimSpace(name))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskStatus IDsByName err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id.String())
	}
	return out, rows.Err()
}

// Count returns how many custom statuses a project has.
func Count(ctx context.Context, projectID uuid.UUID) (int, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `SELECT COUNT(*) FROM task_statuses WHERE project_id = $1 AND `+softDelete.Live(""), projectID).Scan(&n)
	return n, err
}

// Create adds a status at the end of the project's order.
func Create(ctx context.Context, s *TaskStatus, createdBy uuid.UUID) (*TaskStatus, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	out, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		INSERT INTO task_statuses (id, project_id, name, category, color, position, created_by)
		VALUES ($1, $2, $3, $4, $5, COALESCE((SELECT MAX(position) + 1 FROM task_statuses WHERE project_id = $2), 0), $6)
		RETURNING `+columns, uuid.New(), s.ProjectID, s.Name, s.Category, s.Color, createdBy))
	if isUniqueViolation(err) {
		return nil, ErrNameTaken
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskStatus Create err: %+v", err)
	}
	return out, err
}

// Update changes a status's name, category and colour.
func Update(ctx context.Context, s *TaskStatus) (*TaskStatus, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	out, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		UPDATE task_statuses SET name = $3, category = $4, color = $5, updated_at = NOW()
		WHERE id = $1 AND project_id = $2 AND `+softDelete.Live("")+` RETURNING `+columns, s.ID, s.ProjectID, s.Name, s.Category, s.Color))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if isUniqueViolation(err) {
		return nil, ErrNameTaken
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskStatus Update err: %+v", err)
	}
	return out, err
}

// Reorder sets positions from the order of ids. Ids not of this project are
// ignored; statuses left out keep their place after the ones given.
func Reorder(ctx context.Context, projectID uuid.UUID, ids []uuid.UUID) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		UPDATE task_statuses t SET position = o.ord, updated_at = NOW()
		FROM unnest($2::uuid[]) WITH ORDINALITY AS o(id, ord)
		WHERE t.id = o.id AND t.project_id = $1`, projectID, pq.Array(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskStatus Reorder err: %+v", err)
	}
	return err
}

// Delete removes one of a project's statuses, keeping what its delete moved
// for its restore. A status deleted again before its last restore finished
// keeps that restore's remainder beside its own (Moves.Earlier), so the next
// restore brings back all of both.
func Delete(ctx context.Context, projectID, id, by uuid.UUID, moved Moves) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		pending, found, err := pgexec.QueryOne(ctx, `SELECT deleted_moves FROM task_statuses WHERE id = $1 AND project_id = $2 AND `+softDelete.Live("")+` FOR UPDATE`,
			func(r pgexec.Row) (b []byte, err error) { err = r.Scan(&b); return b, err }, id, projectID)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if pending != nil {
			var was Moves
			if err := json.Unmarshal(pending, &was); err != nil {
				return err
			}
			moved.Earlier = was.All()
		}
		raw, err := json.Marshal(moved)
		if err != nil {
			return err
		}
		if _, err := pgexec.Exec(ctx, `UPDATE task_statuses SET deleted_moves = $3 WHERE id = $1 AND project_id = $2 AND `+softDelete.Live(""), id, projectID, raw); err != nil {
			return err
		}
		deleted, err := softDelete.Delete(ctx, softDelete.Status, id, by, "project_id = $3", projectID)
		if err == nil && !deleted {
			return ErrNotFound
		}
		return err
	})
}

// Moves is where a status's delete moved its tasks, which it moved, and what
// else that pointed at it the delete pointed there (Repointed, each as its
// listener names it: a GitHub automation rule). Earlier holds, oldest first,
// the moves of the deletes before it whose restore hadn't finished.
type Moves struct {
	Category   string   `json:"category"`
	CustomID   string   `json:"custom_id,omitempty"`
	CustomName string   `json:"custom_name,omitempty"`
	Tasks      []string `json:"tasks"`
	Repointed  []string `json:"repointed,omitempty"`
	Earlier    []Moves  `json:"earlier,omitempty"`
}

// All is each delete's moves that m holds, oldest first.
func (m Moves) All() []Moves {
	all := append([]Moves(nil), m.Earlier...)
	m.Earlier = nil
	return append(all, m)
}

// Repointed records on a deleted status what its delete pointed elsewhere
// (refs), for its restore to point back.
func Repointed(ctx context.Context, id uuid.UUID, refs []string) error {
	raw, err := json.Marshal(refs)
	if err != nil {
		return err
	}
	_, err = pgexec.Exec(ctx, `UPDATE task_statuses SET deleted_moves = jsonb_set(COALESCE(deleted_moves, '{}'::jsonb), '{repointed}', $2::jsonb) WHERE id = $1 AND deleted_at IS NOT NULL`, id, string(raw))
	return err
}

// Restore brings a deleted status back and answers it with the moves its
// deletes made, for its tasks' return. Their record stays until Restored
// clears it once all are back, so a restore that failed partway is finished
// by the next: a live status that still holds its moves answers them again.
// softDelete.ErrTaken: a live status of the project has its name now.
func Restore(ctx context.Context, id uuid.UUID) (*TaskStatus, Moves, error) {
	var moved Moves
	var s *TaskStatus
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		var deleted bool
		raw, found, err := pgexec.QueryOne(ctx, `SELECT COALESCE(deleted_moves, '{}'::jsonb), deleted_at IS NOT NULL FROM task_statuses
			WHERE id = $1 AND `+softDelete.Restorable(softDelete.Status)+` FOR UPDATE`,
			func(r pgexec.Row) (b []byte, err error) { err = r.Scan(&b, &deleted); return b, err }, id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &moved); err != nil {
			return err
		}
		if deleted {
			if _, err := softDelete.Restore(ctx, softDelete.Status, id); err != nil {
				return err
			}
		}
		s, _, err = pgexec.QueryOne(ctx, `SELECT `+columns+` FROM task_statuses WHERE id = $1 AND `+softDelete.Live(""), func(r pgexec.Row) (*TaskStatus, error) { return scan(r) }, id)
		return err
	})
	return s, moved, err
}

// Restored clears a restored status's record of what its delete moved, once
// its tasks and what pointed at it are back.
func Restored(ctx context.Context, id uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE task_statuses SET deleted_moves = NULL WHERE id = $1 AND `+softDelete.Live(""), id)
	return err
}

// Order is a project's saved column order (migration 326): built-in keys and
// custom status ids, as its admins dragged them; nil when it never set one.
func Order(ctx context.Context, projectID uuid.UUID) ([]string, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var order []string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT statuses FROM project_status_order WHERE project_id = $1::uuid`, projectID).Scan(pq.Array(&order))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskStatus Order err: %+v", err)
		return nil, err
	}
	return order, nil
}

// SetOrder saves a project's column order, by whom.
func SetOrder(ctx context.Context, projectID uuid.UUID, order []string, by uuid.UUID) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var setBy any
	if by != uuid.Nil {
		setBy = by
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		INSERT INTO project_status_order (project_id, statuses, set_by, set_at)
		VALUES ($1::uuid, $2::text[], $3::uuid, NOW())
		ON CONFLICT (project_id) DO UPDATE SET statuses = EXCLUDED.statuses, set_by = EXCLUDED.set_by, set_at = NOW()`,
		projectID, pq.Array(order), setBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TaskStatus SetOrder err: %+v", err)
	}
	return err
}
