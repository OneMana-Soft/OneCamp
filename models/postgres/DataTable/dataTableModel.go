// Package models (DataTable) is the Postgres data-access layer for the Tables
// feature (migration 91): a first-class, Notion-style structured-data entity
// with fields (columns), rows, and saved views. The business layer adds
// validation + permission checks; this package only persists and reads.
package models

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Visibility constants (aligned with the migration CHECK).
const (
	VisibilityPrivate   = "private"
	VisibilityWorkspace = "workspace"
)

// Field type constants (aligned with the migration CHECK).
const (
	FieldText        = "text"
	FieldNumber      = "number"
	FieldSelect      = "select"
	FieldMultiSelect = "multi_select"
	FieldDate        = "date"
	FieldCheckbox    = "checkbox"
	FieldPerson      = "person"
	FieldURL         = "url"
	FieldEmail       = "email"
	// FieldRelation links rows to OneCamp entities (tasks, docs, boards, users,
	// projects). The field config carries {"relation_target": "<type>"} and the
	// cell value is an array of {id,label,type} refs (req 4.2).
	FieldRelation = "relation"
	// FieldFormula is worked out from the row's other fields on each read
	// (business/DataTable/formula). Its config carries {"formula": "..."}, with
	// fields named by id; its cells are never stored.
	FieldFormula = "formula"
	// FieldRollup is worked out on each read from the rows a relation field
	// links to (business/DataTable/relations.go). Its config carries
	// {"relation": "<relation field id>", "field": "<field id in the linked
	// table>", "aggregate": "<how>"}; its cells are never stored.
	FieldRollup = "rollup"
)

// View type constants.
const (
	ViewGrid     = "grid"
	ViewBoard    = "board"
	ViewCalendar = "calendar"
)

// ValidVisibility / ValidFieldType / ValidViewType validate inputs.
func ValidVisibility(v string) bool { return v == VisibilityPrivate || v == VisibilityWorkspace }

func ValidFieldType(t string) bool {
	switch t {
	case FieldText, FieldNumber, FieldSelect, FieldMultiSelect, FieldDate,
		FieldCheckbox, FieldPerson, FieldURL, FieldEmail, FieldRelation, FieldFormula, FieldRollup:
		return true
	default:
		return false
	}
}

func ValidViewType(t string) bool {
	switch t {
	case ViewGrid, ViewBoard, ViewCalendar:
		return true
	default:
		return false
	}
}

// DataTable mirrors a row of data_tables.
type DataTable struct {
	Id          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	Description *string    `json:"description,omitempty"`
	Icon        *string    `json:"icon,omitempty"`
	Visibility  string     `json:"visibility"`
	CreatedBy   uuid.UUID  `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

// Field mirrors a row of data_table_fields.
type Field struct {
	Id        uuid.UUID  `json:"id"`
	TableId   uuid.UUID  `json:"table_id"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Config    string     `json:"config"` // raw JSON object
	Position  float64    `json:"position"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// Row mirrors a row of data_table_rows.
type Row struct {
	Id        uuid.UUID  `json:"id"`
	TableId   uuid.UUID  `json:"table_id"`
	Values    string     `json:"values"` // raw JSON object keyed by field id
	Position  float64    `json:"position"`
	CreatedBy *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// View mirrors a row of data_table_views.
type View struct {
	Id        uuid.UUID  `json:"id"`
	TableId   uuid.UUID  `json:"table_id"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Config    string     `json:"config"` // raw JSON object
	Position  float64    `json:"position"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

type scanner interface {
	Scan(dest ...any) error
}

// ───────────────────────── data_tables ─────────────────────────

const tableColumns = `id, name, description, icon, visibility, created_by, created_at, updated_at, deleted_at`

func scanTable(s scanner) (*DataTable, error) {
	var t DataTable
	var description, icon sql.NullString
	var deletedAt sql.NullTime
	if err := s.Scan(&t.Id, &t.Name, &description, &icon, &t.Visibility, &t.CreatedBy,
		&t.CreatedAt, &t.UpdatedAt, &deletedAt); err != nil {
		return nil, err
	}
	if description.Valid {
		t.Description = &description.String
	}
	if icon.Valid {
		t.Icon = &icon.String
	}
	if deletedAt.Valid {
		t.DeletedAt = &deletedAt.Time
	}
	return &t, nil
}

// CreateTable inserts a new table and returns the generated id.
func CreateTable(ctx context.Context, t *DataTable) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	id := uuid.New()
	const q = `INSERT INTO data_tables (id, name, description, icon, visibility, created_by)
		VALUES ($1,$2,$3,$4,$5,$6)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, t.Name, t.Description, t.Icon, t.Visibility, t.CreatedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateTable err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// UpdateTable updates the editable header fields of a table.
func UpdateTable(ctx context.Context, t *DataTable) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE data_tables SET name=$2, description=$3, icon=$4, visibility=$5, updated_at=NOW()
		WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, t.Id, t.Name, t.Description, t.Icon, t.Visibility)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateTable err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SoftDeleteTable marks a table deleted (its children cascade logically via the
// table_id scoping of every read).
func SoftDeleteTable(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE data_tables SET deleted_at=NOW(), updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SoftDeleteTable err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetTableByID returns a single non-deleted table, or (nil, nil) if absent.
func GetTableByID(ctx context.Context, id uuid.UUID) (*DataTable, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + tableColumns + ` FROM data_tables WHERE id=$1 AND deleted_at IS NULL`
	t, err := scanTable(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetTableByID err: %+v", err)
		return nil, err
	}
	return t, nil
}

// ListTablesVisibleTo returns non-deleted tables the user may see: all
// workspace-visible tables plus their own private ones. Admins see all.
func ListTablesVisibleTo(ctx context.Context, userID uuid.UUID, isAdmin bool) ([]*DataTable, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var q string
	var args []interface{}
	if isAdmin {
		q = `SELECT ` + tableColumns + ` FROM data_tables WHERE deleted_at IS NULL ORDER BY updated_at DESC LIMIT 1000`
	} else {
		q = `SELECT ` + tableColumns + ` FROM data_tables
			WHERE deleted_at IS NULL AND (visibility='workspace' OR created_by=$1)
			ORDER BY updated_at DESC LIMIT 1000`
		args = append(args, userID)
	}
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListTablesVisibleTo err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*DataTable
	for rows.Next() {
		t, scanErr := scanTable(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ───────────────────────── fields ─────────────────────────

func scanField(s scanner) (*Field, error) {
	var f Field
	var deletedAt sql.NullTime
	if err := s.Scan(&f.Id, &f.TableId, &f.Name, &f.Type, &f.Config, &f.Position,
		&f.CreatedAt, &f.UpdatedAt, &deletedAt); err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		f.DeletedAt = &deletedAt.Time
	}
	if strings.TrimSpace(f.Config) == "" {
		f.Config = "{}"
	}
	return &f, nil
}

const fieldColumns = `id, table_id, name, type, config, position, created_at, updated_at, deleted_at`

// CreateField inserts a column.
func CreateField(ctx context.Context, f *Field) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(f.Config) == "" {
		f.Config = "{}"
	}
	id := uuid.New()
	const q = `INSERT INTO data_table_fields (id, table_id, name, type, config, position)
		VALUES ($1,$2,$3,$4,$5,$6)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, f.TableId, f.Name, f.Type, f.Config, f.Position)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateField err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// UpdateField updates a column's editable attributes.
func UpdateField(ctx context.Context, f *Field) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(f.Config) == "" {
		f.Config = "{}"
	}
	const q = `UPDATE data_table_fields SET name=$2, type=$3, config=$4, position=$5, updated_at=NOW()
		WHERE id=$1 AND table_id=$6 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, f.Id, f.Name, f.Type, f.Config, f.Position, f.TableId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateField err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteField soft-deletes a column.
func DeleteField(ctx context.Context, tableId, fieldId uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE data_table_fields SET deleted_at=NOW(), updated_at=NOW()
		WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, fieldId, tableId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteField err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListFields returns a table's columns in position order.
func ListFields(ctx context.Context, tableId uuid.UUID) ([]*Field, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + fieldColumns + ` FROM data_table_fields
		WHERE table_id=$1 AND deleted_at IS NULL ORDER BY position ASC, created_at ASC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, tableId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListFields err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Field
	for rows.Next() {
		f, scanErr := scanField(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ───────────────────────── rows ─────────────────────────

func scanRow(s scanner) (*Row, error) {
	var r Row
	var createdBy uuid.NullUUID
	var deletedAt sql.NullTime
	if err := s.Scan(&r.Id, &r.TableId, &r.Values, &r.Position, &createdBy,
		&r.CreatedAt, &r.UpdatedAt, &deletedAt); err != nil {
		return nil, err
	}
	if createdBy.Valid {
		r.CreatedBy = &createdBy.UUID
	}
	if deletedAt.Valid {
		r.DeletedAt = &deletedAt.Time
	}
	if strings.TrimSpace(r.Values) == "" {
		r.Values = "{}"
	}
	return &r, nil
}

const rowColumns = `id, table_id, values, position, created_by, created_at, updated_at, deleted_at`

// CreateRow inserts a row and returns the persisted row (via RETURNING) so the
// caller gets DB-populated timestamps without a second read round trip.
func CreateRow(ctx context.Context, r *Row) (*Row, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(r.Values) == "" {
		r.Values = "{}"
	}
	id := uuid.New()
	const q = `INSERT INTO data_table_rows (id, table_id, values, position, created_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING ` + rowColumns
	out, err := scanRow(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id, r.TableId, r.Values, r.Position, r.CreatedBy))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateRow err: %+v", err)
		return nil, err
	}
	return out, nil
}

// UpdateRowValues replaces a row's values blob and position, returning the
// updated row (via RETURNING) so the caller avoids a follow-up read. Returns
// sql.ErrNoRows when the row does not exist (or is deleted).
func UpdateRowValues(ctx context.Context, tableId, rowId uuid.UUID, valuesJSON string, position float64) (*Row, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(valuesJSON) == "" {
		valuesJSON = "{}"
	}
	const q = `UPDATE data_table_rows SET values=$3, position=$4, updated_at=NOW()
		WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL RETURNING ` + rowColumns
	out, err := scanRow(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, rowId, tableId, valuesJSON, position))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateRowValues err: %+v", err)
		return nil, err
	}
	return out, nil
}

// DeleteRow soft-deletes a row, and its links to and from other rows
// (relationModel.go), in one transaction. It says whether it had links.
func DeleteRow(ctx context.Context, tableId, rowId uuid.UUID) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	const q = `UPDATE data_table_rows SET deleted_at=NOW(), updated_at=NOW()
		WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL`
	res, err := tx.ExecContext(dbctx, q, rowId, tableId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteRow err: %+v", err)
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, sql.ErrNoRows
	}
	res, err = tx.ExecContext(dbctx, `DELETE FROM data_table_links WHERE from_row = $1 OR to_row = $1`, rowId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteRow links err: %+v", err)
		return false, err
	}
	linked, _ := res.RowsAffected()
	return linked > 0, tx.Commit()
}

// GetRowByID returns a single non-deleted row, or (nil, nil) if absent.
func GetRowByID(ctx context.Context, tableId, rowId uuid.UUID) (*Row, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + rowColumns + ` FROM data_table_rows WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL`
	r, err := scanRow(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, rowId, tableId))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetRowByID err: %+v", err)
		return nil, err
	}
	return r, nil
}

// ListRows returns a table's rows in position order, bounded by limit/offset.
func ListRows(ctx context.Context, tableId uuid.UUID, limit, offset int) ([]*Row, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	q := `SELECT ` + rowColumns + ` FROM data_table_rows
		WHERE table_id=$1 AND deleted_at IS NULL ORDER BY position ASC, created_at ASC
		LIMIT $2 OFFSET $3`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, tableId, limit, offset)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListRows err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Row
	for rows.Next() {
		r, scanErr := scanRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// escapeLikePattern escapes the LIKE/ILIKE wildcard metacharacters in a raw
// substring so it is matched literally (backslash is the escape char, so it
// must be doubled first). The result is meant to be wrapped in %…% by the
// caller and used with `ILIKE $n ESCAPE '\'`.
func escapeLikePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ListRowsFiltered returns a page of a table's non-deleted rows whose raw JSONB
// text contains EVERY one of likeSubstrings (case-insensitive), in the same
// position order as ListRows. It is a deliberately COARSE, superset filter: the
// business/aggregation layer still re-applies its exact, typed predicates in
// memory, so this only ever narrows the candidate set to a superset of true
// matches — never changing results, only reducing how many rows must be loaded
// so a filtered aggregation over a large table completes within the scan cap
// instead of truncating. Substrings are matched literally (wildcards escaped)
// and passed as bound parameters, so there is no injection surface. An empty
// likeSubstrings behaves like ListRows.
func ListRowsFiltered(ctx context.Context, tableId uuid.UUID, likeSubstrings []string, limit, offset int) ([]*Row, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	var sb strings.Builder
	sb.WriteString(`SELECT ` + rowColumns + ` FROM data_table_rows WHERE table_id=$1 AND deleted_at IS NULL`)
	args := []interface{}{tableId}
	n := 2
	for _, sub := range likeSubstrings {
		if strings.TrimSpace(sub) == "" {
			continue
		}
		sb.WriteString(fmt.Sprintf(` AND "values"::text ILIKE $%d ESCAPE '\'`, n))
		args = append(args, "%"+escapeLikePattern(sub)+"%")
		n++
	}
	sb.WriteString(fmt.Sprintf(` ORDER BY position ASC, created_at ASC LIMIT $%d OFFSET $%d`, n, n+1))
	args = append(args, limit, offset)

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, sb.String(), args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListRowsFiltered err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Row
	for rows.Next() {
		r, scanErr := scanRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ───────────────────────── views ─────────────────────────

func scanView(s scanner) (*View, error) {
	var v View
	var deletedAt sql.NullTime
	if err := s.Scan(&v.Id, &v.TableId, &v.Name, &v.Type, &v.Config, &v.Position,
		&v.CreatedAt, &v.UpdatedAt, &deletedAt); err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		v.DeletedAt = &deletedAt.Time
	}
	if strings.TrimSpace(v.Config) == "" {
		v.Config = "{}"
	}
	return &v, nil
}

const viewColumns = `id, table_id, name, type, config, position, created_at, updated_at, deleted_at`

// CreateView inserts a saved view.
func CreateView(ctx context.Context, v *View) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(v.Config) == "" {
		v.Config = "{}"
	}
	id := uuid.New()
	const q = `INSERT INTO data_table_views (id, table_id, name, type, config, position)
		VALUES ($1,$2,$3,$4,$5,$6)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, v.TableId, v.Name, v.Type, v.Config, v.Position)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateView err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// UpdateView updates a saved view's editable attributes.
func UpdateView(ctx context.Context, v *View) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(v.Config) == "" {
		v.Config = "{}"
	}
	const q = `UPDATE data_table_views SET name=$2, type=$3, config=$4, position=$5, updated_at=NOW()
		WHERE id=$1 AND table_id=$6 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, v.Id, v.Name, v.Type, v.Config, v.Position, v.TableId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateView err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteView soft-deletes a saved view.
func DeleteView(ctx context.Context, tableId, viewId uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE data_table_views SET deleted_at=NOW(), updated_at=NOW()
		WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, viewId, tableId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteView err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListViews returns a table's saved views in position order.
func ListViews(ctx context.Context, tableId uuid.UUID) ([]*View, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + viewColumns + ` FROM data_table_views
		WHERE table_id=$1 AND deleted_at IS NULL ORDER BY position ASC, created_at ASC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, tableId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListViews err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*View
	for rows.Next() {
		v, scanErr := scanView(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CountRows returns the number of non-deleted rows in a table.
func CountRows(ctx context.Context, tableId uuid.UUID) (int, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT COUNT(*) FROM data_table_rows WHERE table_id=$1 AND deleted_at IS NULL`
	var n int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, tableId).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// GetViewByID returns a single non-deleted view, or (nil, nil) if absent.
func GetViewByID(ctx context.Context, tableId, viewId uuid.UUID) (*View, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + viewColumns + ` FROM data_table_views WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL`
	v, err := scanView(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, viewId, tableId))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetViewByID err: %+v", err)
		return nil, err
	}
	return v, nil
}
