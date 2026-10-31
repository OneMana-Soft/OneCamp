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
	"github.com/akashc777/OneCamp/models/pgexec"
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
	// (business/Formula). Its config carries {"formula": "..."}, with
	// fields named by id; its cells are never stored.
	FieldFormula = "formula"
	// FieldRollup is worked out on each read from the rows a relation field
	// links to (business/DataTable/relations.go). Its config carries
	// {"relation": "<relation field id>", "field": "<field id in the linked
	// table>", "aggregate": "<how>"}; its cells are never stored.
	FieldRollup = "rollup"

	// The kinds tables B1 adds (D-TB-3), each a business/FieldTypes kind.
	// Stored: a status is a select whose options sit in groups (to do,
	// doing, done); people are links to members, stored as a relation to
	// people is ([{id, type: "user"}]); a long text keeps its lines; a phone
	// number its characters; a duration its seconds; a rating its stars. A
	// currency and a percent stay a number's format.
	FieldStatus   = "status"
	FieldPeople   = "people"
	FieldLongText = "long_text"
	FieldPhone    = "phone"
	FieldDuration = "duration"
	FieldRating   = "rating"
	// FieldAttachment is files ([{id, name, size, mime}]): its table's
	// files (attachments whose place is the table), shown as Files.
	FieldAttachment = "attachment"
	// Worked out from the row on each read, never stored: its number in the
	// table (with the field's prefix, an id like DEAL-12), when it was made
	// and last changed, and by whom.
	FieldAutonumber   = "autonumber"
	FieldCreatedTime  = "created_time"
	FieldCreatedBy    = "created_by"
	FieldModifiedTime = "modified_time"
	FieldModifiedBy   = "modified_by"
)

// View type constants.
const (
	ViewGrid     = "grid"
	ViewBoard    = "board"
	ViewCalendar = "calendar"
	ViewChart    = "chart"
	// B4's layouts: cards, a list of rows, bars across their dates, and the
	// bars with the links that say what waits on what (migration 927).
	ViewGallery  = "gallery"
	ViewList     = "list"
	ViewTimeline = "timeline"
	ViewGantt    = "gantt"
)

// ValidVisibility / ValidViewType validate inputs. Which kinds a field may be
// is business/FieldTypes' (Kinds.Valid): a kind registered there.
func ValidVisibility(v string) bool { return v == VisibilityPrivate || v == VisibilityWorkspace }

func ValidViewType(t string) bool {
	switch t {
	case ViewGrid, ViewBoard, ViewCalendar, ViewChart, ViewGallery, ViewList, ViewTimeline, ViewGantt:
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
	Id       uuid.UUID `json:"id"`
	TableId  uuid.UUID `json:"table_id"`
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	Config   string    `json:"config"` // raw JSON object
	Position float64   `json:"position"`
	// Description says what the field is for: its header and the record
	// show it (migration 300).
	Description *string `json:"description,omitempty"`
	// Required is a field every row made through the API, an AI tool or a
	// form must give. Left out when it isn't, as a guest's copy always is.
	Required bool `json:"required,omitempty"`
	// Default is what a new row's cell starts as (JSON as stored).
	Default json.RawMessage `json:"default_value,omitempty"`
	// Primary is the field that names the table's rows: one per table.
	Primary   bool       `json:"primary"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// Row mirrors a row of data_table_rows.
type Row struct {
	Id       uuid.UUID `json:"id"`
	TableId  uuid.UUID `json:"table_id"`
	Values   string    `json:"values"` // raw JSON object keyed by field id
	Position float64   `json:"position"`
	// RowNo is the row's number in its table, in the order rows were made,
	// never given twice (migration 300): autonumbers and short links.
	RowNo     int64      `json:"row_no,omitempty"`
	CreatedBy *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// AIKept is the AI columns whose cell in this row a person wrote: a fill
	// leaves them as they are until someone asks for it again.
	AIKept []string `json:"ai_kept,omitempty"`
	// Version is how many times the row has been written (migration 301): a
	// writer may name the version it read, and is refused if the row was
	// written since.
	Version int64 `json:"version"`
	// PositionKey is the row's place among its table's rows, which are read
	// in key order: a fractional key (business/Tree/position's), so a row
	// goes between two others, or moves, without renumbering any.
	PositionKey string `json:"position_key,omitempty"`
	// UpdatedBy is who last changed the row; DeletedBy who deleted it.
	UpdatedBy *uuid.UUID `json:"updated_by,omitempty"`
	DeletedBy *uuid.UUID `json:"deleted_by,omitempty"`
	// Comments is how many comments the row's open threads hold, for the
	// member's grid (business/DataTable WithCommentCounts); never stored, and
	// absent from what the API, AI tools and guests read.
	Comments int `json:"comments,omitempty"`
	// Page is the row's page, the doc it hosts (migration 307), once someone
	// has written in it: for the member's grid and record (business/DataTable
	// WithPages); absent from what the API, AI tools and guests read.
	Page *uuid.UUID `json:"page,omitempty"`
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

// View mirrors a row of data_table_views. Owner is the person whose own view
// it is (migration 512); a view with none is shared with everyone who reads
// the table, and a locked one takes no change until it's unlocked.
type View struct {
	Id        uuid.UUID  `json:"id"`
	TableId   uuid.UUID  `json:"table_id"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Config    string     `json:"config"` // raw JSON object
	Position  float64    `json:"position"`
	Owner     *uuid.UUID `json:"owner_id,omitempty"`
	Locked    bool       `json:"locked"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

type scanner interface {
	Scan(dest ...any) error
}

// thenScan is a scanner of a statement's columns followed by more, which it
// reads into more.
type thenScan struct {
	s    scanner
	more []any
}

func (t thenScan) Scan(dest ...any) error { return t.s.Scan(append(dest, t.more...)...) }

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
// table_id scoping of every read), by who deleted it: it goes to the trash
// (DeletedTables), from which RestoreTable brings it back.
func SoftDeleteTable(ctx context.Context, id, by uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE data_tables SET deleted_at=NOW(), updated_at=NOW(), deleted_by=$2 WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, nullUUID(by))
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
	var description sql.NullString
	var def []byte
	if err := s.Scan(&f.Id, &f.TableId, &f.Name, &f.Type, &f.Config, &f.Position,
		&description, &f.Required, &def, &f.Primary,
		&f.CreatedAt, &f.UpdatedAt, &deletedAt); err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		f.DeletedAt = &deletedAt.Time
	}
	if description.Valid {
		f.Description = &description.String
	}
	if len(def) > 0 && string(def) != "null" {
		f.Default = json.RawMessage(def)
	}
	if strings.TrimSpace(f.Config) == "" {
		f.Config = "{}"
	}
	return &f, nil
}

const fieldColumns = `id, table_id, name, type, config, position, description, required, default_value, is_primary, created_at, updated_at, deleted_at`

// Every write to data_table_fields, by the functions below or any other,
// fires the Computed Store's trigger (data_table_computed_field_written:
// migration 535's, airtable-03; 538's since V-108): a formula or rollup is
// held from the moment it is made, and a field made or deleted, or changed in
// type, config, deleted_at or is_primary, raises in the writer's own
// transaction the generation of every kept value it can change, which is
// then filled again. A new writer gets that without a call. A new column that
// changes what a formula or rollup gives must join the trigger's WHEN list,
// in a migration of its own.

// CreateField inserts a column.
func CreateField(ctx context.Context, f *Field) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(f.Config) == "" {
		f.Config = "{}"
	}
	id := uuid.New()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, insertFieldSQL, id, f.TableId, f.Name, f.Type, f.Config, f.Position,
		f.Description, f.Required, defaultOf(f))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateField err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// insertFieldSQL inserts a field ($1 its id), the table's primary when the
// table has none: its first field, as the web has always shown the first one.
const insertFieldSQL = `INSERT INTO data_table_fields (id, table_id, name, type, config, position, description, required, default_value, is_primary)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, NOT EXISTS (
		SELECT 1 FROM data_table_fields WHERE table_id = $2 AND is_primary AND deleted_at IS NULL))`

// defaultOf is a field's default as a statement takes it: SQL NULL for none.
func defaultOf(f *Field) any {
	if len(f.Default) == 0 || string(f.Default) == "null" {
		return nil
	}
	return string(f.Default)
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
	const q = `UPDATE data_table_fields SET name=$2, type=$3, config=$4, position=$5,
		description=$7, required=$8, default_value=$9, updated_at=NOW()
		WHERE id=$1 AND table_id=$6 AND deleted_at IS NULL`
	res, err := db.ExecContext(ctx, q, f.Id, f.Name, f.Type, f.Config, f.Position, f.TableId, f.Description, f.Required, defaultOf(f))
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

// cellsChangedOf is what a rewrite of a field's ($2) cells returns: each row
// it changed, and its cell before (o, the row as the statement found it) and
// after, for the row ledger.
const cellsChangedOf = `r.id, o.values -> $2::text, r.values -> $2::text`

// relabelSelectCells and relabelMultiCells rewrite a field's cells by a map
// of labels ($3, {"old": "new"}, "" to take one out), a row at a time in one
// statement, so the map applies to every label at once (two options swapping
// names swap in their cells too). A select cell is a label; a multi-select
// cell is a list of them, which keeps its order and each label once.
const relabelSelectCells = `UPDATE data_table_rows r
	SET values = jsonb_set(r.values, ARRAY[$2::text], to_jsonb($3::jsonb ->> (r.values ->> $2::text))), updated_at = NOW(),
		version = r.version + 1
	FROM data_table_rows o
	WHERE o.id = r.id AND r.table_id = $1 AND r.deleted_at IS NULL
	  AND jsonb_typeof(r.values -> $2::text) = 'string' AND $3::jsonb ? (r.values ->> $2::text)
	RETURNING ` + cellsChangedOf

const relabelMultiCells = `UPDATE data_table_rows r
	SET values = jsonb_set(r.values, ARRAY[$2::text], COALESCE((
		SELECT jsonb_agg(x.v ORDER BY x.n) FROM (
			SELECT DISTINCT ON (m.v) m.v, m.n FROM (
				SELECT CASE WHEN jsonb_typeof(e.j) = 'string' AND $3::jsonb ? (e.j #>> '{}')
					THEN to_jsonb($3::jsonb ->> (e.j #>> '{}')) ELSE e.j END AS v, e.n
				FROM jsonb_array_elements(r.values -> $2::text) WITH ORDINALITY AS e(j, n)
			) m WHERE m.v <> '""'::jsonb ORDER BY m.v, m.n
		) x), '[]'::jsonb)), updated_at = NOW(), version = r.version + 1
	FROM data_table_rows o
	WHERE o.id = r.id AND r.table_id = $1 AND r.deleted_at IS NULL AND jsonb_typeof(r.values -> $2::text) = 'array'
	  AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(r.values -> $2::text) t WHERE $3::jsonb ? t)
	RETURNING ` + cellsChangedOf

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
const setCellsSQL = `UPDATE data_table_rows r SET values = jsonb_set(r.values, ARRAY[$2::text], u.cell::jsonb), updated_at = NOW(),
		version = r.version + 1
	FROM unnest($3::uuid[], $4::text[]) AS u(id, cell), data_table_rows o
	WHERE r.id = u.id AND o.id = r.id AND r.table_id = $1 AND r.deleted_at IS NULL
	RETURNING ` + cellsChangedOf

// FieldPlan works out a field update from the field as the update finds it,
// locked: the field to write, how its cells convert as its type changes (nil
// for none), and the rewrites to its cells after that.
type FieldPlan func(current *Field) (*Field, Convert, []CellRewrite, error)

// UpdateFieldAndCells updates a field and its cells in one transaction, so no
// reader finds the field changed and its cells not. The update is planned
// from the field as the transaction finds it, locked: two saves planned from
// one copy (an option renamed twice in quick succession) would each rewrite
// the cells as the field was before the other. It has BulkTimeout, as the
// cells of every row may change. The cells it changes are cs's changes,
// stored in the same transaction (the row ledger; cs nil records none). It
// returns how many rows' cells changed; a field that's gone is sql.ErrNoRows.
func UpdateFieldAndCells(ctx context.Context, tableID, fieldID uuid.UUID, plan FieldPlan, cs *ChangeSet, committing Committing) (int, error) {
	changed := map[uuid.UUID]bool{}
	var cells []Change
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
			ids, cellsOf := make([]uuid.UUID, 0, len(converted)), make([]string, 0, len(converted))
			for id, cell := range converted {
				ids, cellsOf = append(ids, id), append(cellsOf, string(cell))
			}
			// A list a row long goes to pgx as it is, as in CellsOf: through
			// pq.Array, pgx writes out the whole list in an error before taking it.
			if err := collectCells(ctx, tx, changed, &cells, f.Id.String(), setCellsSQL, f.TableId, f.Id.String(), ids, cellsOf); err != nil {
				helpers.LogErrorWithContext(ctx, "models/UpdateFieldAndCells convert err: %+v", err)
				return err
			}
		}
		for _, rw := range rewrites {
			if err := collectCells(ctx, tx, changed, &cells, f.Id.String(), rw.query, append([]any{f.TableId, f.Id.String()}, rw.args...)...); err != nil {
				helpers.LogErrorWithContext(ctx, "models/UpdateFieldAndCells cells err: %+v", err)
				return err
			}
		}
		if cs == nil || len(cells) == 0 {
			return nil
		}
		cs.Changes = append(cs.Changes, cells...)
		if err := recordTx(ctx, tx, cs); err != nil {
			return err
		}
		if committing != nil {
			return committing(ctx, tx, cs)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(changed), nil
}

// collectCells runs a statement rewriting a field's cells (cellsChangedOf),
// adding the rows it changed to into, and each cell's change to cells.
func collectCells(ctx context.Context, tx *sql.Tx, into map[uuid.UUID]bool, cells *[]Change, field, query string, args ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var before, after []byte
		if err := rows.Scan(&id, &before, &after); err != nil {
			return err
		}
		into[id] = true
		c := Change{Row: id, Field: field}
		if before != nil {
			c.Before = json.RawMessage(before)
		}
		if after != nil {
			c.After = json.RawMessage(after)
		}
		if !sameJSON(c.Before, c.After) {
			*cells = append(*cells, c)
		}
	}
	return rows.Err()
}

// OptionFieldsWithoutIDs is a page of the live select and multi-select
// fields, in id order after after, that keep an option without an id.
func OptionFieldsWithoutIDs(ctx context.Context, after uuid.UUID, limit int) ([]*Field, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, `SELECT `+fieldColumns+` FROM data_table_fields
		WHERE id > $1 AND deleted_at IS NULL AND type IN ('select', 'multi_select')
		  AND jsonb_typeof(config -> 'options') = 'array'
		  AND EXISTS (SELECT 1 FROM jsonb_array_elements(config -> 'options') o
		              WHERE jsonb_typeof(o) <> 'object' OR COALESCE(o ->> 'id', '') = '')
		ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/OptionFieldsWithoutIDs err: %+v", err)
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

// SetFieldConfigIf sets a live field's config to next while it is still was,
// and says whether it was: a save made meanwhile is never written over. The
// field's updated_at stays, as no reader sees a change.
func SetFieldConfigIf(ctx context.Context, id uuid.UUID, was, next string) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, `UPDATE data_table_fields SET config = $3
		WHERE id = $1 AND deleted_at IS NULL AND config = $2::jsonb`, id, was, next)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetFieldConfigIf err: %+v", err)
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetPrimary makes a field its table's primary one, the first of its
// columns, in one transaction: the field that was primary isn't any more, and
// no reader finds two or none.
func SetPrimary(ctx context.Context, tableId, fieldId uuid.UUID) error {
	return postgresInit.InTx(ctx, postgresInit.DBConn.DBTimeout, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE data_table_fields SET is_primary = false, updated_at = NOW()
			WHERE table_id = $1 AND is_primary AND id <> $2`, tableId, fieldId); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE data_table_fields f SET is_primary = true, updated_at = NOW(),
			position = LEAST(f.position, (SELECT MIN(position) - 1 FROM data_table_fields
				WHERE table_id = $1 AND deleted_at IS NULL AND id <> $2))
			WHERE f.id = $2 AND f.table_id = $1 AND f.deleted_at IS NULL`, tableId, fieldId)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// DeleteField soft-deletes a column.
func DeleteField(ctx context.Context, tableId, fieldId, by uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	return deleteField(dbctx, postgresInit.DBConn.SqlDB, tableId, fieldId, by)
}

// deleteField soft-deletes a live field of a table on db; by is who deleted
// it. It is sql.ErrNoRows when there's no such field.
func deleteField(ctx context.Context, db execer, tableId, fieldId, by uuid.UUID) error {
	res, err := db.ExecContext(ctx, `UPDATE data_table_fields SET deleted_at=NOW(), updated_at=NOW(), deleted_by=$3
		WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL`, fieldId, tableId, nullUUID(by))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteField err: %+v", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
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
	var createdBy, updatedBy, deletedBy uuid.NullUUID
	var deletedAt sql.NullTime
	var rowNo sql.NullInt64
	var key sql.NullString
	if err := s.Scan(&r.Id, &r.TableId, &r.Values, &r.Position, &rowNo, &createdBy,
		&r.CreatedAt, &r.UpdatedAt, &deletedAt, &r.Version, &key, &updatedBy, &deletedBy); err != nil {
		return nil, err
	}
	r.RowNo = rowNo.Int64
	r.PositionKey = key.String
	if createdBy.Valid {
		r.CreatedBy = &createdBy.UUID
	}
	if updatedBy.Valid {
		r.UpdatedBy = &updatedBy.UUID
	}
	if deletedBy.Valid {
		r.DeletedBy = &deletedBy.UUID
	}
	if deletedAt.Valid {
		r.DeletedAt = &deletedAt.Time
	}
	if strings.TrimSpace(r.Values) == "" {
		r.Values = "{}"
	}
	return &r, nil
}

const rowColumns = `id, table_id, values, position, row_no, created_by, created_at, updated_at, deleted_at, version, position_key, updated_by, deleted_by`

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
// it leaves out aren't counted: a whole row drops them.
//
// Cells of the table's fields in the trash (trashedFieldsSQL) are kept as
// stored, whatever the row says: no read shows them, so no copy has them
// (or has them as they were before the delete), and the field's restore
// brings them back (tables A2). A patch leaves them alone as it is.
//
// What's left: a correction made from such a copy that changes anything
// else, another AI cell among it, is left out, as no whole row can tell it
// from the copy's stale cells; the page shows the cell as stored when it
// reads the row again. A patch naming the cell (MergeRowValues) is always
// written.
const wholeRowCellsSQL = `((CASE WHEN (SELECT COUNT(*) = 1 AND bool_or(named) FROM (
		SELECT e.k = ANY(COALESCE($6::text[], '{}')) FROM jsonb_each($3::jsonb - ` + trashedFieldsSQL + `) AS e(k, v)
		WHERE e.v IS DISTINCT FROM c.old -> e.k
		UNION ALL
		SELECT false FROM unnest(COALESCE($6::text[], '{}')) AS f WHERE c.old ? f AND NOT $3::jsonb ? f
	) AS d(named))
	THEN $3::jsonb
	ELSE ($3::jsonb - COALESCE($6::text[], '{}')) || COALESCE((SELECT jsonb_object_agg(f, c.old -> f)
		FROM unnest(COALESCE($6::text[], '{}')) AS f WHERE c.old ? f), '{}'::jsonb) END) - ` + trashedFieldsSQL + `)
	|| COALESCE((SELECT jsonb_object_agg(k, c.old -> k) FROM unnest(` + trashedFieldsSQL + `) AS k WHERE c.old ? k), '{}'::jsonb)`

// trashedFieldsSQL is the ids of the fields in the trash of the table a
// statement writes ($2): idx_data_table_fields_trash finds them.
const trashedFieldsSQL = `ARRAY(SELECT id::text FROM data_table_fields WHERE table_id = $2 AND deleted_at IS NOT NULL)`

// ErrRowTooLarge is a merge that would take a row past the size it may be.
var ErrRowTooLarge = errors.New("row is too large")

// writeCellsSQL writes a row's cells as cells (%s, over the row's values as
// they were, c.old, and the cells given, $3), its place ($5, unless null),
// and marks the AI columns among $6 whose cell it changes as a person's
// (AIKeptKey), all in one statement: a fill's answer written meanwhile can't
// be mistaken for theirs, and two writes can't each mark over the other. It
// adds one to the row's version and names who wrote it ($7), and returns the
// row and its values as they were (WriteRow).
var writeCellsSQL = `WITH c AS (
		SELECT id, values AS old FROM data_table_rows
		WHERE id = $1 AND table_id = $2 AND deleted_at IS NULL FOR UPDATE
	), n AS (
		SELECT id, old, %s AS cells, ` + fmt.Sprintf(keptSQL, "old") + ` AS kept FROM c
	), m AS (
		SELECT id, old, cells, kept || COALESCE((SELECT jsonb_agg(f) FROM unnest($6::text[]) f
			WHERE (cells -> f) IS DISTINCT FROM (old -> f) AND NOT kept ? f), '[]'::jsonb) AS kept
		FROM n
	)
	UPDATE data_table_rows r SET
		values = CASE WHEN jsonb_array_length(m.kept) = 0 THEN m.cells - '` + AIKeptKey + `'
			ELSE m.cells || jsonb_build_object('` + AIKeptKey + `', m.kept) END,
		position = COALESCE($5::double precision, r.position), updated_at = NOW(),
		version = r.version + 1, updated_by = $7
	FROM m
	WHERE r.id = m.id AND octet_length(m.cells::text) <= $4
	RETURNING ` + rowColumnsOfR + `, m.old`

// rowColumnsOfR is rowColumns of a statement's row named r.
const rowColumnsOfR = `r.id, r.table_id, r.values, r.position, r.row_no, r.created_by, r.created_at, r.updated_at, r.deleted_at, r.version, r.position_key, r.updated_by, r.deleted_by`

// ErrAIKept is an AI fill's answer for a cell a person wrote since.
var ErrAIKept = errors.New("a person wrote this cell")

// ErrCellChanged is an AI fill's answer for a cell that changed while the
// model answered: a person's correction, an agent's write, or a newer fill;
// or for a row whose cells the answer was worked out from changed meanwhile.
var ErrCellChanged = errors.New("the cell changed while the AI answered")

// keptSQL is the cells of a row's values (%s) a person wrote (AIKeptKey), a
// JSON array of their fields: none when there's no list.
const keptSQL = `COALESCE(%s -> '` + AIKeptKey + `', '[]'::jsonb)`

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
// changed meanwhile. It adds one to the row's version, and its change to cs,
// in the write's transaction (the row ledger); who last changed the row
// stays who it was.
func WriteAICell(ctx context.Context, tableId, rowId uuid.UUID, field, cellJSON string, read json.RawMessage, inputs string, again bool, maxBytes int, cs *ChangeSet, committing Committing) (*Row, error) {
	var was any // SQL NULL: the row had no such cell
	if read != nil {
		was = string(read)
	}
	var reads any // SQL NULL: the answer read no cell
	if inputs != "" {
		reads = inputs
	}
	q := `WITH c AS (
			SELECT id, values AS old, ` + fmt.Sprintf(keptSQL, "values") + ` AS kept FROM data_table_rows
			WHERE id = $1 AND table_id = $2 AND deleted_at IS NULL FOR UPDATE
		)
		UPDATE data_table_rows r SET
			values = CASE WHEN jsonb_array_length(c.kept - $5::text) = 0 THEN (c.old || $3::jsonb) - '` + AIKeptKey + `'
				ELSE (c.old || $3::jsonb) || jsonb_build_object('` + AIKeptKey + `', c.kept - $5::text) END,
			updated_at = NOW(), version = r.version + 1
		FROM c
		WHERE r.id = c.id AND ($6 OR NOT c.kept ? $5) AND (c.old -> $5::text) IS NOT DISTINCT FROM $7::jsonb
			AND ` + fmt.Sprintf(unchangedSQL, "$8", "c.old") + `
			AND octet_length((c.old || $3::jsonb)::text) <= $4
		RETURNING ` + rowColumnsOfR + `, c.old`
	var out *Row
	err := ledgerTx(ctx, 1, cs, committing, func(dbctx context.Context, tx *sql.Tx) error {
		var old string
		r, err := scanRow(thenScan{tx.QueryRowContext(dbctx, q, rowId, tableId, cellJSON, maxBytes, field, again, was, reads), []any{&old}})
		if errors.Is(err, sql.ErrNoRows) {
			var kept, same bool
			if qerr := tx.QueryRowContext(dbctx,
				`SELECT `+fmt.Sprintf(keptSQL, "values")+` ? $3, (values -> $3::text) IS NOT DISTINCT FROM $4::jsonb
					AND `+fmt.Sprintf(unchangedSQL, "$5", "values")+`
				FROM data_table_rows WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL`,
				rowId, tableId, field, was, reads).Scan(&kept, &same); qerr == nil {
				switch {
				case kept && !again:
					return ErrAIKept
				case !same:
					return ErrCellChanged
				}
			}
			return whyNotWrittenTx(dbctx, tx, tableId, rowId)
		}
		if err != nil {
			return err
		}
		if cs != nil {
			cs.Changes = append(cs.Changes, cellChanges(rowId, old, r.Values)...)
		}
		out = r
		return nil
	})
	if err != nil {
		if !errors.Is(err, ErrAIKept) && !errors.Is(err, ErrCellChanged) && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, ErrRowTooLarge) {
			helpers.LogErrorWithContext(ctx, "models/WriteAICell err: %+v", err)
		}
		return nil, err
	}
	return out, nil
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

// rowOrder is the order rows are read in, page after page: by place (their
// position keys, migration 301, through idx_data_table_rows_order), then id,
// so rows tying on a key come in one order on every read and a page's edge
// never falls between them two ways (a row skipped on one page and repeated
// on the next). A row with no key yet (one made by a server from before the
// keys, between the migration and the new server starting) comes last, until
// KeyRowsWithout gives it one.
const rowOrder = `ORDER BY position_key ASC, id ASC`

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

// emptyCellSQL is whether r's cell of the field $2 holds nothing (none, null
// or "") and no person wrote it (AIKeptKey): what a fill of an AI column
// writes.
var emptyCellSQL = `COALESCE(r."values" ->> $2::text, '') = '' AND NOT ` + fmt.Sprintf(keptSQL, `r."values"`) + ` ? $2::text`

// fillableSQL is the rows a column's fill takes, as it counts them and as it
// reads them (CountRowsToFill, RowsToFillAfter): live, with a key (a row with
// none yet has no place for a fill's cursor to walk past), and an empty cell
// no person wrote. One predicate, so a fill's total is what it walks.
var fillableSQL = `r.deleted_at IS NULL AND r.position_key IS NOT NULL AND ` + emptyCellSQL

// RowsToFill is up to limit of a table's rows whose cell of field is empty
// and no person's (emptyCellSQL), in the table's order.
func RowsToFill(ctx context.Context, tableId uuid.UUID, field string, limit int) ([]*Row, error) {
	rows, err := pgexec.Query(ctx, `SELECT `+rowColumns+` FROM data_table_rows r
		WHERE r.table_id = $1 AND r.deleted_at IS NULL AND `+emptyCellSQL+` `+rowOrder+` LIMIT $3`,
		func(r pgexec.Row) (*Row, error) { return scanRow(r) }, tableId, field, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RowsToFill err: %+v", err)
		return nil, err
	}
	return rows, nil
}

// CountRowsToFill is how many of a table's rows a column's fill takes
// (fillableSQL): its total.
func CountRowsToFill(ctx context.Context, tableId uuid.UUID, field string) (int, error) {
	n, err := pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM data_table_rows r
		WHERE r.table_id = $1 AND `+fillableSQL, tableId, field)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CountRowsToFill err: %+v", err)
	}
	return n, err
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
	var owner uuid.NullUUID
	if err := s.Scan(&v.Id, &v.TableId, &v.Name, &v.Type, &v.Config, &v.Position,
		&v.CreatedAt, &v.UpdatedAt, &deletedAt, &owner, &v.Locked); err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		v.DeletedAt = &deletedAt.Time
	}
	if owner.Valid {
		v.Owner = &owner.UUID
	}
	if strings.TrimSpace(v.Config) == "" {
		v.Config = "{}"
	}
	return &v, nil
}

const viewColumns = `id, table_id, name, type, config, position, created_at, updated_at, deleted_at, owner_id, locked`

// CreateView inserts a saved view.
func CreateView(ctx context.Context, v *View) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(v.Config) == "" {
		v.Config = "{}"
	}
	id := uuid.New()
	const q = `INSERT INTO data_table_views (id, table_id, name, type, config, position, owner_id, locked)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	var owner any
	if v.Owner != nil {
		owner = *v.Owner
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, v.TableId, v.Name, v.Type, v.Config, v.Position, owner, v.Locked)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateView err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// UpdateView updates a saved view's editable attributes, unless it's locked:
// the lock holds here, in the write, so one that lands after a caller's check
// still does. No view changed is sql.ErrNoRows.
func UpdateView(ctx context.Context, v *View) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(v.Config) == "" {
		v.Config = "{}"
	}
	const q = `UPDATE data_table_views SET name=$2, type=$3, config=$4, position=$5, updated_at=NOW()
		WHERE id=$1 AND table_id=$6 AND deleted_at IS NULL AND NOT locked`
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

// LockView locks a saved view, or unlocks it.
func LockView(ctx context.Context, tableId, viewId uuid.UUID, locked bool) error {
	n, err := pgexec.Exec(ctx, `UPDATE data_table_views SET locked=$3, updated_at=NOW()
		WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL`, viewId, tableId, locked)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/LockView err: %+v", err)
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetView is one of a table's saved views; nil when it has none by that id.
func GetView(ctx context.Context, tableId, viewId uuid.UUID) (*View, error) {
	v, found, err := pgexec.QueryOne(ctx, `SELECT `+viewColumns+` FROM data_table_views
		WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL`, func(r pgexec.Row) (*View, error) { return scanView(r) }, viewId, tableId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetView err: %+v", err)
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return v, nil
}

// DeleteView soft-deletes a saved view, unless it's locked (as UpdateView).
// No view deleted is sql.ErrNoRows.
func DeleteView(ctx context.Context, tableId, viewId uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE data_table_views SET deleted_at=NOW(), updated_at=NOW()
		WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL AND NOT locked`
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

// ListViews returns a table's saved views that reader sees, in position
// order: those shared, and their own.
func ListViews(ctx context.Context, tableId, reader uuid.UUID) ([]*View, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + viewColumns + ` FROM data_table_views
		WHERE table_id=$1 AND deleted_at IS NULL AND (owner_id IS NULL OR owner_id=$2)
		ORDER BY position ASC, created_at ASC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, tableId, reader)
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
