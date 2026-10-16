// Package models (DataTable) is the Postgres data-access layer for the Tables
// feature (migration 91): a first-class, Notion-style structured-data entity
// with fields (columns), rows, and saved views. The business layer adds
// validation + permission checks; this package only persists and reads.
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
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
	CreatedBy   uuid.UUID  `json:"created_by,omitzero"` // zero for a guest (GetGuestBundle)
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
	// AIKept is the AI columns whose cell in this row a person wrote: a fill
	// leaves them as they are until someone asks for it again.
	AIKept []string `json:"ai_kept,omitempty"`
}

// AIKeptKey is where a row's values keep its AIKept: no field id looks like
// it, every read lifts it out of Values, and no write takes it from a caller.
const AIKeptKey = "_ai_kept"

// liftAIKept moves a row's AIKept out of its values.
func liftAIKept(r *Row) {
	if !strings.Contains(r.Values, `"`+AIKeptKey+`"`) {
		return
	}
	var cells map[string]json.RawMessage
	if json.Unmarshal([]byte(r.Values), &cells) != nil {
		return
	}
	_ = json.Unmarshal(cells[AIKeptKey], &r.AIKept)
	delete(cells, AIKeptKey)
	if b, err := json.Marshal(cells); err == nil {
		r.Values = string(b)
	}
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

// TableChange is a change to a table's header: a name or visibility left nil
// stays as it is, as does a description or icon not set (SetDescription,
// SetIcon); one set to nil is cleared.
type TableChange struct {
	Name, Visibility        *string
	SetDescription, SetIcon bool
	Description, Icon       *string
}

// UpdateTable changes the header fields a change names, and only those, in
// one statement: a field written from a copy read earlier can't undo another
// changed since. It returns the table, and its visibility as it was.
func UpdateTable(ctx context.Context, id uuid.UUID, c TableChange) (*DataTable, string, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `WITH was AS (SELECT id, visibility FROM data_tables WHERE id = $1 AND deleted_at IS NULL FOR UPDATE)
		UPDATE data_tables t SET name = COALESCE($2, t.name),
			description = CASE WHEN $3 THEN $4 ELSE t.description END,
			icon = CASE WHEN $5 THEN $6 ELSE t.icon END,
			visibility = COALESCE($7, t.visibility), updated_at = NOW()
		FROM was WHERE t.id = was.id
		RETURNING t.id, t.name, t.description, t.icon, t.visibility, t.created_by, t.created_at, t.updated_at, t.deleted_at, was.visibility`
	var before string
	out, err := scanTable(thenScan{postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id, c.Name, c.SetDescription, c.Description, c.SetIcon, c.Icon, c.Visibility), []any{&before}})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", sql.ErrNoRows
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateTable err: %+v", err)
		return nil, "", err
	}
	return out, before, nil
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

// execer is the database, or a transaction on it.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// UpdateField updates a column's editable attributes.
func UpdateField(ctx context.Context, f *Field) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	return updateField(dbctx, postgresInit.DBConn.SqlDB, f)
}

func updateField(ctx context.Context, db execer, f *Field) error {
	if strings.TrimSpace(f.Config) == "" {
		f.Config = "{}"
	}
	const q = `UPDATE data_table_fields SET name=$2, type=$3, config=$4, position=$5, updated_at=NOW()
		WHERE id=$1 AND table_id=$6 AND deleted_at IS NULL`
	res, err := db.ExecContext(ctx, q, f.Id, f.Name, f.Type, f.Config, f.Position, f.TableId)
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

// CellRewrite is a change to the cells of the field being updated, made in
// the field update's transaction (UpdateFieldAndCells): a statement over the
// table's live rows, with $1 the table, $2 the field id and its args after,
// that returns the id of each row it changes.
type CellRewrite struct {
	query string
	args  []any
}

// relabelSelectCells and relabelMultiCells rewrite a field's cells by a map
// of labels ($3, {"old": "new"}, "" to take one out), a row at a time in one
// statement, so the map applies to every label at once (two options swapping
// names swap in their cells too). A select cell is a label; a multi-select
// cell is a list of them, which keeps its order and each label once.
const relabelSelectCells = `UPDATE data_table_rows
	SET values = jsonb_set(values, ARRAY[$2::text], to_jsonb($3::jsonb ->> (values ->> $2::text))), updated_at = NOW()
	WHERE table_id = $1 AND deleted_at IS NULL
	  AND jsonb_typeof(values -> $2::text) = 'string' AND $3::jsonb ? (values ->> $2::text)
	RETURNING id`

const relabelMultiCells = `UPDATE data_table_rows r
	SET values = jsonb_set(r.values, ARRAY[$2::text], COALESCE((
		SELECT jsonb_agg(x.v ORDER BY x.n) FROM (
			SELECT DISTINCT ON (m.v) m.v, m.n FROM (
				SELECT CASE WHEN jsonb_typeof(e.j) = 'string' AND $3::jsonb ? (e.j #>> '{}')
					THEN to_jsonb($3::jsonb ->> (e.j #>> '{}')) ELSE e.j END AS v, e.n
				FROM jsonb_array_elements(r.values -> $2::text) WITH ORDINALITY AS e(j, n)
			) m WHERE m.v <> '""'::jsonb ORDER BY m.v, m.n
		) x), '[]'::jsonb)), updated_at = NOW()
	WHERE r.table_id = $1 AND r.deleted_at IS NULL AND jsonb_typeof(r.values -> $2::text) = 'array'
	  AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(r.values -> $2::text) t WHERE $3::jsonb ? t)
	RETURNING r.id`

// Relabel is the rewrite of a select or multi-select field's cells by
// relabel: each option's old label to its new one, or to "" for one taken
// out, so no cell is left holding a label the field no longer offers.
func Relabel(relabel map[string]string) []CellRewrite {
	if len(relabel) == 0 {
		return nil
	}
	labels, _ := json.Marshal(relabel)
	return []CellRewrite{{relabelSelectCells, []any{string(labels)}}, {relabelMultiCells, []any{string(labels)}}}
}

// Convert is a field's cells converted as its type changes, in the field
// update's transaction: given each live row's cell (by row id) as it is, it
// gives those that change, and may change the field (its options) before it
// is written.
type Convert func(cells map[uuid.UUID]json.RawMessage) (map[uuid.UUID]json.RawMessage, error)

// fieldCellsSQL reads a field's ($2) cells in a table's ($1) live rows.
const fieldCellsSQL = `SELECT id, values -> $2::text FROM data_table_rows
	WHERE table_id = $1 AND deleted_at IS NULL AND values ? $2::text`

// FieldCells is a field's cells in a table's live rows, by row.
func FieldCells(ctx context.Context, tableId, fieldId uuid.UUID) (map[uuid.UUID]json.RawMessage, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	return fieldCells(dbctx, postgresInit.DBConn.SqlDB, fieldCellsSQL, tableId, fieldId)
}

// querier is the database, or a transaction on it.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func fieldCells(ctx context.Context, db querier, q string, tableId, fieldId uuid.UUID) (map[uuid.UUID]json.RawMessage, error) {
	rows, err := db.QueryContext(ctx, q, tableId, fieldId.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/fieldCells err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]json.RawMessage{}
	for rows.Next() {
		var id uuid.UUID
		var cell []byte
		if err := rows.Scan(&id, &cell); err != nil {
			return nil, err
		}
		out[id] = cell
	}
	return out, rows.Err()
}

// setCellsSQL writes a field's ($2) cells in a table ($1): each row ($3) its
// cell ($4, JSON).
const setCellsSQL = `UPDATE data_table_rows r SET values = jsonb_set(r.values, ARRAY[$2::text], u.cell::jsonb), updated_at = NOW()
	FROM unnest($3::uuid[], $4::text[]) AS u(id, cell)
	WHERE r.id = u.id AND r.table_id = $1 AND r.deleted_at IS NULL
	RETURNING r.id`

// FieldPlan works out a field update from the field as the update finds it,
// locked: the field to write, how its cells convert as its type changes (nil
// for none), and the rewrites to its cells after that.
type FieldPlan func(current *Field) (*Field, Convert, []CellRewrite, error)

// UpdateFieldAndCells updates a field and its cells in one transaction, so no
// reader finds the field changed and its cells not. The update is planned
// from the field as the transaction finds it, locked: two saves planned from
// one copy (an option renamed twice in quick succession) would each rewrite
// the cells as the field was before the other. It has BulkTimeout, as the
// cells of every row may change. It returns how many rows' cells changed; a
// field that's gone is sql.ErrNoRows.
func UpdateFieldAndCells(ctx context.Context, tableID, fieldID uuid.UUID, plan FieldPlan) (int, error) {
	changed := map[uuid.UUID]bool{}
	err := postgresInit.InTx(ctx, postgresInit.BulkTimeout, func(ctx context.Context, tx *sql.Tx) error {
		current, err := scanField(tx.QueryRowContext(ctx, `SELECT `+fieldColumns+` FROM data_table_fields
			WHERE id = $1 AND table_id = $2 AND deleted_at IS NULL FOR UPDATE`, fieldID, tableID))
		if err != nil {
			return err
		}
		f, convert, rewrites, err := plan(current)
		if err != nil {
			return err
		}
		var converted map[uuid.UUID]json.RawMessage
		if convert != nil {
			cells, err := fieldCells(ctx, tx, fieldCellsSQL+` FOR UPDATE`, f.TableId, f.Id)
			if err != nil {
				return err
			}
			if converted, err = convert(cells); err != nil {
				return err
			}
		}
		if err := updateField(ctx, tx, f); err != nil {
			return err
		}
		if len(converted) > 0 {
			ids, cells := make([]uuid.UUID, 0, len(converted)), make([]string, 0, len(converted))
			for id, cell := range converted {
				ids, cells = append(ids, id), append(cells, string(cell))
			}
			// A list a row long goes to pgx as it is, as in CellsOf: through
			// pq.Array, pgx writes out the whole list in an error before taking it.
			if err := collectIDs(ctx, tx, changed, setCellsSQL, f.TableId, f.Id.String(), ids, cells); err != nil {
				helpers.LogErrorWithContext(ctx, "models/UpdateFieldAndCells convert err: %+v", err)
				return err
			}
		}
		for _, rw := range rewrites {
			if err := collectIDs(ctx, tx, changed, rw.query, append([]any{f.TableId, f.Id.String()}, rw.args...)...); err != nil {
				helpers.LogErrorWithContext(ctx, "models/UpdateFieldAndCells cells err: %+v", err)
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(changed), nil
}

// collectIDs runs a statement returning row ids, and adds them to into.
func collectIDs(ctx context.Context, tx *sql.Tx, into map[uuid.UUID]bool, query string, args ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		into[id] = true
	}
	return rows.Err()
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

// thenScan is a scanner of a row's columns (rowColumns) followed by more, which
// it reads into more.
type thenScan struct {
	s    scanner
	more []any
}

func (t thenScan) Scan(dest ...any) error { return t.s.Scan(append(dest, t.more...)...) }

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
	liftAIKept(&r)
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

// UpdateRowValues replaces a row's values blob, and its position unless
// position is nil (writeCells); it returns the updated row, and its values as
// they were. Returns sql.ErrNoRows when the row does not exist (or is
// deleted), and ErrRowTooLarge past maxBytes. The AI columns' cells (aiFields)
// are as wholeRowCellsSQL says.
func UpdateRowValues(ctx context.Context, tableId, rowId uuid.UUID, valuesJSON string, position *float64, aiFields []string, maxBytes int) (*Row, string, error) {
	if strings.TrimSpace(valuesJSON) == "" {
		valuesJSON = "{}"
	}
	return writeCells(ctx, wholeRowCellsSQL, tableId, rowId, valuesJSON, position, aiFields, maxBytes)
}

// wholeRowCellsSQL is a whole row's cells as written ($3, over the row's
// values as they were, c.old), the AI columns' among them ($6) as stored
// unless one of them is the only cell the row changes. The web released
// before patches saves a cell edit by sending its copy of the whole row, the
// AI cells as it last showed them: an answer that landed after it read the
// row was put back to the one before, or taken out where the copy had none,
// and marked a person's correction.
//
// The changes are counted against the row as stored: each cell the row names
// that differs, and each AI cell it leaves out. An AI cell it names that is
// the only change is a person's correction, written and marked. Other cells
// it leaves out aren't counted (a deleted field's, which no page shows, among
// them): a whole row drops them either way.
//
// What's left: a correction made from such a copy that changes anything
// else, another AI cell among it, is left out, as no whole row can tell it
// from the copy's stale cells; the page shows the cell as stored when it
// reads the row again. A patch naming the cell (MergeRowValues) is always
// written.
const wholeRowCellsSQL = `CASE WHEN (SELECT COUNT(*) = 1 AND bool_or(named) FROM (
		SELECT e.k = ANY(COALESCE($6::text[], '{}')) FROM jsonb_each($3::jsonb) AS e(k, v)
		WHERE e.v IS DISTINCT FROM c.old -> e.k
		UNION ALL
		SELECT false FROM unnest(COALESCE($6::text[], '{}')) AS f WHERE c.old ? f AND NOT $3::jsonb ? f
	) AS d(named))
	THEN $3::jsonb
	ELSE ($3::jsonb - COALESCE($6::text[], '{}')) || COALESCE((SELECT jsonb_object_agg(f, c.old -> f)
		FROM unnest(COALESCE($6::text[], '{}')) AS f WHERE c.old ? f), '{}'::jsonb) END`

// ErrRowTooLarge is a merge that would take a row past the size it may be.
var ErrRowTooLarge = errors.New("row is too large")

// MergeRowValues writes the given cells into a row in one statement, leaving
// its other cells and its place as they are now, not as they were when the
// caller read it (an AI fill reads the row, waits seconds for the model, and
// used to write that copy back over anything changed meanwhile). The merged
// row may be at most maxBytes. It returns the row, and its values as they
// were (writeCells).
func MergeRowValues(ctx context.Context, tableId, rowId uuid.UUID, patchJSON string, aiFields []string, maxBytes int) (*Row, string, error) {
	return writeCells(ctx, "c.old || $3::jsonb", tableId, rowId, patchJSON, nil, aiFields, maxBytes)
}

// writeCellsSQL writes a row's cells as cells (%s, over the row's values as
// they were, c.old, and the cells given, $3), its place ($5, unless null),
// and marks the AI columns among $6 whose cell it changes as a person's
// (AIKeptKey), all in one statement: a fill's answer written meanwhile can't
// be mistaken for theirs, and two writes can't each mark over the other. It
// returns the row, and its values as they were.
const writeCellsSQL = `WITH c AS (
		SELECT id, values AS old FROM data_table_rows
		WHERE id = $1 AND table_id = $2 AND deleted_at IS NULL FOR UPDATE
	), n AS (
		SELECT id, old, %s AS cells, COALESCE(old -> '` + AIKeptKey + `', '[]'::jsonb) AS kept FROM c
	), m AS (
		SELECT id, old, cells, kept || COALESCE((SELECT jsonb_agg(f) FROM unnest($6::text[]) f
			WHERE (cells -> f) IS DISTINCT FROM (old -> f) AND NOT kept ? f), '[]'::jsonb) AS kept
		FROM n
	)
	UPDATE data_table_rows r SET
		values = CASE WHEN jsonb_array_length(m.kept) = 0 THEN m.cells - '` + AIKeptKey + `'
			ELSE m.cells || jsonb_build_object('` + AIKeptKey + `', m.kept) END,
		position = COALESCE($5::double precision, r.position), updated_at = NOW()
	FROM m
	WHERE r.id = m.id AND octet_length(m.cells::text) <= $4
	RETURNING ` + rowColumnsOfR + `, m.old`

// rowColumnsOfR is rowColumns of a statement's row named r.
const rowColumnsOfR = `r.id, r.table_id, r.values, r.position, r.created_by, r.created_at, r.updated_at, r.deleted_at`

func writeCells(ctx context.Context, cells string, tableId, rowId uuid.UUID, cellsJSON string, position *float64, aiFields []string, maxBytes int) (*Row, string, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var old string
	out, err := scanRow(thenScan{postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, fmt.Sprintf(writeCellsSQL, cells),
		rowId, tableId, cellsJSON, maxBytes, position, pq.Array(aiFields)), []any{&old}})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", whyNotWritten(dbctx, tableId, rowId)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/writeCells err: %+v", err)
		return nil, "", err
	}
	return out, old, nil
}

// whyNotWritten is why a write to a row changed nothing: it's too large, or
// the row is gone (sql.ErrNoRows).
func whyNotWritten(ctx context.Context, tableId, rowId uuid.UUID) error {
	var live bool
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM data_table_rows WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL)`,
		rowId, tableId).Scan(&live); err == nil && live {
		return ErrRowTooLarge
	}
	return sql.ErrNoRows
}

// ErrAIKept is an AI fill's answer for a cell a person wrote since.
var ErrAIKept = errors.New("a person wrote this cell")

// ErrCellChanged is an AI fill's answer for a cell that changed while the
// model answered: a person's correction, an agent's write, or a newer fill;
// or for a row whose cells the answer was worked out from changed meanwhile.
var ErrCellChanged = errors.New("the cell changed while the AI answered")

// unchangedSQL is whether a row's values (%s) still hold the cells $N names
// ({field: value}, JSON null for one the row didn't have) as they were read.
const unchangedSQL = `NOT EXISTS (SELECT 1 FROM jsonb_each(COALESCE(%s::jsonb, '{}'::jsonb)) AS s(k, v)
	WHERE COALESCE(%s -> s.k, 'null'::jsonb) IS DISTINCT FROM s.v)`

// WriteAICell writes an AI column's answer (cellJSON, {field: answer}) into a
// row, in one statement, only while the cell still holds what the fill read
// (read; nil when the row had none) and the cells the answer was worked out
// from (inputs, as unchangedSQL reads them) are as the fill read them
// (ErrCellChanged otherwise), and unless a person wrote it (AIKept:
// ErrAIKept). Of two fills for two quick edits, the first to answer read the
// older cells, and wrote over the newer one's answer. again ("Fill again")
// writes over a person's cell, and takes the mark off, but never over one
// changed meanwhile.
func WriteAICell(ctx context.Context, tableId, rowId uuid.UUID, field, cellJSON string, read json.RawMessage, inputs string, again bool, maxBytes int) (*Row, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var was any // SQL NULL: the row had no such cell
	if read != nil {
		was = string(read)
	}
	var reads any // SQL NULL: the answer read no cell
	if inputs != "" {
		reads = inputs
	}
	q := `WITH c AS (
			SELECT id, values AS old, COALESCE(values -> '` + AIKeptKey + `', '[]'::jsonb) AS kept FROM data_table_rows
			WHERE id = $1 AND table_id = $2 AND deleted_at IS NULL FOR UPDATE
		)
		UPDATE data_table_rows r SET
			values = CASE WHEN jsonb_array_length(c.kept - $5::text) = 0 THEN (c.old || $3::jsonb) - '` + AIKeptKey + `'
				ELSE (c.old || $3::jsonb) || jsonb_build_object('` + AIKeptKey + `', c.kept - $5::text) END,
			updated_at = NOW()
		FROM c
		WHERE r.id = c.id AND ($6 OR NOT c.kept ? $5) AND (c.old -> $5::text) IS NOT DISTINCT FROM $7::jsonb
			AND ` + fmt.Sprintf(unchangedSQL, "$8", "c.old") + `
			AND octet_length((c.old || $3::jsonb)::text) <= $4
		RETURNING ` + rowColumnsOfR
	out, err := scanRow(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, rowId, tableId, cellJSON, maxBytes, field, again, was, reads))
	if errors.Is(err, sql.ErrNoRows) {
		var kept, same bool
		if qerr := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
			`SELECT COALESCE(values -> '`+AIKeptKey+`', '[]'::jsonb) ? $3, (values -> $3::text) IS NOT DISTINCT FROM $4::jsonb
				AND `+fmt.Sprintf(unchangedSQL, "$5", "values")+`
			FROM data_table_rows WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL`,
			rowId, tableId, field, was, reads).Scan(&kept, &same); qerr == nil {
			switch {
			case kept && !again:
				return nil, ErrAIKept
			case !same:
				return nil, ErrCellChanged
			}
		}
		return nil, whyNotWritten(dbctx, tableId, rowId)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/WriteAICell err: %+v", err)
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

// rowOrder is the order rows are read in, page after page: by place, then
// age, then id, so rows tying on both (made in one statement) come in one
// order on every read, and a page's edge never falls between them two ways
// (a row skipped on one page and repeated on the next).
const rowOrder = `ORDER BY position ASC, created_at ASC, id ASC`

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
		WHERE table_id=$1 AND deleted_at IS NULL ` + rowOrder + `
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

// CountRows returns the number of non-deleted rows in a table.
func CountRows(ctx context.Context, tableId uuid.UUID) (int, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT COUNT(*) FROM data_table_rows WHERE table_id=$1 AND deleted_at IS NULL`
	var n int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, tableId).Scan(&n); err != nil {
		helpers.LogErrorWithContext(ctx, "models/CountRows err: %+v", err)
		return 0, err
	}
	return n, nil
}

// stripItemLinksSQL reduces each link to an item (an object whose type is
// among $3) in a page of rows, $2 of them in id order after $1, deleted rows
// and every cell among them, to its {id, type}: whatever else was stored with
// one, the item's title among it, goes. It answers the page's last id (none
// past the end) and how many rows it changed. A row keeps its updated_at, as
// no reader sees a change: each read names a row's links for its reader.
const stripItemLinksSQL = `WITH page AS (
		SELECT id, CASE WHEN jsonb_typeof(values) = 'object' THEN values ELSE '{}'::jsonb END AS cells
		FROM data_table_rows WHERE id > $1 ORDER BY id LIMIT $2
	), fixed AS (
		UPDATE data_table_rows r SET values = (
			SELECT jsonb_object_agg(c.k, CASE
				WHEN jsonb_typeof(c.v) = 'array' THEN COALESCE((
					SELECT jsonb_agg(CASE WHEN jsonb_typeof(a.e) = 'object' AND a.e ->> 'type' = ANY($3)
						THEN jsonb_build_object('id', a.e -> 'id', 'type', a.e -> 'type') ELSE a.e END ORDER BY a.n)
					FROM jsonb_array_elements(c.v) WITH ORDINALITY AS a(e, n)), '[]'::jsonb)
				WHEN jsonb_typeof(c.v) = 'object' AND c.v ->> 'type' = ANY($3)
					THEN jsonb_build_object('id', c.v -> 'id', 'type', c.v -> 'type')
				ELSE c.v END)
			FROM jsonb_each(r.values) AS c(k, v))
		FROM page p
		WHERE r.id = p.id AND EXISTS (
			SELECT 1 FROM jsonb_each(p.cells) AS c(k, v),
				jsonb_array_elements(CASE WHEN jsonb_typeof(c.v) = 'array' THEN c.v ELSE jsonb_build_array(c.v) END) AS a(e)
			WHERE jsonb_typeof(a.e) = 'object' AND a.e ->> 'type' = ANY($3) AND a.e - 'id' - 'type' <> '{}'::jsonb)
		RETURNING r.id
	)
	SELECT (SELECT id FROM page ORDER BY id DESC LIMIT 1), (SELECT COUNT(*) FROM fixed)`

// StripItemLinks reduces the links to items of the kinds given, in a page of
// rows after the row after (in id order, limit of them), to their {id, type}
// (stripItemLinksSQL). It answers the page's last row, uuid.Nil past the end,
// and how many rows it changed.
func StripItemLinks(ctx context.Context, kinds []string, after uuid.UUID, limit int) (uuid.UUID, int, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var last uuid.NullUUID
	var n int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, stripItemLinksSQL, after, limit, kinds).Scan(&last, &n); err != nil {
		helpers.LogErrorWithContext(ctx, "models/StripItemLinks err: %+v", err)
		return uuid.Nil, 0, err
	}
	return last.UUID, n, nil
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
	sb.WriteString(fmt.Sprintf(` %s LIMIT $%d OFFSET $%d`, rowOrder, n, n+1))
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
