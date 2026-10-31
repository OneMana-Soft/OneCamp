package models

// The row ledger's store (migration 301; business/RowLedger, PROGRAM.md §4.2
// #62). Every write to a table's rows runs in one transaction with its change
// set: who made it and how, what kind of write it was, and each change it made
// (a cell's value before and after, a whole row made, deleted or brought back,
// a row moved, a relation field's links changed), stored before the write
// commits, so a write is never recorded without happening or the other way
// round. Writes of many rows are a few statements, never one per row.
//
// Undo reads a change set back and reverses only what still holds: a cell is
// put back only while it holds what the change set wrote, a row made is
// deleted only while nobody has written it since, and so on (ApplyInverse);
// what it skips it counts.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ChangeSet is one write to a table's rows (data_table_change_sets), with its
// changes (data_table_changes).
type ChangeSet struct {
	ID      uuid.UUID
	TableID uuid.UUID
	// Actor is who made it, Agent the agent acting for them; uuid.Nil for
	// none (the server on its own; a person acting themselves).
	Actor     uuid.UUID
	Agent     uuid.UUID
	ActorKind string
	Kind      string
	// Undoes is the change set this one undoes (an undo) or redoes (a redo);
	// UndoneBy the one that undid this one.
	Undoes   uuid.UUID
	UndoneBy uuid.UUID
	// Operation is the change sets a person undoes as one, such as an
	// import's (the change_set_operations migration); uuid.Nil for none.
	Operation uuid.UUID
	At        time.Time
	// Rows and Fields are counted from Changes as it's stored: how many rows
	// it changed, and which fields' cells or links.
	Rows    int
	Fields  []string
	Changes []Change
	// Append is a change set stored already (an AI column's fill, whose
	// turns write into the one change set): its Changes are added to it,
	// after those it has, and an undone one takes no more (ErrUndone).
	Append bool
}

// Change is one change a change set made: a cell's value before and after
// (JSON as stored; nil for no cell), or a whole row's, a move's or a link
// change's (Field is one of the constants below).
type Change struct {
	Row    uuid.UUID
	Field  string
	Before json.RawMessage
	After  json.RawMessage
}

// The Field of a Change that isn't one cell's.
const (
	// WholeRow is a row made, deleted or brought back (wholeRow).
	WholeRow = ""
	// FieldPosition is a row moved: its key before and after.
	FieldPosition = "#position"
	// LinksPrefix and a relation field's id is that field's links from the
	// row changed (linkChange).
	LinksPrefix = "#links:"
)

// wholeRow is a WholeRow change's Before or After: the row as it was or is.
// A delete's After is Deleted with the row's version; its Before carries the
// row's links, which went with it, so a restore can put them back.
type wholeRow struct {
	Values  json.RawMessage `json:"values,omitempty"`
	Key     string          `json:"key,omitempty"`
	RowNo   int64           `json:"row_no,omitempty"`
	Version int64           `json:"version"`
	Deleted bool            `json:"deleted,omitempty"`
	Links   []storedLink    `json:"links,omitempty"`
}

// storedLink is a link as a change keeps it.
type storedLink struct {
	Field uuid.UUID `json:"field"`
	From  uuid.UUID `json:"from"`
	To    uuid.UUID `json:"to"`
}

// linkChange is a "#links:" change's After: the links the write made and
// took away, through the field that holds them (a relation field, or the one
// a field showing its links from the other table belongs to).
type linkChange struct {
	Field  uuid.UUID    `json:"link_field"`
	Add    []storedLink `json:"add,omitempty"`
	Remove []storedLink `json:"remove,omitempty"`
}

func asJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Committing runs in a write's transaction once its change set is stored:
// the ledger's observers (business/RowLedger). An error undoes the write.
type Committing func(ctx context.Context, tx *sql.Tx, cs *ChangeSet) error

// Keys is n position keys in order, after a and before b, either of which may
// be "" for the start or the end of the list (business/Tree/position's
// Spread, which the business layer passes in).
type Keys func(a, b string, n int) ([]string, error)

// Place is where rows go: after a row, before one, at a number (the float
// position the API and the web released before keys send: after the last row
// whose number is at most it), or, with none of them, after the last row.
type Place struct {
	After, Before uuid.UUID
	Number        *float64
}

// ErrNoSuchPlace is a place next to a row that isn't one of the table's.
var ErrNoSuchPlace = errors.New("that row isn't in this table")

// ErrVersion is a write naming the version of a row it read, which has been
// written since.
var ErrVersion = errors.New("the row changed since it was read")

// RowError is why one row of a write wasn't written: sql.ErrNoRows (it isn't
// one of the table's rows, or it's deleted), ErrRowTooLarge, or ErrVersion
// with the row as it is now.
type RowError struct {
	Row     uuid.UUID
	Err     error
	Current *Row
}

// RowErrors is a write refused row by row: nothing of it was written.
type RowErrors []RowError

func (e RowErrors) Error() string {
	if len(e) == 1 {
		return e[0].Err.Error()
	}
	return fmt.Sprintf("%d rows couldn't be written", len(e))
}

// ledgerTx runs write and stores cs in one transaction, then runs committing
// in it, and commits; a write of one row has DBTimeout, of more BulkTimeout.
// A change set with no changes (a write that changed nothing) isn't stored.
func ledgerTx(ctx context.Context, rows int, cs *ChangeSet, committing Committing, write func(ctx context.Context, tx *sql.Tx) error) error {
	timeout := postgresInit.DBConn.DBTimeout
	if rows > 1 || timeout <= 0 {
		timeout = postgresInit.BulkTimeout
	}
	return postgresInit.InTx(ctx, timeout, func(ctx context.Context, tx *sql.Tx) error {
		if err := write(ctx, tx); err != nil {
			return err
		}
		if cs == nil || len(cs.Changes) == 0 {
			return nil
		}
		record := recordTx
		if cs.Append {
			record = appendTx
		}
		if err := record(ctx, tx, cs); err != nil {
			if !errors.Is(err, ErrUndone) {
				helpers.LogErrorWithContext(ctx, "models/ledger record err: %+v", err)
			}
			return err
		}
		if committing != nil {
			return committing(ctx, tx, cs)
		}
		return nil
	})
}

func nullUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// jsonText is a change's value as the changes' insert takes it: "" for none.
func jsonText(v json.RawMessage) string {
	if len(bytes.TrimSpace(v)) == 0 {
		return ""
	}
	return string(v)
}

// recordTx stores a change set and its changes in tx, in two statements,
// counting its rows and fields first.
func recordTx(ctx context.Context, tx *sql.Tx, cs *ChangeSet) error {
	if cs.ID == uuid.Nil {
		cs.ID = uuid.New()
	}
	cs.Rows, cs.Fields = rowsAndFields(cs.Changes)
	if err := tx.QueryRowContext(ctx, `INSERT INTO data_table_change_sets (id, table_id, actor, agent, actor_kind, kind, rows, fields, undoes, operation)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING at`,
		cs.ID, cs.TableID, nullUUID(cs.Actor), nullUUID(cs.Agent), cs.ActorKind, cs.Kind, cs.Rows, pq.Array(cs.Fields), nullUUID(cs.Undoes), nullUUID(cs.Operation)).Scan(&cs.At); err != nil {
		return err
	}
	ids := make([]uuid.UUID, len(cs.Changes))
	fields := make([]string, len(cs.Changes))
	before := make([]string, len(cs.Changes))
	after := make([]string, len(cs.Changes))
	for i, c := range cs.Changes {
		ids[i], fields[i], before[i], after[i] = c.Row, c.Field, jsonText(c.Before), jsonText(c.After)
	}
	// The lists go to pgx as they are, as in CellsOf.
	_, err := tx.ExecContext(ctx, `INSERT INTO data_table_changes (change_set, seq, table_id, row_id, field, before, after, at)
		SELECT $1, c.seq, $2, c.row_id, c.field, NULLIF(c.before, '')::jsonb, NULLIF(c.after, '')::jsonb, $3
		FROM unnest($4::uuid[], $5::text[], $6::text[], $7::text[]) WITH ORDINALITY AS c(row_id, field, before, after, seq)`,
		cs.ID, cs.TableID, cs.At, ids, fields, before, after)
	return err
}

// appendTx adds cs's changes to the change set cs.ID, stored already, in tx,
// after the changes it has, and counts them into its rows and fields. The
// change set is locked first, as ApplyInverse locks it, so a write and an
// undo of it take turns: one undone already takes nothing more (ErrUndone),
// and the write rolls back with it. Each write of a fill is of rows it
// hasn't written before (it walks the table once), so its rows are added
// to the count as they come.
func appendTx(ctx context.Context, tx *sql.Tx, cs *ChangeSet) error {
	ctx = pgexec.WithTx(ctx, tx)
	undone, _, err := pgexec.QueryOne(ctx, `SELECT undone_by FROM data_table_change_sets WHERE id = $1 AND table_id = $2 FOR UPDATE`,
		func(r pgexec.Row) (uuid.NullUUID, error) {
			var u uuid.NullUUID
			return u, r.Scan(&u)
		}, cs.ID, cs.TableID)
	if err != nil {
		return err
	}
	if undone.Valid {
		return ErrUndone
	}
	rows, fields := rowsAndFields(cs.Changes)
	// Its fields, in the order they first came, each once.
	at, _, err := pgexec.QueryOne(ctx, `UPDATE data_table_change_sets SET rows = rows + $2,
			fields = (SELECT COALESCE(array_agg(u.f ORDER BY u.o), '{}') FROM (
				SELECT f, MIN(o) AS o FROM unnest(fields || $3::text[]) WITH ORDINALITY AS a(f, o) GROUP BY f) u)
		WHERE id = $1 RETURNING at`, func(r pgexec.Row) (time.Time, error) {
		var t time.Time
		return t, r.Scan(&t)
	}, cs.ID, rows, pq.Array(fields))
	if err != nil {
		return err
	}
	cs.At, cs.Rows, cs.Fields = at, rows, fields // this write's, as its observers hear it
	ids := make([]uuid.UUID, len(cs.Changes))
	names := make([]string, len(cs.Changes))
	before := make([]string, len(cs.Changes))
	after := make([]string, len(cs.Changes))
	for i, c := range cs.Changes {
		ids[i], names[i], before[i], after[i] = c.Row, c.Field, jsonText(c.Before), jsonText(c.After)
	}
	_, err = pgexec.Exec(ctx, `INSERT INTO data_table_changes (change_set, seq, table_id, row_id, field, before, after, at)
		SELECT $1, (SELECT COALESCE(MAX(seq), 0) FROM data_table_changes WHERE change_set = $1) + c.seq, $2, c.row_id, c.field,
			NULLIF(c.before, '')::jsonb, NULLIF(c.after, '')::jsonb, NOW()
		FROM unnest($3::uuid[], $4::text[], $5::text[], $6::text[]) WITH ORDINALITY AS c(row_id, field, before, after, seq)`,
		cs.ID, cs.TableID, ids, names, before, after)
	return err
}

// rowsAndFields counts the rows changes touch and names the fields whose
// cells or links they change, in the order they come: a whole row made or
// deleted, or a move, names none. The list is empty, never nil: the column
// is NOT NULL, and pq.Array of a nil list is NULL, which refused every
// change set of new rows.
func rowsAndFields(changes []Change) (int, []string) {
	rows := map[uuid.UUID]bool{}
	seen := map[string]bool{}
	fields := []string{}
	for _, c := range changes {
		rows[c.Row] = true
		f := strings.TrimPrefix(c.Field, LinksPrefix)
		if f == WholeRow || f == FieldPosition || seen[f] {
			continue
		}
		seen[f] = true
		fields = append(fields, f)
	}
	return len(rows), fields
}

// cellChanges are the cells a write changed in a row, from its values before
// and after (AIKeptKey aside), in field order.
func cellChanges(row uuid.UUID, was, is string) []Change {
	var old, now map[string]json.RawMessage
	_ = json.Unmarshal([]byte(was), &old)
	_ = json.Unmarshal([]byte(is), &now)
	keys := map[string]bool{}
	for k := range old {
		keys[k] = true
	}
	for k := range now {
		keys[k] = true
	}
	delete(keys, AIKeptKey)
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []Change
	for _, k := range names {
		b, a := old[k], now[k]
		if sameJSON(b, a) {
			continue
		}
		out = append(out, Change{Row: row, Field: k, Before: b, After: a})
	}
	return out
}

// sameJSON is whether two JSON values are the same value (nil is no value).
func sameJSON(a, b json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(asJSON(x), asJSON(y))
}

// lockOrder takes a table's place lock for the rest of tx: its row in
// data_tables, FOR NO KEY UPDATE, which every write placing rows takes
// (creates and moves), so no two place rows between the same neighbours at
// once. Inserts referencing the table take only a key-share lock, which this
// leaves free. A table that's gone is sql.ErrNoRows.
func lockOrder(ctx context.Context, tx *sql.Tx, table uuid.UUID) error {
	var one int
	return tx.QueryRowContext(ctx, `SELECT 1 FROM data_tables WHERE id = $1 AND deleted_at IS NULL FOR NO KEY UPDATE`, table).Scan(&one)
}

// placeKeys is the keys rows going to at go between, in tx with the table's
// place lock held, leaving out the rows moving there. After the last row is
// after every row's key, deleted rows' too: a row restored keeps its place,
// and a new row never takes it.
func placeKeys(ctx context.Context, tx *sql.Tx, table uuid.UUID, at Place, moving []uuid.UUID) (prev, next string, err error) {
	if moving == nil {
		moving = []uuid.UUID{}
	}
	keyOf := func(row uuid.UUID) (string, error) {
		var k sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT position_key FROM data_table_rows
			WHERE id = $1 AND table_id = $2 AND deleted_at IS NULL AND NOT id = ANY($3)`, row, table, moving).Scan(&k)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !k.Valid) {
			return "", ErrNoSuchPlace
		}
		return k.String, err
	}
	var k sql.NullString
	switch {
	case at.After != uuid.Nil:
		if prev, err = keyOf(at.After); err != nil {
			return "", "", err
		}
		err = tx.QueryRowContext(ctx, `SELECT MIN(position_key) FROM data_table_rows
			WHERE table_id = $1 AND deleted_at IS NULL AND position_key > $2 AND NOT id = ANY($3)`, table, prev, moving).Scan(&k)
		return prev, k.String, err
	case at.Before != uuid.Nil:
		if next, err = keyOf(at.Before); err != nil {
			return "", "", err
		}
		err = tx.QueryRowContext(ctx, `SELECT MAX(position_key) FROM data_table_rows
			WHERE table_id = $1 AND deleted_at IS NULL AND position_key < $2 AND NOT id = ANY($3)`, table, next, moving).Scan(&k)
		return k.String, next, err
	case at.Number != nil:
		err = tx.QueryRowContext(ctx, `SELECT position_key FROM data_table_rows
			WHERE table_id = $1 AND deleted_at IS NULL AND position <= $2 AND position_key IS NOT NULL AND NOT id = ANY($3)
			ORDER BY position DESC, position_key DESC LIMIT 1`, table, *at.Number, moving).Scan(&k)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", "", err
		}
		prev = k.String
		var n sql.NullString
		if prev == "" {
			err = tx.QueryRowContext(ctx, `SELECT MIN(position_key) FROM data_table_rows
				WHERE table_id = $1 AND deleted_at IS NULL AND NOT id = ANY($2)`, table, moving).Scan(&n)
		} else {
			err = tx.QueryRowContext(ctx, `SELECT MIN(position_key) FROM data_table_rows
				WHERE table_id = $1 AND deleted_at IS NULL AND position_key > $2 AND NOT id = ANY($3)`, table, prev, moving).Scan(&n)
		}
		return prev, n.String, err
	}
	err = tx.QueryRowContext(ctx, `SELECT MAX(position_key) FROM data_table_rows WHERE table_id = $1 AND NOT id = ANY($2)`, table, moving).Scan(&k)
	return k.String, "", err
}

// NewRow is a row to make: its id (set by the caller, so links can name it
// before it's made) and its cells as stored.
type NewRow struct {
	ID     uuid.UUID
	Values string
}

// createRowsTx makes rows in a table at a place, in the order given, with the
// links given to and from them, in tx, adding a WholeRow change a row to cs.
// Each row is numbered in its table, given a key at the place and a float
// position (the place's number, or after the table's highest). by is who
// makes them (uuid.Nil for no one). It returns the rows as made, in the order
// given; a table that's gone is sql.ErrNoRows, a row that would take another
// past maxPerRow links ErrTooManyLinks.
func createRowsTx(ctx context.Context, tx *sql.Tx, table, by uuid.UUID, rows []NewRow, at Place, links []FieldLinks, maxPerRow int, keys Keys, cs *ChangeSet) ([]*Row, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	if err := lockOrder(ctx, tx, table); err != nil {
		return nil, err
	}
	if _, err := keyRowsWithoutTx(ctx, tx, table, keys); err != nil {
		return nil, err
	}
	prev, next, err := placeKeys(ctx, tx, table, at, nil)
	if err != nil {
		return nil, err
	}
	ks, err := keys(prev, next, len(rows))
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(rows))
	values := make([]string, len(rows))
	for i, r := range rows {
		if r.ID == uuid.Nil {
			r.ID = uuid.New()
		}
		ids[i], values[i] = r.ID, emptyIfBlank(r.Values)
	}
	const q = `WITH n AS (
			UPDATE data_tables SET next_row_no = next_row_no + $3 WHERE id = $2 RETURNING next_row_no - $3 AS first
		), p AS (
			SELECT COALESCE($6::double precision, (SELECT COALESCE(MAX(position), 0) + 1
				FROM data_table_rows WHERE table_id = $2 AND deleted_at IS NULL)) AS start
		)
		INSERT INTO data_table_rows (id, table_id, values, position, position_key, created_by, updated_by, row_no)
		SELECT u.id, $2, u.v::jsonb, CASE WHEN $6::double precision IS NULL THEN p.start + u.i - 1 ELSE p.start END,
			u.k, $1, $1, n.first + u.i - 1
		FROM unnest($4::uuid[], $5::text[], $7::text[]) WITH ORDINALITY AS u(id, v, k, i), n, p
		RETURNING ` + rowColumns
	rs, err := tx.QueryContext(ctx, q, nullUUID(by), table, len(rows), ids, values, at.Number, ks)
	if err != nil {
		return nil, err
	}
	made := map[uuid.UUID]*Row{}
	for rs.Next() {
		r, err := scanRow(rs)
		if err != nil {
			rs.Close()
			return nil, err
		}
		made[r.Id] = r
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	if len(made) != len(rows) {
		return nil, sql.ErrNoRows
	}
	if len(links) > 0 {
		// No one else can see the new rows to lock them, so they may come first.
		live, err := lockLinked(ctx, tx, links)
		if err != nil {
			return nil, err
		}
		if _, err := changeLinksMade(ctx, tx, links, live, maxPerRow); err != nil {
			return nil, err
		}
	}
	out := make([]*Row, len(ids))
	for i, id := range ids {
		r := made[id]
		out[i] = r
		if cs != nil {
			cs.Changes = append(cs.Changes, Change{Row: id, Field: WholeRow,
				After: asJSON(wholeRow{Values: json.RawMessage(r.Values), Key: r.PositionKey, RowNo: r.RowNo, Version: r.Version})})
		}
	}
	return out, nil
}

// CellsPatch sets the cells it names on one row (Cells, a JSON object; a null
// clears a cell). Version, when not 0, is the version the writer read: a row
// written since is ErrVersion.
type CellsPatch struct {
	Row     uuid.UUID
	Cells   string
	Version int64
}

// patchRowsSQL writes many rows' cells in one statement, each as c.old ||
// its patch, marking the AI columns among $6 whose cell it changes as a
// person's (as writeCellsSQL does a row), adding one to each row's version and
// naming who wrote it ($5). A row over $4 bytes, or whose version isn't the
// one its patch names, isn't written. Rows are locked in id order, so two
// batches never each wait for the other.
var patchRowsSQL = `WITH p AS (
		SELECT * FROM unnest($1::uuid[], $3::text[], $7::bigint[]) AS p(id, cells, want)
	), c AS (
		SELECT r.id, r.values AS old, r.version FROM data_table_rows r
		WHERE r.table_id = $2 AND r.deleted_at IS NULL AND r.id IN (SELECT id FROM p)
		ORDER BY r.id FOR UPDATE
	), n AS (
		SELECT c.id, c.old, c.version, p.want, c.old || p.cells::jsonb AS cells,
			` + fmt.Sprintf(keptSQL, "c.old") + ` AS kept
		FROM c JOIN p ON p.id = c.id
	), m AS (
		SELECT id, old, version, want, cells, kept || COALESCE((SELECT jsonb_agg(f) FROM unnest($6::text[]) f
			WHERE (cells -> f) IS DISTINCT FROM (old -> f) AND NOT kept ? f), '[]'::jsonb) AS kept
		FROM n
	)
	UPDATE data_table_rows r SET
		values = CASE WHEN jsonb_array_length(m.kept) = 0 THEN m.cells - '` + AIKeptKey + `'
			ELSE m.cells || jsonb_build_object('` + AIKeptKey + `', m.kept) END,
		version = r.version + 1, updated_by = $5, updated_at = NOW()
	FROM m
	WHERE r.id = m.id AND octet_length(m.cells::text) <= $4 AND (m.want = 0 OR m.want = m.version)
	RETURNING ` + rowColumnsOfR + `, m.old`

// patchRowsTx writes the cells each patch names into its row, in tx, adding
// one change a cell changed to cs. Only the rows it changes are written; the
// AI columns among aiFields whose cell a patch changes are marked a person's.
// A row that isn't the table's or is deleted, a row past maxBytes, or a row
// written since the version its patch names refuses the whole write:
// RowErrors, row by row. It returns the rows as written, in the order given.
// A row named twice is the caller's to refuse.
func patchRowsTx(ctx context.Context, tx *sql.Tx, table, by uuid.UUID, patches []CellsPatch, aiFields []string, maxBytes int, cs *ChangeSet) ([]*Row, error) {
	if len(patches) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, len(patches))
	cells := make([]string, len(patches))
	want := make([]int64, len(patches))
	for i, p := range patches {
		ids[i], cells[i], want[i] = p.Row, emptyIfBlank(p.Cells), p.Version
	}
	rs, err := tx.QueryContext(ctx, patchRowsSQL, ids, table, cells, maxBytes, nullUUID(by), pq.Array(aiFields), want)
	if err != nil {
		return nil, err
	}
	written := map[uuid.UUID]*Row{}
	olds := map[uuid.UUID]string{}
	for rs.Next() {
		var old string
		r, err := scanRow(thenScan{rs, []any{&old}})
		if err != nil {
			rs.Close()
			return nil, err
		}
		written[r.Id], olds[r.Id] = r, old
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	if len(written) < len(patches) {
		return nil, whyRowsNotWritten(ctx, tx, table, patches, written, maxBytes)
	}
	out := make([]*Row, len(ids))
	for i, id := range ids {
		out[i] = written[id]
		if cs != nil {
			cs.Changes = append(cs.Changes, cellChanges(id, olds[id], written[id].Values)...)
		}
	}
	return out, nil
}

// whyRowsNotWritten is why the rows of patches not among written weren't:
// RowErrors, each gone, too large, or written since its version.
func whyRowsNotWritten(ctx context.Context, tx *sql.Tx, table uuid.UUID, patches []CellsPatch, written map[uuid.UUID]*Row, maxBytes int) error {
	var out RowErrors
	for _, p := range patches {
		if written[p.Row] != nil {
			continue
		}
		r, err := scanRow(tx.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM data_table_rows
			WHERE id = $1 AND table_id = $2 AND deleted_at IS NULL`, p.Row, table))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out = append(out, RowError{Row: p.Row, Err: sql.ErrNoRows})
			continue
		case err != nil:
			return err
		}
		if p.Version != 0 && r.Version != p.Version {
			liftAIKept(r)
			out = append(out, RowError{Row: p.Row, Err: ErrVersion, Current: r})
			continue
		}
		out = append(out, RowError{Row: p.Row, Err: ErrRowTooLarge})
	}
	return out
}

// WriteRow writes one row's cells whole (wholeRowCellsSQL: a row from the web
// released before patches, or the API), and moves it to position when that's
// set and not the number it has, in one transaction with cs. It returns the
// row as written; a row that's gone is sql.ErrNoRows, one past maxBytes
// ErrRowTooLarge.
func WriteRow(ctx context.Context, table, row, by uuid.UUID, valuesJSON string, position *float64, aiFields []string, maxBytes int, keys Keys, cs *ChangeSet, committing Committing) (*Row, error) {
	if strings.TrimSpace(valuesJSON) == "" {
		valuesJSON = "{}"
	}
	var out *Row
	err := ledgerTx(ctx, 1, cs, committing, func(ctx context.Context, tx *sql.Tx) error {
		var was float64
		if err := tx.QueryRowContext(ctx, `SELECT position FROM data_table_rows WHERE id = $1 AND table_id = $2 AND deleted_at IS NULL`,
			row, table).Scan(&was); err != nil {
			return err
		}
		// A move takes the table's place lock, before the row's as creates do,
		// so a move and a create linking to the row never wait for each other.
		moves := position != nil && *position != was && keys != nil
		if moves {
			if err := lockOrder(ctx, tx, table); err != nil {
				return err
			}
		}
		var old string
		r, err := scanRow(thenScan{tx.QueryRowContext(ctx, fmt.Sprintf(writeCellsSQL, wholeRowCellsSQL),
			row, table, valuesJSON, maxBytes, position, pq.Array(aiFields), nullUUID(by)), []any{&old}})
		if errors.Is(err, sql.ErrNoRows) {
			return whyNotWrittenTx(ctx, tx, table, row)
		}
		if err != nil {
			return err
		}
		if cs != nil {
			cs.Changes = append(cs.Changes, cellChanges(row, old, r.Values)...)
		}
		// A copy of the row sends its number unchanged: only a new number moves it.
		if moves {
			moved, err := moveRows(ctx, tx, table, by, []uuid.UUID{row}, Place{Number: position}, keys, cs)
			if err != nil {
				return err
			}
			if len(moved) == 1 {
				r.PositionKey = moved[0]
				r.Version++
			}
		}
		out = r
		return nil
	})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, ErrRowTooLarge) {
			helpers.LogErrorWithContext(ctx, "models/WriteRow err: %+v", err)
		}
		return nil, err
	}
	return out, nil
}

// whyNotWrittenTx is whyNotWritten in tx.
func whyNotWrittenTx(ctx context.Context, tx *sql.Tx, table, row uuid.UUID) error {
	var live bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM data_table_rows WHERE id=$1 AND table_id=$2 AND deleted_at IS NULL)`,
		row, table).Scan(&live); err == nil && live {
		return ErrRowTooLarge
	}
	return sql.ErrNoRows
}

// takeRowLinks takes away the links to and from rows, in tx, and gives each
// row's, as a change keeps them.
func takeRowLinks(ctx context.Context, tx *sql.Tx, rows []uuid.UUID) (map[uuid.UUID][]storedLink, error) {
	rs, err := tx.QueryContext(ctx, `DELETE FROM data_table_links WHERE from_row = ANY($1) OR to_row = ANY($1)
		RETURNING field_id, from_row, to_row, created_at`, rows)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	in := map[uuid.UUID]bool{}
	for _, r := range rows {
		in[r] = true
	}
	type made struct {
		l  storedLink
		at time.Time
	}
	var all []made
	for rs.Next() {
		var m made
		if err := rs.Scan(&m.l.Field, &m.l.From, &m.l.To, &m.at); err != nil {
			return nil, err
		}
		all = append(all, m)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	// In the order they were made, so a restore makes them again in it.
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	out := map[uuid.UUID][]storedLink{}
	for _, m := range all {
		if in[m.l.From] {
			out[m.l.From] = append(out[m.l.From], m.l)
		}
		if in[m.l.To] && m.l.To != m.l.From {
			out[m.l.To] = append(out[m.l.To], m.l)
		}
	}
	return out, nil
}

// Batch is a write of a table's rows in one transaction and one change set:
// rows made (at a place, with links to and from them), cells set, rows
// deleted (softly: they go to the table's trash, keeping their cells and
// place; their links go, kept in the change set for a restore).
type Batch struct {
	Create []NewRow
	At     Place
	Links  []FieldLinks
	Patch  []CellsPatch
	// RowLinks are rows' links changed through one field each (a patch's
	// relation cells), as ChangeRowLinks changes one row's.
	RowLinks []RowLinks
	Delete   []uuid.UUID
}

// RowLinks is a change to one row's links: through Shown, the field the row
// shows them in, stored as Links.Field's.
type RowLinks struct {
	Row, Shown uuid.UUID
	Links      FieldLinks
}

// Size is how many rows the batch writes.
func (b Batch) Size() int { return len(b.Create) + len(b.Patch) + len(b.Delete) }

// BatchDone is what a batch wrote: the rows made and the rows patched, each in
// the order given, and whether a row deleted had links.
type BatchDone struct {
	Created, Patched []*Row
	Linked           bool
}

// ApplyBatch writes a batch in one transaction with cs, whose changes it adds:
// rows made first, then cells set, then rows deleted. A row that isn't one of
// the table's live rows, a row past maxBytes and a row written since the
// version its patch names refuse the whole batch (RowErrors, row by row); so
// do a table that's gone (sql.ErrNoRows), a place next to a row that isn't
// the table's (ErrNoSuchPlace) and a row that would link past maxPerRow
// (ErrTooManyLinks). Nothing of a refused batch is written. A row named twice
// is the caller's to refuse.
func ApplyBatch(ctx context.Context, table, by uuid.UUID, b Batch, aiFields []string, maxBytes, maxPerRow int, keys Keys, cs *ChangeSet, committing Committing) (*BatchDone, error) {
	done := &BatchDone{}
	err := ledgerTx(ctx, b.Size(), cs, committing, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		if done.Created, err = createRowsTx(ctx, tx, table, by, b.Create, b.At, b.Links, maxPerRow, keys, cs); err != nil {
			return err
		}
		if done.Patched, err = patchRowsTx(ctx, tx, table, by, b.Patch, aiFields, maxBytes, cs); err != nil {
			return err
		}
		for _, rl := range b.RowLinks {
			if _, _, err := changeRowLinksTx(ctx, tx, rl.Row, rl.Shown, rl.Links, maxPerRow, cs); err != nil {
				return err
			}
		}
		if len(b.Delete) == 0 {
			return nil
		}
		at := 0
		if cs != nil {
			at = len(cs.Changes)
		}
		gone, err := deleteRows(ctx, tx, table, by, b.Delete, 0, cs)
		if err != nil {
			return err
		}
		if len(gone) < len(b.Delete) {
			var out RowErrors
			for _, id := range b.Delete {
				if !gone[id] {
					out = append(out, RowError{Row: id, Err: sql.ErrNoRows})
				}
			}
			return out
		}
		if cs != nil {
			for _, c := range cs.Changes[at:] {
				var w wholeRow
				if json.Unmarshal(c.Before, &w) == nil && len(w.Links) > 0 {
					done.Linked = true
				}
			}
		}
		return nil
	})
	if err != nil {
		var re RowErrors
		if !errors.As(err, &re) && !errors.Is(err, ErrTooManyLinks) && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, ErrNoSuchPlace) {
			helpers.LogErrorWithContext(ctx, "models/ApplyBatch err: %+v", err)
		}
		return nil, err
	}
	return done, nil
}

// deleteRows deletes the live rows among ids in tx, those at version (when not
// 0) alone, with their links, adding a WholeRow change for each to cs. It
// says which it deleted.
func deleteRows(ctx context.Context, tx *sql.Tx, table, by uuid.UUID, ids []uuid.UUID, version int64, cs *ChangeSet) (map[uuid.UUID]bool, error) {
	return deleteRowsAt(ctx, tx, table, by, ids, map[uuid.UUID]int64{}, version, cs)
}

// deleteRowsAt is deleteRows with a version for each row (versions; a row not
// in it takes version, and 0 is any).
func deleteRowsAt(ctx context.Context, tx *sql.Tx, table, by uuid.UUID, ids []uuid.UUID, versions map[uuid.UUID]int64, version int64, cs *ChangeSet) (map[uuid.UUID]bool, error) {
	want := make([]int64, len(ids))
	for i, id := range ids {
		want[i] = version
		if v, ok := versions[id]; ok {
			want[i] = v
		}
	}
	// Locked in id order, as every write of many rows locks them.
	rs, err := tx.QueryContext(ctx, `WITH p AS (SELECT * FROM unnest($2::uuid[], $4::bigint[]) AS p(id, want)),
		l AS (SELECT r.id FROM data_table_rows r JOIN p ON p.id = r.id WHERE r.table_id = $1 AND r.deleted_at IS NULL
			ORDER BY r.id FOR UPDATE OF r)
		UPDATE data_table_rows r SET deleted_at = NOW(), deleted_by = $3, updated_at = NOW(), version = r.version + 1
		FROM p JOIN l ON l.id = p.id WHERE r.id = p.id AND (p.want = 0 OR r.version = p.want)
		RETURNING r.id, r.values, r.position_key, r.row_no, r.version`, table, ids, nullUUID(by), want)
	if err != nil {
		return nil, err
	}
	type was struct {
		values  string
		key     sql.NullString
		no      sql.NullInt64
		version int64
	}
	gone := map[uuid.UUID]was{}
	var order []uuid.UUID
	for rs.Next() {
		var id uuid.UUID
		var w was
		if err := rs.Scan(&id, &w.values, &w.key, &w.no, &w.version); err != nil {
			rs.Close()
			return nil, err
		}
		gone[id] = w
		order = append(order, id)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(gone))
	if len(gone) == 0 {
		return out, nil
	}
	links, err := takeRowLinks(ctx, tx, order)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		w, ok := gone[id]
		if !ok {
			continue
		}
		out[id] = true
		if cs == nil {
			continue
		}
		r := &Row{Values: w.values}
		liftAIKept(r)
		cs.Changes = append(cs.Changes, Change{Row: id, Field: WholeRow,
			Before: asJSON(wholeRow{Values: json.RawMessage(r.Values), Key: w.key.String, RowNo: w.no.Int64, Version: w.version - 1, Links: links[id]}),
			After:  asJSON(wholeRow{Deleted: true, Version: w.version})})
	}
	return out, nil
}

// lastLinks is each row's links as its latest delete took them away.
func lastLinks(ctx context.Context, tx *sql.Tx, rows []uuid.UUID) (map[uuid.UUID][]storedLink, error) {
	rs, err := tx.QueryContext(ctx, `SELECT DISTINCT ON (row_id) row_id, before FROM data_table_changes
		WHERE row_id = ANY($1) AND field = '' AND after ? 'deleted'
		ORDER BY row_id, at DESC, seq DESC`, rows)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[uuid.UUID][]storedLink{}
	for rs.Next() {
		var id uuid.UUID
		var before []byte
		if err := rs.Scan(&id, &before); err != nil {
			return nil, err
		}
		var w wholeRow
		if json.Unmarshal(before, &w) == nil {
			out[id] = w.Links
		}
	}
	return out, rs.Err()
}

// restoreRows brings back the deleted rows among ids in tx, each only at its
// version in versions when it's there (0 or absent: any), and their links
// (links: the links each had, as its delete kept them) whose other row is
// live, adding a WholeRow change for each to cs. It says which it restored.
func restoreRows(ctx context.Context, tx *sql.Tx, table, by uuid.UUID, ids []uuid.UUID, versions map[uuid.UUID]int64, links map[uuid.UUID][]storedLink, maxPerRow int, cs *ChangeSet) (map[uuid.UUID]bool, error) {
	want := make([]int64, len(ids))
	for i, id := range ids {
		want[i] = versions[id]
	}
	rs, err := tx.QueryContext(ctx, `WITH p AS (SELECT * FROM unnest($2::uuid[], $4::bigint[]) AS p(id, want)),
		l AS (SELECT r.id FROM data_table_rows r JOIN p ON p.id = r.id WHERE r.table_id = $1 AND r.deleted_at IS NOT NULL
			ORDER BY r.id FOR UPDATE OF r)
		UPDATE data_table_rows r SET deleted_at = NULL, deleted_by = NULL, updated_at = NOW(), updated_by = $3, version = r.version + 1
		FROM p JOIN l ON l.id = p.id WHERE r.id = p.id AND (p.want = 0 OR r.version = p.want)
		RETURNING r.id, r.values, r.position_key, r.row_no, r.version`, table, ids, nullUUID(by), want)
	if err != nil {
		return nil, err
	}
	type is struct {
		values  string
		key     sql.NullString
		no      sql.NullInt64
		version int64
	}
	back := map[uuid.UUID]is{}
	for rs.Next() {
		var id uuid.UUID
		var w is
		if err := rs.Scan(&id, &w.values, &w.key, &w.no, &w.version); err != nil {
			rs.Close()
			return nil, err
		}
		back[id] = w
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(back))
	if len(back) == 0 {
		return out, nil
	}
	// Their links, where the other row is live; each once, though both its
	// rows come back.
	byField := map[uuid.UUID]*FieldLinks{}
	var fields []uuid.UUID
	seen := map[storedLink]bool{}
	for id := range back {
		for _, l := range links[id] {
			if seen[l] {
				continue
			}
			seen[l] = true
			fl := byField[l.Field]
			if fl == nil {
				fl = &FieldLinks{Field: l.Field}
				byField[l.Field] = fl
				fields = append(fields, l.Field)
			}
			fl.Add = append(fl.Add, Link{From: l.From, To: l.To})
		}
	}
	sort.Slice(fields, func(i, j int) bool { return bytes.Compare(fields[i][:], fields[j][:]) < 0 })
	changes := make([]FieldLinks, 0, len(fields))
	for _, f := range fields {
		changes = append(changes, *byField[f])
	}
	made := map[storedLink]bool{}
	if len(changes) > 0 {
		live, err := lockLinked(ctx, tx, changes)
		if err != nil {
			return nil, err
		}
		added, err := changeLinksMade(ctx, tx, changes, live, maxPerRow)
		if err != nil {
			return nil, err
		}
		for _, l := range added {
			made[l] = true
		}
	}
	for _, id := range ids {
		w, ok := back[id]
		if !ok {
			continue
		}
		out[id] = true
		if cs == nil {
			continue
		}
		var kept []storedLink
		for _, l := range links[id] {
			if made[l] {
				kept = append(kept, l)
			}
		}
		r := &Row{Values: w.values}
		liftAIKept(r)
		cs.Changes = append(cs.Changes, Change{Row: id, Field: WholeRow,
			Before: asJSON(wholeRow{Deleted: true, Version: w.version - 1}),
			After:  asJSON(wholeRow{Values: json.RawMessage(r.Values), Key: w.key.String, RowNo: w.no.Int64, Version: w.version, Links: kept})})
	}
	return out, nil
}

// RestoreRows brings rows back from a table's trash, with their places and
// cells, and the links their delete took away whose other row is live, in one
// transaction with cs. A row that isn't in the table's trash refuses the
// whole restore: RowErrors.
func RestoreRows(ctx context.Context, table, by uuid.UUID, rows []uuid.UUID, maxPerRow int, cs *ChangeSet, committing Committing) error {
	if len(rows) == 0 {
		return nil
	}
	err := ledgerTx(ctx, len(rows), cs, committing, func(ctx context.Context, tx *sql.Tx) error {
		links, err := lastLinks(ctx, tx, rows)
		if err != nil {
			return err
		}
		back, err := restoreRows(ctx, tx, table, by, rows, nil, links, maxPerRow, cs)
		if err != nil {
			return err
		}
		if len(back) < len(rows) {
			var out RowErrors
			for _, id := range rows {
				if !back[id] {
					out = append(out, RowError{Row: id, Err: sql.ErrNoRows})
				}
			}
			return out
		}
		return nil
	})
	if err != nil {
		var re RowErrors
		if !errors.As(err, &re) && !errors.Is(err, ErrTooManyLinks) {
			helpers.LogErrorWithContext(ctx, "models/RestoreRows err: %+v", err)
		}
	}
	return err
}

// moveRows gives the live rows among ids keys at a place, in the order given,
// in tx (taking the table's place lock), adding a FieldPosition change for
// each to cs. It returns their new keys, in the order given.
func moveRows(ctx context.Context, tx *sql.Tx, table, by uuid.UUID, ids []uuid.UUID, at Place, keys Keys, cs *ChangeSet) ([]string, error) {
	if err := lockOrder(ctx, tx, table); err != nil {
		return nil, err
	}
	if _, err := keyRowsWithoutTx(ctx, tx, table, keys); err != nil {
		return nil, err
	}
	prev, next, err := placeKeys(ctx, tx, table, at, ids)
	if err != nil {
		return nil, err
	}
	ks, err := keys(prev, next, len(ids))
	if err != nil {
		return nil, err
	}
	rs, err := tx.QueryContext(ctx, `WITH p AS (SELECT * FROM unnest($2::uuid[], $3::text[]) AS p(id, k)),
		o AS (SELECT r.id, r.position_key AS old FROM data_table_rows r JOIN p ON p.id = r.id
			WHERE r.table_id = $1 AND r.deleted_at IS NULL ORDER BY r.id FOR UPDATE OF r)
		UPDATE data_table_rows r SET position_key = p.k, version = r.version + 1, updated_by = $4, updated_at = NOW()
		FROM p JOIN o ON o.id = p.id WHERE r.id = p.id
		RETURNING r.id, o.old`, table, ids, ks, nullUUID(by))
	if err != nil {
		return nil, err
	}
	old := map[uuid.UUID]sql.NullString{}
	for rs.Next() {
		var id uuid.UUID
		var k sql.NullString
		if err := rs.Scan(&id, &k); err != nil {
			rs.Close()
			return nil, err
		}
		old[id] = k
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	if len(old) < len(ids) {
		var out RowErrors
		for _, id := range ids {
			if _, ok := old[id]; !ok {
				out = append(out, RowError{Row: id, Err: sql.ErrNoRows})
			}
		}
		return nil, out
	}
	for i, id := range ids {
		if cs != nil {
			var before json.RawMessage
			if k := old[id]; k.Valid {
				before = asJSON(k.String)
			}
			cs.Changes = append(cs.Changes, Change{Row: id, Field: FieldPosition, Before: before, After: asJSON(ks[i])})
		}
	}
	return ks, nil
}

// MoveRows moves rows of a table to a place, in the order given, in one
// transaction with cs. A row that isn't one of the table's live rows refuses
// the whole move (RowErrors); a place next to such a row is ErrNoSuchPlace.
func MoveRows(ctx context.Context, table, by uuid.UUID, rows []uuid.UUID, at Place, keys Keys, cs *ChangeSet, committing Committing) error {
	if len(rows) == 0 {
		return nil
	}
	err := ledgerTx(ctx, len(rows), cs, committing, func(ctx context.Context, tx *sql.Tx) error {
		_, err := moveRows(ctx, tx, table, by, rows, at, keys, cs)
		return err
	})
	if err != nil {
		var re RowErrors
		if !errors.As(err, &re) && !errors.Is(err, ErrNoSuchPlace) {
			helpers.LogErrorWithContext(ctx, "models/MoveRows err: %+v", err)
		}
	}
	return err
}

// ChangeRowLinks adds and removes a relation field's links in one transaction
// with cs, whose changes it adds: one "#links:" change for row (shown, the
// field it was changed through, which may be the field showing the links
// from the other table) naming the links made and taken away. Links there
// already, and links to or from a row deleted meanwhile, aren't made. A row
// that would link to more than maxPerRow rows through the field makes it
// ErrTooManyLinks, and nothing changes. It says how many it made and removed.
func ChangeRowLinks(ctx context.Context, row uuid.UUID, shown uuid.UUID, change FieldLinks, maxPerRow int, cs *ChangeSet, committing Committing) (added, removed int, err error) {
	if len(change.Add) == 0 && len(change.Remove) == 0 {
		return 0, 0, nil
	}
	err = ledgerTx(ctx, 1, cs, committing, func(ctx context.Context, tx *sql.Tx) error {
		added, removed, err = changeRowLinksTx(ctx, tx, row, shown, change, maxPerRow, cs)
		return err
	})
	if err != nil {
		if !errors.Is(err, ErrTooManyLinks) {
			helpers.LogErrorWithContext(ctx, "models/ChangeRowLinks err: %+v", err)
		}
		return 0, 0, err
	}
	return added, removed, nil
}

// changeRowLinksTx is ChangeRowLinks's work in tx: row's links through
// shown, removed then added, with one change in cs saying both.
func changeRowLinksTx(ctx context.Context, tx *sql.Tx, row, shown uuid.UUID, change FieldLinks, maxPerRow int, cs *ChangeSet) (added, removed int, err error) {
	live, err := lockLinked(ctx, tx, []FieldLinks{change})
	if err != nil {
		return 0, 0, err
	}
	gone, err := removeLinks(ctx, tx, change)
	if err != nil {
		return 0, 0, err
	}
	made, err := changeLinksMade(ctx, tx, []FieldLinks{{Field: change.Field, Add: change.Add}}, live, maxPerRow)
	if err != nil {
		return 0, 0, err
	}
	if cs != nil && (len(made) > 0 || len(gone) > 0) {
		cs.Changes = append(cs.Changes, Change{Row: row, Field: LinksPrefix + shown.String(),
			After: asJSON(linkChange{Field: change.Field, Add: made, Remove: gone})})
	}
	return len(made), len(gone), nil
}

// removeLinks takes away a field's links that change removes, in tx, and
// says which it took.
func removeLinks(ctx context.Context, tx *sql.Tx, change FieldLinks) ([]storedLink, error) {
	if len(change.Remove) == 0 {
		return nil, nil
	}
	from, to := linkPairs(change.Remove)
	rs, err := tx.QueryContext(ctx, `DELETE FROM data_table_links l USING unnest($2::uuid[], $3::uuid[]) AS p(f, t)
		WHERE l.field_id = $1 AND l.from_row = p.f AND l.to_row = p.t RETURNING l.from_row, l.to_row`, change.Field, pq.Array(from), pq.Array(to))
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []storedLink
	for rs.Next() {
		l := storedLink{Field: change.Field}
		if err := rs.Scan(&l.From, &l.To); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rs.Err()
}

// changeLinksMade makes the links changes add between rows live says exist,
// in tx, each after those made before, and says which it made. A row that
// would link to more than maxPerRow rows through a field makes it
// ErrTooManyLinks.
func changeLinksMade(ctx context.Context, tx *sql.Tx, changes []FieldLinks, live map[uuid.UUID]bool, maxPerRow int) ([]storedLink, error) {
	var out []storedLink
	for _, c := range changes {
		var add []Link
		for _, l := range c.Add {
			if live[l.From] && live[l.To] {
				add = append(add, l)
			}
		}
		if len(add) == 0 {
			continue
		}
		from, to := linkPairs(add)
		rs, err := tx.QueryContext(ctx, `INSERT INTO data_table_links (field_id, from_row, to_row, created_at)
			SELECT $1, p.f, p.t, NOW() + p.i * INTERVAL '1 microsecond' FROM unnest($2::uuid[], $3::uuid[]) WITH ORDINALITY AS p(f, t, i)
			ON CONFLICT DO NOTHING RETURNING from_row, to_row, created_at`, c.Field, pq.Array(from), pq.Array(to))
		if err != nil {
			return nil, err
		}
		type made struct {
			l  storedLink
			at time.Time
		}
		var ms []made
		for rs.Next() {
			m := made{l: storedLink{Field: c.Field}}
			if err := rs.Scan(&m.l.From, &m.l.To, &m.at); err != nil {
				rs.Close()
				return nil, err
			}
			ms = append(ms, m)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return nil, err
		}
		sort.SliceStable(ms, func(i, j int) bool { return ms[i].at.Before(ms[j].at) })
		for _, m := range ms {
			out = append(out, m.l)
		}
		var over int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT from_row FROM data_table_links
			WHERE field_id = $1 AND from_row = ANY($2) GROUP BY from_row HAVING COUNT(*) > $3) x`,
			c.Field, pq.Array(from), maxPerRow).Scan(&over); err != nil {
			return nil, err
		}
		if over > 0 {
			return nil, ErrTooManyLinks
		}
	}
	return out, nil
}

// ───────────────────────── reading the ledger ─────────────────────────

const changeSetColumns = `id, table_id, actor, agent, actor_kind, kind, rows, fields, undoes, undone_by, at, operation`

func scanChangeSet(s scanner) (*ChangeSet, error) {
	var cs ChangeSet
	var actor, agent, undoes, undoneBy, operation uuid.NullUUID
	var fields pq.StringArray
	if err := s.Scan(&cs.ID, &cs.TableID, &actor, &agent, &cs.ActorKind, &cs.Kind, &cs.Rows, &fields, &undoes, &undoneBy, &cs.At, &operation); err != nil {
		return nil, err
	}
	cs.Actor, cs.Agent, cs.Undoes, cs.UndoneBy, cs.Operation = actor.UUID, agent.UUID, undoes.UUID, undoneBy.UUID, operation.UUID
	cs.Fields = []string(fields)
	return &cs, nil
}

// changesOf is a change set's changes, in the order it made them.
func changesOf(ctx context.Context, db queryRower, set uuid.UUID) ([]Change, error) {
	rs, err := db.QueryContext(ctx, `SELECT row_id, field, before, after FROM data_table_changes WHERE change_set = $1 ORDER BY seq`, set)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []Change
	for rs.Next() {
		var c Change
		var b, a []byte
		if err := rs.Scan(&c.Row, &c.Field, &b, &a); err != nil {
			return nil, err
		}
		if b != nil {
			c.Before = json.RawMessage(b)
		}
		if a != nil {
			c.After = json.RawMessage(a)
		}
		out = append(out, c)
	}
	return out, rs.Err()
}

// queryRower is the database, or a transaction on it.
type queryRower interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// GetChangeSet is a table's change set, with its changes; nil when it isn't
// one of the table's.
func GetChangeSet(ctx context.Context, table, id uuid.UUID) (*ChangeSet, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	cs, err := scanChangeSet(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, `SELECT `+changeSetColumns+`
		FROM data_table_change_sets WHERE id = $1 AND table_id = $2`, id, table))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetChangeSet err: %+v", err)
		return nil, err
	}
	if cs.Changes, err = changesOf(dbctx, postgresInit.DBConn.SqlDB, id); err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetChangeSet changes err: %+v", err)
		return nil, err
	}
	return cs, nil
}

// undoDepth is how many of a person's latest change sets on a table undo and
// redo look among (D-TB-5).
const undoDepth = 100

// The kinds of change set that an undo of nothing named passes over: an undo
// is redone, not undone; and actor kinds that aren't the person's own doing.
var notOnTheStack = []string{"undo"}
var notTheirOwn = []string{"automation", "system"}

// LatestToUndo is the change set a person's undo on a table undoes when it
// names none: their latest, among their last undoDepth, that isn't an undo,
// wasn't undone, and was made by them (not by an automation for them). nil
// when there's none.
func LatestToUndo(ctx context.Context, table, actor uuid.UUID) (*ChangeSet, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	cs, err := scanChangeSet(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, `SELECT `+changeSetColumns+` FROM (
			SELECT * FROM data_table_change_sets WHERE table_id = $1 AND actor = $2 AND NOT actor_kind = ANY($4)
			ORDER BY at DESC, id DESC LIMIT $3
		) s WHERE undone_by IS NULL AND NOT kind = ANY($5) ORDER BY at DESC, id DESC LIMIT 1`,
		table, actor, undoDepth, pq.Array(notTheirOwn), pq.Array(notOnTheStack)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return cs, err
}

// LatestOf is a person's newest change set on a table of any kind, made by
// them (not by an automation for them): nil when there's none.
func LatestOf(ctx context.Context, table, actor uuid.UUID) (*ChangeSet, error) {
	cs, found, err := pgexec.QueryOne(ctx, `SELECT `+changeSetColumns+` FROM data_table_change_sets
		WHERE table_id = $1 AND actor = $2 AND NOT actor_kind = ANY($3)
		ORDER BY at DESC, id DESC LIMIT 1`, func(r pgexec.Row) (*ChangeSet, error) { return scanChangeSet(r) }, table, actor, pq.Array(notTheirOwn))
	if err != nil || !found {
		return nil, err
	}
	return cs, nil
}

// OperationSets are a person's change sets of operation op on a table that
// aren't undone, newest first: what an undo or a redo of any of them
// reverses.
func OperationSets(ctx context.Context, table, actor, op uuid.UUID) ([]*ChangeSet, error) {
	return pgexec.Query(ctx, `SELECT `+changeSetColumns+` FROM data_table_change_sets
		WHERE table_id = $1 AND operation = $2 AND actor = $3 AND undone_by IS NULL
		ORDER BY at DESC, id DESC`, func(r pgexec.Row) (*ChangeSet, error) { return scanChangeSet(r) }, table, op, actor)
}

// LatestToRedo is the undo a person's redo on a table redoes: their latest
// undo, among their last undoDepth change sets, that wasn't redone and has
// nothing new of theirs on the table after it (an undo or a redo isn't new).
// nil when there's none.
func LatestToRedo(ctx context.Context, table, actor uuid.UUID) (*ChangeSet, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	cs, err := scanChangeSet(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, `WITH mine AS (
			SELECT * FROM data_table_change_sets WHERE table_id = $1 AND actor = $2 AND NOT actor_kind = ANY($4)
			ORDER BY at DESC, id DESC LIMIT $3
		), fresh AS (SELECT MAX(at) AS at FROM mine WHERE kind NOT IN ('undo', 'redo'))
		SELECT `+changeSetColumns+` FROM mine WHERE kind = 'undo' AND undone_by IS NULL
			AND at > COALESCE((SELECT at FROM fresh), '-infinity'::timestamptz)
		ORDER BY at DESC, id DESC LIMIT 1`,
		table, actor, undoDepth, pq.Array(notTheirOwn)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return cs, err
}

// ErrUndone is a change set undone already.
var ErrUndone = errors.New("that change was undone already")

// Reversal is what an undo or a redo did: how many of the change set's
// changes it reversed, and how many it left, as what they changed had
// changed again since.
type Reversal struct {
	Reversed, Skipped int
}

// ApplyInverse reverses a table's change set target in one transaction with
// cs (an undo or a redo, whose Undoes it sets), reversing only what still
// holds what target made it:
//
//   - a cell goes back to its value before only while it holds its value after;
//   - a row made is deleted, and a row brought back deleted again, only while
//     its version is the one target left it at (nobody has written it since);
//   - a row deleted comes back only while it's deleted at the version target
//     left it at, with its links whose other row is live;
//   - a row moved goes back only while it's where target put it;
//   - links made are taken away, and links taken away made again where both
//     rows are live.
//
// target is marked undone by cs. A change set undone already is ErrUndone.
func ApplyInverse(ctx context.Context, table, by uuid.UUID, target uuid.UUID, maxPerRow int, cs *ChangeSet, committing Committing) (Reversal, error) {
	var done Reversal
	cs.Undoes = target
	if cs.ID == uuid.Nil {
		cs.ID = uuid.New()
	}
	// Recorded here, not by ledgerTx: the change set is stored, changes or
	// none, before target is marked undone by it.
	err := ledgerTx(ctx, 2, nil, nil, func(ctx context.Context, tx *sql.Tx) error {
		var undone uuid.NullUUID
		if err := tx.QueryRowContext(ctx, `SELECT undone_by FROM data_table_change_sets WHERE id = $1 AND table_id = $2 FOR UPDATE`,
			target, table).Scan(&undone); err != nil {
			return err
		}
		if undone.Valid {
			return ErrUndone
		}
		changes, err := changesOf(ctx, tx, target)
		if err != nil {
			return err
		}
		var cells []Change
		var made, deleted, restored, moved []Change
		var links []Change
		for _, c := range changes {
			switch {
			case c.Field == WholeRow:
				var b, a wholeRow
				_ = json.Unmarshal(c.Before, &b)
				_ = json.Unmarshal(c.After, &a)
				switch {
				case a.Deleted:
					deleted = append(deleted, c)
				case b.Deleted:
					restored = append(restored, c)
				default:
					made = append(made, c)
				}
			case c.Field == FieldPosition:
				moved = append(moved, c)
			case strings.HasPrefix(c.Field, LinksPrefix):
				links = append(links, c)
			default:
				cells = append(cells, c)
			}
		}
		n, err := reverseCells(ctx, tx, table, by, cells, cs)
		if err != nil {
			return err
		}
		done.Reversed += n
		// Rows made, or brought back, go again while nobody has written them since.
		for _, group := range [][]Change{made, restored} {
			if len(group) == 0 {
				continue
			}
			ids := make([]uuid.UUID, len(group))
			versions := map[uuid.UUID]int64{}
			for i, c := range group {
				var a wholeRow
				_ = json.Unmarshal(c.After, &a)
				ids[i], versions[c.Row] = c.Row, a.Version
			}
			gone, err := deleteRowsAt(ctx, tx, table, by, ids, versions, 0, cs)
			if err != nil {
				return err
			}
			done.Reversed += len(gone)
		}
		if len(deleted) > 0 {
			ids := make([]uuid.UUID, len(deleted))
			versions := map[uuid.UUID]int64{}
			kept := map[uuid.UUID][]storedLink{}
			for i, c := range deleted {
				var b, a wholeRow
				_ = json.Unmarshal(c.Before, &b)
				_ = json.Unmarshal(c.After, &a)
				ids[i], versions[c.Row], kept[c.Row] = c.Row, a.Version, b.Links
			}
			back, err := restoreRows(ctx, tx, table, by, ids, versions, kept, maxPerRow, cs)
			if err != nil {
				return err
			}
			done.Reversed += len(back)
		}
		n, err = reverseMoves(ctx, tx, table, by, moved, cs)
		if err != nil {
			return err
		}
		done.Reversed += n
		n, err = reverseLinks(ctx, tx, links, maxPerRow, cs)
		if err != nil {
			return err
		}
		done.Reversed += n
		done.Skipped = len(changes) - done.Reversed
		if len(cs.Changes) == 0 {
			// Nothing held still: the change set is stored all the same, so
			// the undo says what it skipped and the next undo moves on.
			if err := recordEmpty(ctx, tx, cs); err != nil {
				return err
			}
		} else {
			if err := recordTx(ctx, tx, cs); err != nil {
				return err
			}
			if committing != nil {
				if err := committing(ctx, tx, cs); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE data_table_change_sets SET undone_by = $2 WHERE id = $1`, target, cs.ID)
		return err
	})
	if err != nil {
		if !errors.Is(err, ErrUndone) && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, ErrTooManyLinks) {
			helpers.LogErrorWithContext(ctx, "models/ApplyInverse err: %+v", err)
		}
		return Reversal{}, err
	}
	return done, nil
}

// recordEmpty stores a change set that changed nothing: an undo whose every
// change had changed again since.
func recordEmpty(ctx context.Context, tx *sql.Tx, cs *ChangeSet) error {
	if cs.ID == uuid.Nil {
		cs.ID = uuid.New()
	}
	return tx.QueryRowContext(ctx, `INSERT INTO data_table_change_sets (id, table_id, actor, agent, actor_kind, kind, rows, fields, undoes, operation)
		VALUES ($1, $2, $3, $4, $5, $6, 0, '{}', $7, $8) RETURNING at`,
		cs.ID, cs.TableID, nullUUID(cs.Actor), nullUUID(cs.Agent), cs.ActorKind, cs.Kind, nullUUID(cs.Undoes), nullUUID(cs.Operation)).Scan(&cs.At)
}

// reverseCells puts cells back to their values before changes, in one
// statement, each only while it holds its value after, adding the cells it
// changed to cs. It says how many it put back.
func reverseCells(ctx context.Context, tx *sql.Tx, table, by uuid.UUID, changes []Change, cs *ChangeSet) (int, error) {
	if len(changes) == 0 {
		return 0, nil
	}
	rows := make([]uuid.UUID, len(changes))
	fields := make([]string, len(changes))
	before := make([]string, len(changes))
	after := make([]string, len(changes))
	for i, c := range changes {
		rows[i], fields[i], before[i], after[i] = c.Row, c.Field, jsonText(c.Before), jsonText(c.After)
	}
	rs, err := tx.QueryContext(ctx, `WITH ch AS (
			SELECT * FROM unnest($2::uuid[], $3::text[], $4::text[], $5::text[]) AS ch(row_id, field, before, after)
		), c AS (
			SELECT r.id, r.values AS old FROM data_table_rows r
			WHERE r.table_id = $1 AND r.deleted_at IS NULL AND r.id IN (SELECT row_id FROM ch)
			ORDER BY r.id FOR UPDATE
		), ok AS (
			SELECT ch.* FROM ch JOIN c ON c.id = ch.row_id
			WHERE (c.old -> ch.field) IS NOT DISTINCT FROM NULLIF(ch.after, '')::jsonb
		), n AS (
			SELECT c.id, c.old,
				(c.old - COALESCE((SELECT array_agg(ok.field) FROM ok WHERE ok.row_id = c.id AND ok.before = ''), '{}'::text[]))
				|| COALESCE((SELECT jsonb_object_agg(ok.field, ok.before::jsonb) FROM ok WHERE ok.row_id = c.id AND ok.before <> ''), '{}'::jsonb) AS cells
			FROM c WHERE EXISTS (SELECT 1 FROM ok WHERE ok.row_id = c.id)
		)
		UPDATE data_table_rows r SET values = n.cells, version = r.version + 1, updated_by = $6, updated_at = NOW()
		FROM n WHERE r.id = n.id
		RETURNING r.id, n.old, r.values`, table, rows, fields, before, after, nullUUID(by))
	if err != nil {
		return 0, err
	}
	defer rs.Close()
	put := 0
	for rs.Next() {
		var id uuid.UUID
		var old, now string
		if err := rs.Scan(&id, &old, &now); err != nil {
			return 0, err
		}
		changed := cellChanges(id, old, now)
		put += len(changed)
		if cs != nil {
			cs.Changes = append(cs.Changes, changed...)
		}
	}
	return put, rs.Err()
}

// reverseMoves puts moved rows back where they were, each only while it's
// where its move put it, adding a FieldPosition change for each to cs. It
// says how many it put back.
func reverseMoves(ctx context.Context, tx *sql.Tx, table, by uuid.UUID, changes []Change, cs *ChangeSet) (int, error) {
	if len(changes) == 0 {
		return 0, nil
	}
	ids := make([]uuid.UUID, len(changes))
	was := make([]string, len(changes))
	now := make([]string, len(changes))
	for i, c := range changes {
		var b, a string
		_ = json.Unmarshal(c.Before, &b)
		_ = json.Unmarshal(c.After, &a)
		ids[i], was[i], now[i] = c.Row, b, a
	}
	rs, err := tx.QueryContext(ctx, `WITH p AS (SELECT * FROM unnest($2::uuid[], $3::text[], $4::text[]) AS p(id, was, now)),
		l AS (SELECT r.id FROM data_table_rows r JOIN p ON p.id = r.id WHERE r.table_id = $1 AND r.deleted_at IS NULL
			ORDER BY r.id FOR UPDATE OF r)
		UPDATE data_table_rows r SET position_key = NULLIF(p.was, ''), version = r.version + 1, updated_by = $5, updated_at = NOW()
		FROM p JOIN l ON l.id = p.id WHERE r.id = p.id AND r.position_key = p.now
		RETURNING r.id, p.was, p.now`, table, ids, was, now, nullUUID(by))
	if err != nil {
		return 0, err
	}
	defer rs.Close()
	put := 0
	for rs.Next() {
		var id uuid.UUID
		var b, a string
		if err := rs.Scan(&id, &b, &a); err != nil {
			return 0, err
		}
		put++
		if cs != nil {
			var to json.RawMessage
			if b != "" {
				to = asJSON(b)
			}
			cs.Changes = append(cs.Changes, Change{Row: id, Field: FieldPosition, Before: asJSON(a), After: to})
		}
	}
	return put, rs.Err()
}

// reverseLinks takes away the links changes made and makes again those they
// took away, where both rows are live, adding a "#links:" change for each it
// reverses to cs. It says how many changes it reversed.
func reverseLinks(ctx context.Context, tx *sql.Tx, changes []Change, maxPerRow int, cs *ChangeSet) (int, error) {
	put := 0
	for _, c := range changes {
		var lc linkChange
		if json.Unmarshal(c.After, &lc) != nil {
			continue
		}
		inverse := FieldLinks{Field: lc.Field}
		for _, l := range lc.Add {
			inverse.Remove = append(inverse.Remove, Link{From: l.From, To: l.To})
		}
		for _, l := range lc.Remove {
			inverse.Add = append(inverse.Add, Link{From: l.From, To: l.To})
		}
		live, err := lockLinked(ctx, tx, []FieldLinks{inverse})
		if err != nil {
			return 0, err
		}
		gone, err := removeLinks(ctx, tx, inverse)
		if err != nil {
			return 0, err
		}
		made, err := changeLinksMade(ctx, tx, []FieldLinks{{Field: inverse.Field, Add: inverse.Add}}, live, maxPerRow)
		if err != nil {
			return 0, err
		}
		if len(gone) == 0 && len(made) == 0 {
			continue
		}
		put++
		if cs != nil {
			cs.Changes = append(cs.Changes, Change{Row: c.Row, Field: c.Field,
				After: asJSON(linkChange{Field: lc.Field, Add: made, Remove: gone})})
		}
	}
	return put, nil
}

// HistoryEntry is one change set's changes to one row, as the row's history
// shows it.
type HistoryEntry struct {
	Set     *ChangeSet
	Changes []Change
}

// RowHistory is a row's change sets, newest first, with each one's changes to
// the row: limit of them, those older than the change set before (uuid.Nil:
// from the newest). more says whether there are older ones.
func RowHistory(ctx context.Context, table, row, before uuid.UUID, limit int) ([]HistoryEntry, bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	db := postgresInit.DBConn.SqlDB
	q := `SELECT ` + changeSetColumns + ` FROM data_table_change_sets s
		WHERE s.table_id = $1 AND s.id IN (SELECT change_set FROM data_table_changes WHERE row_id = $2)
		AND ($3::uuid IS NULL OR (s.at, s.id) < (SELECT at, id FROM data_table_change_sets WHERE id = $3))
		ORDER BY s.at DESC, s.id DESC LIMIT $4`
	rs, err := db.QueryContext(dbctx, q, table, row, nullUUID(before), limit+1)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RowHistory err: %+v", err)
		return nil, false, err
	}
	var sets []*ChangeSet
	for rs.Next() {
		cs, err := scanChangeSet(rs)
		if err != nil {
			rs.Close()
			return nil, false, err
		}
		sets = append(sets, cs)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, false, err
	}
	more := len(sets) > limit
	if more {
		sets = sets[:limit]
	}
	out := make([]HistoryEntry, 0, len(sets))
	if len(sets) == 0 {
		return out, more, nil
	}
	// The page's changes to the row, in one read.
	ids := make([]uuid.UUID, len(sets))
	for i, cs := range sets {
		ids[i] = cs.ID
	}
	crs, err := db.QueryContext(dbctx, `SELECT change_set, row_id, field, before, after FROM data_table_changes
		WHERE row_id = $1 AND change_set = ANY($2) ORDER BY change_set, seq`, row, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RowHistory changes err: %+v", err)
		return nil, false, err
	}
	defer crs.Close()
	bySet := map[uuid.UUID][]Change{}
	for crs.Next() {
		var set uuid.UUID
		var c Change
		var b, a []byte
		if err := crs.Scan(&set, &c.Row, &c.Field, &b, &a); err != nil {
			return nil, false, err
		}
		if b != nil {
			c.Before = json.RawMessage(b)
		}
		if a != nil {
			c.After = json.RawMessage(a)
		}
		bySet[set] = append(bySet[set], c)
	}
	if err := crs.Err(); err != nil {
		return nil, false, err
	}
	for _, cs := range sets {
		out = append(out, HistoryEntry{Set: cs, Changes: bySet[cs.ID]})
	}
	return out, more, nil
}

// ───────────────────────── the trash ─────────────────────────

// TrashedRow is a row in its table's trash.
type TrashedRow struct {
	ID        uuid.UUID
	Values    string
	RowNo     int64
	DeletedAt time.Time
	DeletedBy *uuid.UUID
}

// TrashedRows is a table's deleted rows, newest first: limit of them, those
// deleted before the row before (uuid.Nil: from the newest). more says
// whether there are older ones.
func TrashedRows(ctx context.Context, table, before uuid.UUID, limit int) ([]TrashedRow, bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, `SELECT id, values, row_no, deleted_at, deleted_by FROM data_table_rows
		WHERE table_id = $1 AND deleted_at IS NOT NULL
		AND ($2::uuid IS NULL OR (deleted_at, id) < (SELECT deleted_at, id FROM data_table_rows WHERE id = $2 AND table_id = $1))
		ORDER BY deleted_at DESC, id DESC LIMIT $3`, table, nullUUID(before), limit+1)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TrashedRows err: %+v", err)
		return nil, false, err
	}
	defer rs.Close()
	var out []TrashedRow
	for rs.Next() {
		var t TrashedRow
		var no sql.NullInt64
		var by uuid.NullUUID
		if err := rs.Scan(&t.ID, &t.Values, &no, &t.DeletedAt, &by); err != nil {
			return nil, false, err
		}
		r := &Row{Values: t.Values}
		liftAIKept(r)
		t.Values, t.RowNo = r.Values, no.Int64
		if by.Valid {
			t.DeletedBy = &by.UUID
		}
		out = append(out, t)
	}
	if err := rs.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// TrashedField is a field in its table's trash.
type TrashedField struct {
	Field     *Field
	DeletedBy *uuid.UUID
}

// TrashedFields is a table's deleted fields, newest first, at most limit.
func TrashedFields(ctx context.Context, table uuid.UUID, limit int) ([]TrashedField, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, `SELECT `+fieldColumns+`, deleted_by FROM data_table_fields
		WHERE table_id = $1 AND deleted_at IS NOT NULL ORDER BY deleted_at DESC, id DESC LIMIT $2`, table, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/TrashedFields err: %+v", err)
		return nil, err
	}
	defer rs.Close()
	var out []TrashedField
	for rs.Next() {
		var by uuid.NullUUID
		f, err := scanField(thenScan{rs, []any{&by}})
		if err != nil {
			return nil, err
		}
		t := TrashedField{Field: f}
		if by.Valid {
			t.DeletedBy = &by.UUID
		}
		out = append(out, t)
	}
	return out, rs.Err()
}

// GetTrashedField is a table's deleted field; nil when it isn't one.
func GetTrashedField(ctx context.Context, table, id uuid.UUID) (*Field, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	f, err := scanField(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, `SELECT `+fieldColumns+` FROM data_table_fields
		WHERE id = $1 AND table_id = $2 AND deleted_at IS NOT NULL`, id, table))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return f, err
}

// RestoreFields brings fields back from the trash (a relation field and the
// field showing its links from the other table, together), in one
// transaction. Their cells stayed in their rows, and a relation field's links
// stayed (DeleteFieldPair), so each comes back whole.
func RestoreFields(ctx context.Context, fields ...*Field) error {
	return postgresInit.InTx(ctx, postgresInit.DBConn.DBTimeout, func(ctx context.Context, tx *sql.Tx) error {
		for _, f := range fields {
			if err := restoreField(ctx, tx, f); err != nil {
				return err
			}
		}
		return nil
	})
}

// restoreField brings a field back from the trash on db; one that isn't in
// the trash stays as it is.
func restoreField(ctx context.Context, db execer, f *Field) error {
	if _, err := db.ExecContext(ctx, `UPDATE data_table_fields SET deleted_at = NULL, deleted_by = NULL, updated_at = NOW()
		WHERE id = $1 AND table_id = $2 AND deleted_at IS NOT NULL`, f.Id, f.TableId); err != nil {
		helpers.LogErrorWithContext(ctx, "models/RestoreFields err: %+v", err)
		return err
	}
	return nil
}

// DeletedTables is the deleted tables a person may bring back (their own, or
// every one for an admin), newest first, at most limit.
func DeletedTables(ctx context.Context, user uuid.UUID, isAdmin bool, limit int) ([]*DataTable, map[uuid.UUID]uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, `SELECT `+tableColumns+`, deleted_by FROM data_tables
		WHERE deleted_at IS NOT NULL AND ($2 OR created_by = $1) ORDER BY deleted_at DESC, id DESC LIMIT $3`, user, isAdmin, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeletedTables err: %+v", err)
		return nil, nil, err
	}
	defer rs.Close()
	var out []*DataTable
	by := map[uuid.UUID]uuid.UUID{}
	for rs.Next() {
		var who uuid.NullUUID
		t, err := scanTable(thenScan{rs, []any{&who}})
		if err != nil {
			return nil, nil, err
		}
		if who.Valid {
			by[t.Id] = who.UUID
		}
		out = append(out, t)
	}
	return out, by, rs.Err()
}

// GetDeletedTable is a deleted table; nil when there's none such.
func GetDeletedTable(ctx context.Context, id uuid.UUID) (*DataTable, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	t, err := scanTable(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, `SELECT `+tableColumns+` FROM data_tables
		WHERE id = $1 AND deleted_at IS NOT NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// RestoreTable brings a deleted table back.
func RestoreTable(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, `UPDATE data_tables SET deleted_at = NULL, deleted_by = NULL, updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NOT NULL`, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RestoreTable err: %+v", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ───────────────────────── upkeep ─────────────────────────

// KeyRowsWithout gives the rows that have no position key one after their
// table's last, table by table in the order they were made (rows made by a
// server from before the keys, between the migration and the new server
// starting), keys from keys. It says how many it keyed.
func KeyRowsWithout(ctx context.Context, keys Keys) (int, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.BulkTimeout)
	defer cancel()
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, `SELECT DISTINCT table_id FROM data_table_rows WHERE position_key IS NULL`)
	if err != nil {
		return 0, err
	}
	var tables []uuid.UUID
	for rs.Next() {
		var id uuid.UUID
		if err := rs.Scan(&id); err != nil {
			rs.Close()
			return 0, err
		}
		tables = append(tables, id)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return 0, err
	}
	keyed := 0
	for _, table := range tables {
		err := postgresInit.InTx(ctx, postgresInit.BulkTimeout, func(ctx context.Context, tx *sql.Tx) error {
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM data_tables WHERE id = $1 FOR NO KEY UPDATE`, table).Scan(&one); err != nil {
				return err
			}
			n, err := keyRowsWithoutTx(ctx, tx, table, keys)
			keyed += n
			return err
		})
		if err != nil {
			helpers.LogErrorWithContext(ctx, "models/KeyRowsWithout table %s err: %+v", table, err)
			return keyed, err
		}
	}
	return keyed, nil
}

// keyRowsWithoutTx gives a table's rows that have no position key one after
// its last, in the order they were made, in tx with the table's place lock
// held (lockOrder). Every write placing rows does it first, so a row placed
// "last" goes after rows a server from before the keys made (they sort last
// until keyed, and the start's KeyRowsWithout may not have reached them).
// The unkeyed index makes it one lookup when there are none. It says how
// many it keyed.
func keyRowsWithoutTx(ctx context.Context, tx *sql.Tx, table uuid.UUID, keys Keys) (int, error) {
	rs, err := tx.QueryContext(ctx, `SELECT id FROM data_table_rows WHERE table_id = $1 AND position_key IS NULL
		ORDER BY position, created_at, id FOR UPDATE`, table)
	if err != nil {
		return 0, err
	}
	var ids []uuid.UUID
	for rs.Next() {
		var id uuid.UUID
		if err := rs.Scan(&id); err != nil {
			rs.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rs.Close()
	if err := rs.Err(); err != nil || len(ids) == 0 {
		return 0, err
	}
	if keys == nil {
		return 0, nil
	}
	var last sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT MAX(position_key) FROM data_table_rows WHERE table_id = $1`, table).Scan(&last); err != nil {
		return 0, err
	}
	ks, err := keys(last.String, "", len(ids))
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE data_table_rows r SET position_key = p.k
		FROM unnest($1::uuid[], $2::text[]) AS p(id, k) WHERE r.id = p.id`, ids, ks); err != nil {
		return 0, err
	}
	return len(ids), SyncPlaces(pgexec.WithTx(ctx, tx), ids)
}

// PurgeTrash removes for good what has been in the trash since before
// cutoff: deleted rows (with their change log), deleted fields (with their
// links) and deleted tables. It says how many rows, fields and tables it
// removed.
func PurgeTrash(ctx context.Context, cutoff time.Time) (rows, fields, tables int64, err error) {
	err = postgresInit.InTx(ctx, postgresInit.BulkTimeout, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM data_table_changes WHERE row_id IN (
			SELECT id FROM data_table_rows WHERE deleted_at < $1)`, cutoff); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM data_table_rows WHERE deleted_at < $1`, cutoff)
		if err != nil {
			return err
		}
		rows, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `DELETE FROM data_table_links WHERE field_id IN (
			SELECT id FROM data_table_fields WHERE deleted_at < $1)`, cutoff); err != nil {
			return err
		}
		if res, err = tx.ExecContext(ctx, `DELETE FROM data_table_fields WHERE deleted_at < $1`, cutoff); err != nil {
			return err
		}
		fields, _ = res.RowsAffected()
		if res, err = tx.ExecContext(ctx, `DELETE FROM data_tables WHERE deleted_at < $1`, cutoff); err != nil {
			return err
		}
		tables, _ = res.RowsAffected()
		return nil
	})
	return rows, fields, tables, err
}

// PurgeHistory removes the change sets made before cutoff, and their
// changes. It says how many change sets it removed.
func PurgeHistory(ctx context.Context, cutoff time.Time) (int64, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.BulkTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, `DELETE FROM data_table_change_sets WHERE at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PeopleNames is what each of ids is called where a table says who changed
// or deleted something: their display name, else their username. Unknown and
// deleted people are absent.
func PeopleNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	var want []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if id != uuid.Nil && !seen[id] {
			seen[id] = true
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return out, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, `SELECT id, COALESCE(NULLIF(display_name, ''), username)
		FROM users WHERE id = ANY($1) AND deleted_at IS NULL`, want)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PeopleNames err: %+v", err)
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var id uuid.UUID
		var name sql.NullString
		if err := rs.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name.String
	}
	return out, rs.Err()
}

// What a WholeRow change did to its row.
const (
	RowCreated  = "created"
	RowDeleted  = "deleted"
	RowRestored = "restored"
)

// RowEvent is what a WholeRow change did to its row (RowCreated, RowDeleted or
// RowRestored); "" for any other change.
func (c Change) RowEvent() string {
	if c.Field != WholeRow {
		return ""
	}
	var b, a wholeRow
	_ = json.Unmarshal(c.Before, &b)
	_ = json.Unmarshal(c.After, &a)
	switch {
	case a.Deleted:
		return RowDeleted
	case b.Deleted:
		return RowRestored
	}
	return RowCreated
}

// RowsByValue is a table's live rows by the text of their value of field (a
// string as it is, a number as written) as text gives it, each text's first
// row in the table's order (rowOrder). A row without one isn't among them.
func RowsByValue(ctx context.Context, table, field uuid.UUID, text func(string) string) (map[string]uuid.UUID, error) {
	type valued struct {
		row   uuid.UUID
		value string
	}
	rows, err := pgexec.Query(ctx, `SELECT id, values->>$2 FROM data_table_rows
		WHERE table_id = $1 AND deleted_at IS NULL AND values->>$2 <> '' `+rowOrder,
		func(r pgexec.Row) (valued, error) {
			var v valued
			return v, r.Scan(&v.row, &v.value)
		}, table, field.String())
	if err != nil {
		return nil, err
	}
	out := make(map[string]uuid.UUID, len(rows))
	for _, r := range rows {
		k := text(r.value)
		if _, taken := out[k]; !taken {
			out[k] = r.row
		}
	}
	return out, nil
}

// LabelsHeld is which of labels a live row of table holds in field: a
// select's cell is its option's label, a multi-select's a list of them.
func LabelsHeld(ctx context.Context, table, field uuid.UUID, labels []string) (map[string]bool, error) {
	held, err := pgexec.Query(ctx, `SELECT DISTINCT l FROM data_table_rows r
		CROSS JOIN LATERAL jsonb_array_elements_text(CASE jsonb_typeof(r.values -> $2)
			WHEN 'array' THEN r.values -> $2 ELSE jsonb_build_array(r.values ->> $2) END) l
		WHERE r.table_id = $1 AND r.deleted_at IS NULL AND r.values -> $2 IS NOT NULL AND l = ANY($3)`,
		func(r pgexec.Row) (string, error) {
			var l string
			return l, r.Scan(&l)
		}, table, field.String(), pq.Array(labels))
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(held))
	for _, l := range held {
		out[l] = true
	}
	return out, nil
}

// ListRowsAfter is a window of a table's live rows in their order (rowOrder):
// at most limit, those after the row after with key key (uuid.Nil: from the
// first row). A row with no key yet sorts last, as rowOrder has it; key ""
// is such a row's.
func ListRowsAfter(ctx context.Context, table uuid.UUID, key string, after uuid.UUID, limit int) ([]*Row, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + rowColumns + ` FROM data_table_rows WHERE table_id = $1 AND deleted_at IS NULL `
	args := []any{table}
	switch {
	case after == uuid.Nil:
	case key != "":
		q += `AND ((position_key, id) > ($2, $3) OR position_key IS NULL) `
		args = append(args, key, after)
	default:
		q += `AND position_key IS NULL AND id > $2 `
		args = append(args, after)
	}
	q += rowOrder + fmt.Sprintf(` LIMIT %d`, limit)
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListRowsAfter err: %+v", err)
		return nil, err
	}
	defer rs.Close()
	var out []*Row
	for rs.Next() {
		r, err := scanRow(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}
