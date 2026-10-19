package business

// The row ledger's writes and reads beyond one row (tables A2, PROGRAM.md
// §4.2 #62): batches of up to maxBatch rows in one change set, moving rows,
// undo and redo, a row's history, and the trash for rows, fields and tables.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: rowLedger.SweepTrashPurge, Every: time.Hour, Run: func(ctx context.Context) error {
		return trashPurge(ctx, policyBusiness.Policies.Int(ctx, policyBusiness.TablesTrashRetentionDays), time.Now())
	}})
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: rowLedger.SweepHistoryPurge, Every: time.Hour, Run: func(ctx context.Context) error {
		return historyPurge(ctx, policyBusiness.Policies.Int(ctx, policyBusiness.TablesHistoryRetentionDays), time.Now())
	}})
}

var (
	// errNothingToUndo and errNothingToRedo are an undo or a redo with
	// nothing of the person's on the table left to reverse.
	errNothingToUndo = fmt.Errorf("Nothing to undo")
	errNothingToRedo = fmt.Errorf("Nothing to redo")
	// errNotYours is an undo naming someone else's change.
	errNotYours = fmt.Errorf("You can undo only your own changes")
	// errFieldUndo is an undo of the cells a field's change rewrote.
	errFieldUndo = fmt.Errorf("This came from changing a field: change the field back to undo it")
	// errUndoneAlready is an undo of a change undone already.
	errUndoneAlready = fmt.Errorf("That change was undone already")
	// errVersion is a write naming the version of a row it read, which has
	// been written since.
	errVersion = fmt.Errorf("This row changed since you read it")
)

// IsConflict is an error a controller answers 409: nothing to undo or redo,
// a change undone already, a row changed since it was read.
func IsConflict(err error) bool {
	var re *RowConflict
	return err == errNothingToUndo || err == errNothingToRedo || err == errUndoneAlready || errors.As(err, &re)
}

// RowConflict is a write refused because a row changed since the version it
// names: the row as it is now, for the writer to show.
type RowConflict struct {
	Row *model.Row
}

func (e *RowConflict) Error() string { return errVersion.Error() }

// BatchError is one row's part of a refused batch: the create's place in the
// list (Index) or the row (Row), the field, and why.
type BatchError struct {
	Index *int      `json:"index,omitempty"`
	Row   uuid.UUID `json:"row,omitempty"`
	Field string    `json:"field,omitempty"`
	Msg   string    `json:"msg"`
}

// BatchErrors is a batch refused, row by row: nothing of it was written.
type BatchErrors []BatchError

func (e BatchErrors) Error() string {
	if len(e) == 1 {
		return e[0].Msg
	}
	return fmt.Sprintf("%d rows can't be written", len(e))
}

// PlaceInput is where rows go: after a row, or before one; with neither,
// after the table's last row.
type PlaceInput struct {
	After  *uuid.UUID `json:"after,omitempty"`
	Before *uuid.UUID `json:"before,omitempty"`
}

func (p *PlaceInput) place() (model.Place, error) {
	var out model.Place
	if p == nil {
		return out, nil
	}
	if p.After != nil && p.Before != nil {
		return out, fmt.Errorf("Put rows after one row or before one, not both")
	}
	if p.After != nil {
		out.After = *p.After
	}
	if p.Before != nil {
		out.Before = *p.Before
	}
	return out, nil
}

// PatchInput sets the cells it names on one row (a nil clears one). Version,
// when set, is the version the writer read: a row written since is refused
// with the row as it is.
type PatchInput struct {
	Row     uuid.UUID              `json:"row"`
	Values  map[string]interface{} `json:"values"`
	Version int64                  `json:"version,omitempty"`
}

// BatchInput is a batch of writes to a table's rows: rows to make (at a
// place), cells to set, rows to delete, all in one change set. Kind says what
// the writer did, for the history ("paste", "fill", "import", "duplicate");
// left out, it's worked out from what the batch does.
type BatchInput struct {
	Create []RowInput   `json:"create"`
	At     *PlaceInput  `json:"at,omitempty"`
	Patch  []PatchInput `json:"patch"`
	Delete []uuid.UUID  `json:"delete"`
	Kind   string       `json:"kind,omitempty"`
}

// BatchResult is what a batch wrote: its change set, the rows made and
// changed (each as the writer reads it), and the rows deleted.
type BatchResult struct {
	ChangeSet uuid.UUID    `json:"change_set,omitempty"`
	Created   []*model.Row `json:"created"`
	Updated   []*model.Row `json:"updated"`
	Deleted   []uuid.UUID  `json:"deleted"`
}

// batchKind is a batch's change kind: one a writer may name, or what the
// batch does.
func batchKind(in BatchInput) rowLedger.ChangeKind {
	switch rowLedger.ChangeKind(in.Kind) {
	case rowLedger.ChangePaste, rowLedger.ChangeFill, rowLedger.ChangeImport, rowLedger.ChangeEdit:
		return rowLedger.ChangeKind(in.Kind)
	}
	switch {
	case len(in.Create) > 0 && len(in.Patch) == 0 && len(in.Delete) == 0:
		return rowLedger.ChangeCreate
	case len(in.Delete) > 0 && len(in.Create) == 0 && len(in.Patch) == 0:
		return rowLedger.ChangeDelete
	}
	return rowLedger.ChangeEdit
}

// Batch writes a batch of a table's rows for actor (who may write its rows):
// at most maxBatch rows in all, each named once, in one transaction and one
// change set, with one live message and one table.rows.changed webhook (in
// place of one a row). Every cell is checked as a single write checks it; a
// bad one refuses the whole batch, with every row's errors (BatchErrors). A
// row changed since the version its patch names refuses it too (RowConflict).
func Batch(ctx context.Context, tableId uuid.UUID, in BatchInput, actor Actor) (*BatchResult, error) {
	ctx = asViewer(ctx, actor)
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	size := len(in.Create) + len(in.Patch) + len(in.Delete)
	if size == 0 {
		return nil, fmt.Errorf("name the rows to make, change or delete")
	}
	if size > maxBatch {
		return nil, fmt.Errorf("A batch writes at most %d rows: this one names %d", maxBatch, size)
	}
	at, err := in.At.place()
	if err != nil {
		return nil, err
	}
	named := map[uuid.UUID]bool{}
	for _, p := range in.Patch {
		if named[p.Row] {
			return nil, fmt.Errorf("A batch names each row once: %s is named twice", p.Row)
		}
		named[p.Row] = true
	}
	for _, id := range in.Delete {
		if named[id] {
			return nil, fmt.Errorf("A batch names each row once: %s is named twice", id)
		}
		named[id] = true
	}
	fields, err := model.ListFields(ctx, tableId)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	var problems BatchErrors
	refuse := func(index *int, row uuid.UUID, err error) {
		var ce fieldTypes.CellErrors
		if errors.As(err, &ce) {
			for _, c := range ce {
				problems = append(problems, BatchError{Index: index, Row: row, Field: c.Field, Msg: c.Msg})
			}
			return
		}
		problems = append(problems, BatchError{Index: index, Row: row, Msg: err.Error()})
	}
	var b model.Batch
	b.At, b.Delete = at, in.Delete
	var linkedTables []uuid.UUID
	for i, r := range in.Create {
		values := r.Values
		if values == nil {
			values = map[string]interface{}{}
		}
		id := uuid.New()
		links, tables, err := takeLinks(ctx, actor, fields, values, id)
		if err != nil {
			i := i
			refuse(&i, uuid.Nil, err)
			continue
		}
		cells, err := storedCells(fields, values, fieldTypes.Input{New: true, Complete: completeWrite(actor)})
		if err != nil {
			i := i
			refuse(&i, uuid.Nil, err)
			continue
		}
		b.Create = append(b.Create, model.NewRow{ID: id, Values: cells})
		b.Links = append(b.Links, links...)
		linkedTables = append(linkedTables, tables...)
	}
	for _, p := range in.Patch {
		values := p.Values
		if values == nil {
			values = map[string]interface{}{}
		}
		cells, err := storedCells(fields, values, fieldTypes.Input{})
		switch {
		case err != nil:
			refuse(nil, p.Row, err)
			continue
		case len(values) == 0:
			refuse(nil, p.Row, errNoCells)
			continue
		}
		b.Patch = append(b.Patch, model.CellsPatch{Row: p.Row, Cells: cells, Version: p.Version})
	}
	if len(problems) > 0 {
		return nil, problems
	}
	cs := newChangeSet(ctx, tableId, actor, batchKind(in))
	done, err := model.ApplyBatch(ctx, tableId, actor.UserID, b, aiColumns(fields), maxValuesBytes, maxLinks, keys, cs, observe)
	if err != nil {
		return nil, batchErr(err)
	}
	out := &BatchResult{ChangeSet: cs.ID, Created: done.Created, Updated: done.Patched, Deleted: in.Delete}
	if out.Deleted == nil {
		out.Deleted = []uuid.UUID{}
	}
	stored := make([]model.Row, 0, len(done.Created)+len(done.Patched))
	for _, r := range append(append([]*model.Row{}, done.Created...), done.Patched...) {
		stored = append(stored, *r)
	}
	withComputed(ctx, fields, append(append([]*model.Row{}, done.Created...), done.Patched...))
	announce(ctx, t, cs, liveAndHooks)
	tellTables(linkedTables...)
	if done.Linked || len(b.Patch) > 0 {
		tellLinkedLater(ctx, tableId)
	}
	// Continuous AI autofill, row by row as a single write's, one after
	// another rather than all at once.
	later := context.WithoutCancel(ctx)
	helpers.GoSafeNamed("tables.batch.autofill", func() {
		for i := range stored {
			r := stored[i]
			if written := changedIn(cs, r.Id); len(written) > 0 {
				AutofillRowOnWrite(later, t, &r, written, actor)
			}
		}
	})
	return out, nil
}

// tellLinkedLater has the readers of the tables linking with table look
// again, off the request.
func tellLinkedLater(ctx context.Context, table uuid.UUID) {
	later := context.WithoutCancel(ctx)
	helpers.GoSafeNamed("tables.linked.tell", func() { tellTablesLinkedWith(later, table) })
}

// batchErr is what a refused write says: its rows' errors, a conflict with
// the row as it is, or why.
func batchErr(err error) error {
	var re model.RowErrors
	switch {
	case errors.As(err, &re):
		var out BatchErrors
		for _, e := range re {
			switch {
			case errors.Is(e.Err, model.ErrVersion):
				if e.Current != nil {
					return &RowConflict{Row: e.Current}
				}
				out = append(out, BatchError{Row: e.Row, Msg: errVersion.Error()})
			case errors.Is(e.Err, model.ErrRowTooLarge):
				out = append(out, BatchError{Row: e.Row, Msg: "row is too large"})
			default:
				out = append(out, BatchError{Row: e.Row, Msg: errRowNotFound.Error()})
			}
		}
		return out
	case errors.Is(err, model.ErrTooManyLinks):
		return errTooManyLinks
	case errors.Is(err, model.ErrNoSuchPlace):
		return fmt.Errorf("That row isn't in this table, so rows can't go next to it")
	case errors.Is(err, sql.ErrNoRows):
		return errNotFound
	}
	return fmt.Errorf("failed to write the rows")
}

// MoveRows moves rows of a table (actor may write its rows) to a place, in
// the order given, in one change set: only their places change.
func MoveRows(ctx context.Context, tableId uuid.UUID, rows []uuid.UUID, to PlaceInput, actor Actor) (uuid.UUID, error) {
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return uuid.Nil, err
	}
	if len(rows) == 0 || len(rows) > maxBatch {
		return uuid.Nil, fmt.Errorf("Move between 1 and %d rows at a time", maxBatch)
	}
	at, err := to.place()
	if err != nil {
		return uuid.Nil, err
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range rows {
		if seen[id] {
			return uuid.Nil, fmt.Errorf("A move names each row once: %s is named twice", id)
		}
		seen[id] = true
		if (at.After != uuid.Nil && at.After == id) || (at.Before != uuid.Nil && at.Before == id) {
			return uuid.Nil, fmt.Errorf("A row can't go next to itself")
		}
	}
	cs := newChangeSet(ctx, tableId, actor, rowLedger.ChangeMove)
	if err := model.MoveRows(ctx, tableId, actor.UserID, rows, at, keys, cs, observe); err != nil {
		return uuid.Nil, batchErr(err)
	}
	announce(ctx, t, cs, liveAndHooks)
	return cs.ID, nil
}

// UndoResult is what an undo or a redo did: its change set, the change set it
// reversed and what kind that was, and how many changes it reversed and left
// as they were, as they had changed again since.
type UndoResult struct {
	ChangeSet uuid.UUID `json:"change_set"`
	Reversed  uuid.UUID `json:"reversed"`
	Kind      string    `json:"kind"`
	Changes   int       `json:"changes"`
	Skipped   int       `json:"skipped"`
}

// Undo reverses one of actor's own change sets on a table: the one named, or
// their latest (among their last 100) that isn't undone. Only what still
// holds the change is put back (model.ApplyInverse); what changed again since
// is left, and counted. An undo is itself a change set, which Redo reverses;
// naming an undo is a redo.
func Undo(ctx context.Context, tableId uuid.UUID, changeSet *uuid.UUID, actor Actor) (*UndoResult, error) {
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	var target *model.ChangeSet
	if changeSet != nil {
		if target, err = model.GetChangeSet(ctx, tableId, *changeSet); err != nil {
			return nil, fmt.Errorf("failed to read the change")
		}
		if target == nil {
			return nil, errNotFound
		}
		if target.Actor != actor.UserID {
			return nil, errNotYours
		}
	} else if target, err = model.LatestToUndo(ctx, tableId, actor.UserID); err != nil {
		return nil, fmt.Errorf("failed to read your changes")
	}
	if target == nil {
		return nil, errNothingToUndo
	}
	kind := rowLedger.ChangeUndo
	if target.Kind == string(rowLedger.ChangeUndo) {
		kind = rowLedger.ChangeRedo
	}
	return reverse(ctx, t, target, kind, actor)
}

// Redo reverses actor's latest undo on a table, when they've changed nothing
// on it since: the change it undid comes back where it still can.
func Redo(ctx context.Context, tableId uuid.UUID, actor Actor) (*UndoResult, error) {
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	target, err := model.LatestToRedo(ctx, tableId, actor.UserID)
	if err != nil {
		return nil, fmt.Errorf("failed to read your changes")
	}
	if target == nil {
		return nil, errNothingToRedo
	}
	return reverse(ctx, t, target, rowLedger.ChangeRedo, actor)
}

// reverse undoes target as a change set of kind (undo or redo).
func reverse(ctx context.Context, t *model.DataTable, target *model.ChangeSet, kind rowLedger.ChangeKind, actor Actor) (*UndoResult, error) {
	if target.Kind == string(rowLedger.ChangeField) {
		return nil, errFieldUndo
	}
	cs := newChangeSet(ctx, t.Id, actor, kind)
	done, err := model.ApplyInverse(ctx, t.Id, actor.UserID, target.ID, maxLinks, cs, observe)
	switch {
	case errors.Is(err, model.ErrUndone):
		return nil, errUndoneAlready
	case errors.Is(err, model.ErrTooManyLinks):
		return nil, errTooManyLinks
	case errors.Is(err, sql.ErrNoRows):
		return nil, errNotFound
	case err != nil:
		return nil, fmt.Errorf("failed to undo that")
	}
	announce(ctx, t, cs, liveAndHooks)
	tellLinkedLater(ctx, t.Id)
	return &UndoResult{ChangeSet: cs.ID, Reversed: target.ID, Kind: target.Kind, Changes: done.Reversed, Skipped: done.Skipped}, nil
}

// HistoryChange is one change a change set made to a row: a cell's value
// before and after (as stored: options by label, links to items as {id,
// type}, never an item's title), or the row made, deleted, brought back or
// moved (Event).
type HistoryChange struct {
	Field  string          `json:"field,omitempty"`
	Event  string          `json:"event,omitempty"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

// HistoryItem is one change set in a row's history: who, how, when, and what
// it changed in the row.
type HistoryItem struct {
	ChangeSet uuid.UUID       `json:"change_set"`
	Kind      string          `json:"kind"`
	ActorKind string          `json:"actor_kind"`
	ActorID   *uuid.UUID      `json:"actor_id,omitempty"`
	ActorName string          `json:"actor_name,omitempty"`
	At        time.Time       `json:"at"`
	Undone    bool            `json:"undone,omitempty"`
	Changes   []HistoryChange `json:"changes"`
}

// History is a page of a row's history, newest first.
type History struct {
	Items []HistoryItem `json:"items"`
	More  bool          `json:"more"`
}

// historyPage is how many change sets a page of a row's history holds.
const historyPage = 50

// historyReads is how many pages of change sets a read of a row's history
// goes through for one that shows something: a page whose every change was
// to fields in the trash shows nothing.
const historyReads = 20

// RowHistory is a row's history, newest first, a page at a time (before: the
// last change set of the page before), for someone who may read the table.
// Moves and links are named by event; a link's other rows by id. A field in
// the trash is read by nothing until it's brought back: its changes, to its
// cells or its links, which wait with it (D14), are left out, and so is a
// change set that changed nothing else in the row.
func RowHistory(ctx context.Context, tableId, rowId uuid.UUID, before *uuid.UUID, actor Actor) (*History, error) {
	if _, err := loadViewable(ctx, tableId, actor); err != nil {
		return nil, err
	}
	fields, err := model.ListFields(ctx, tableId)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	live := make(map[string]bool, len(fields))
	for _, f := range fields {
		live[f.Id.String()] = true
	}
	var from uuid.UUID
	if before != nil {
		from = *before
	}
	var entries []model.HistoryEntry
	more := false
	for i := 0; i < historyReads && len(entries) == 0; i++ {
		page, pageMore, err := model.RowHistory(ctx, tableId, rowId, from, historyPage)
		if err != nil {
			return nil, fmt.Errorf("failed to read the row's history")
		}
		more = pageMore
		for _, e := range page {
			if e.Changes = liveChanges(e.Changes, live); len(e.Changes) > 0 {
				entries = append(entries, e)
			}
		}
		if !more || len(page) == 0 {
			break
		}
		from = page[len(page)-1].Set.ID
	}
	var people []uuid.UUID
	for _, e := range entries {
		people = append(people, e.Set.Actor)
	}
	names, _ := model.PeopleNames(ctx, people)
	out := &History{Items: make([]HistoryItem, 0, len(entries)), More: more}
	for _, e := range entries {
		item := HistoryItem{ChangeSet: e.Set.ID, Kind: e.Set.Kind, ActorKind: e.Set.ActorKind, At: e.Set.At,
			Undone: e.Set.UndoneBy != uuid.Nil, Changes: make([]HistoryChange, 0, len(e.Changes))}
		if e.Set.Actor != uuid.Nil {
			a := e.Set.Actor
			item.ActorID, item.ActorName = &a, names[a]
		}
		for _, c := range e.Changes {
			item.Changes = append(item.Changes, historyChange(c))
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// liveChanges is changes without those to fields not in live: a cell's or a
// relation's links. A whole row's and a move stay. Pure.
func liveChanges(changes []model.Change, live map[string]bool) []model.Change {
	out := changes[:0:0]
	for _, c := range changes {
		f := strings.TrimPrefix(c.Field, model.LinksPrefix)
		if f == model.WholeRow || f == model.FieldPosition || live[f] {
			out = append(out, c)
		}
	}
	return out
}

// historyChange is a change as a row's history shows it. Pure.
func historyChange(c model.Change) HistoryChange {
	switch {
	case c.Field == model.WholeRow:
		return HistoryChange{Event: c.RowEvent()}
	case c.Field == model.FieldPosition:
		return HistoryChange{Event: "moved"}
	case len(c.Field) > len(model.LinksPrefix) && c.Field[:len(model.LinksPrefix)] == model.LinksPrefix:
		var lc struct {
			Add    []struct{ From, To uuid.UUID } `json:"add"`
			Remove []struct{ From, To uuid.UUID } `json:"remove"`
		}
		_ = json.Unmarshal(c.After, &lc)
		return HistoryChange{Field: c.Field[len(model.LinksPrefix):], Event: "links",
			After: asRaw(map[string]int{"linked": len(lc.Add), "unlinked": len(lc.Remove)})}
	}
	return HistoryChange{Field: c.Field, Before: c.Before, After: c.After}
}

func asRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// TrashRow is a row in its table's trash: its cells as it was deleted, who
// deleted it, and when.
type TrashRow struct {
	ID            uuid.UUID       `json:"id"`
	Values        json.RawMessage `json:"values"`
	RowNo         int64           `json:"row_no,omitempty"`
	DeletedAt     time.Time       `json:"deleted_at"`
	DeletedBy     *uuid.UUID      `json:"deleted_by,omitempty"`
	DeletedByName string          `json:"deleted_by_name,omitempty"`
}

// TrashField is a field in its table's trash.
type TrashField struct {
	Field         *model.Field `json:"field"`
	DeletedBy     *uuid.UUID   `json:"deleted_by,omitempty"`
	DeletedByName string       `json:"deleted_by_name,omitempty"`
}

// Trash is a table's trash: its deleted rows, newest first, a page at a
// time; and its deleted fields, for someone who can change it.
type Trash struct {
	Rows   []TrashRow   `json:"rows"`
	More   bool         `json:"more"`
	Fields []TrashField `json:"fields"`
}

// trashPage is how many rows a page of a table's trash holds.
const trashPage = 100

// TableTrash is a table's trash for actor, who may read it: its deleted rows
// (anyone who may write its rows may bring one back), and its deleted fields
// when actor can change the table.
func TableTrash(ctx context.Context, tableId uuid.UUID, before *uuid.UUID, actor Actor) (*Trash, error) {
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	var from uuid.UUID
	if before != nil {
		from = *before
	}
	rows, more, err := model.TrashedRows(ctx, tableId, from, trashPage)
	if err != nil {
		return nil, errRowsUnavailable
	}
	// A deleted row shows the cells of the fields that are live, as every read
	// does (keepCells): a field in the trash keeps its cells for its restore,
	// and nothing reads them meanwhile.
	liveFields, err := model.ListFields(ctx, tableId)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	live := make(map[string]bool, len(liveFields))
	for _, f := range liveFields {
		live[f.Id.String()] = true
	}
	out := &Trash{Rows: make([]TrashRow, 0, len(rows)), More: more, Fields: []TrashField{}}
	var people []uuid.UUID
	for _, r := range rows {
		if r.DeletedBy != nil {
			people = append(people, *r.DeletedBy)
		}
	}
	var fields []model.TrashedField
	if canManage(t, actor) {
		if fields, err = model.TrashedFields(ctx, tableId, trashPage); err != nil {
			return nil, fmt.Errorf("failed to read the trash")
		}
		for _, f := range fields {
			if f.DeletedBy != nil {
				people = append(people, *f.DeletedBy)
			}
		}
	}
	names, _ := model.PeopleNames(ctx, people)
	for _, r := range rows {
		item := TrashRow{ID: r.ID, Values: json.RawMessage(keepCells(r.Values, live)), RowNo: r.RowNo, DeletedAt: r.DeletedAt, DeletedBy: r.DeletedBy}
		if r.DeletedBy != nil {
			item.DeletedByName = names[*r.DeletedBy]
		}
		out.Rows = append(out.Rows, item)
	}
	for _, f := range fields {
		item := TrashField{Field: f.Field, DeletedBy: f.DeletedBy}
		if f.DeletedBy != nil {
			item.DeletedByName = names[*f.DeletedBy]
		}
		out.Fields = append(out.Fields, item)
	}
	return out, nil
}

// RestoreInput names what to bring back from a table's trash.
type RestoreInput struct {
	Rows   []uuid.UUID `json:"rows"`
	Fields []uuid.UUID `json:"fields"`
}

// RestoreResult is what a restore brought back.
type RestoreResult struct {
	ChangeSet uuid.UUID   `json:"change_set,omitempty"`
	Rows      []uuid.UUID `json:"rows"`
	Fields    []uuid.UUID `json:"fields"`
}

// RestoreFromTrash brings rows back from a table's trash (actor may write its
// rows), with their cells, places and the links their delete took whose other
// row is live, in one change set; and fields (actor can change the table),
// with their cells and links, which never left.
func RestoreFromTrash(ctx context.Context, tableId uuid.UUID, in RestoreInput, actor Actor) (*RestoreResult, error) {
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	if len(in.Rows) == 0 && len(in.Fields) == 0 {
		return nil, fmt.Errorf("name the rows or fields to bring back")
	}
	if len(in.Rows) > maxBatch {
		return nil, fmt.Errorf("Bring back at most %d rows at a time", maxBatch)
	}
	if len(in.Fields) > 0 && !canManage(t, actor) {
		return nil, errForbidden
	}
	out := &RestoreResult{Rows: []uuid.UUID{}, Fields: []uuid.UUID{}}
	for _, id := range in.Fields {
		if err := restoreField(ctx, t, id, actor); err != nil {
			return nil, err
		}
		out.Fields = append(out.Fields, id)
	}
	if len(in.Fields) > 0 {
		tellFieldsChanged(tableId)
	}
	if len(in.Rows) > 0 {
		cs := newChangeSet(ctx, tableId, actor, rowLedger.ChangeRestore)
		if err := model.RestoreRows(ctx, tableId, actor.UserID, in.Rows, maxLinks, cs, observe); err != nil {
			return nil, batchErr(err)
		}
		out.ChangeSet, out.Rows = cs.ID, in.Rows
		announce(ctx, t, cs, liveAndHooks)
		tellLinkedLater(ctx, tableId)
	}
	return out, nil
}

// restoreField brings a field of t back from the trash: with the field showing
// its links from the other table when it's a relation field that has one, or,
// for the field showing another's links, only while that one is there, which
// shows its links again.
func restoreField(ctx context.Context, t *model.DataTable, id uuid.UUID, actor Actor) error {
	f, err := model.GetTrashedField(ctx, t.Id, id)
	if err != nil {
		return fmt.Errorf("failed to read the trash")
	}
	if f == nil {
		return fmt.Errorf("That field isn't in this table's trash")
	}
	both := []*model.Field{f}
	if l, ok := linkOf(f); ok {
		switch {
		case l.inverseOf != "":
			owner, err := liveField(ctx, l.table, l.inverseOf)
			if err != nil {
				return err
			}
			if owner == nil {
				return fmt.Errorf("Bring back the field %q shows links of first", f.Name)
			}
			if ol, ok := linkOf(owner); ok && ol.inverse == "" {
				var cfg map[string]interface{}
				_ = json.Unmarshal([]byte(owner.Config), &cfg)
				cfg["inverse"] = f.Id.String()
				b, _ := json.Marshal(cfg)
				owner.Config = string(b)
				if err := model.UpdateField(ctx, owner); err != nil {
					return fmt.Errorf("failed to bring the field back")
				}
			}
		case l.inverse != "":
			if other, err := uuid.Parse(l.inverse); err == nil {
				if g, err := model.GetTrashedField(ctx, l.table, other); err == nil && g != nil {
					both = append(both, g)
				}
			}
		}
	}
	if err := model.RestoreFields(ctx, both...); err != nil {
		return fmt.Errorf("failed to bring the field back")
	}
	for _, g := range both[1:] {
		tellFieldsChanged(g.TableId)
	}
	return nil
}

// liveField is a table's field that isn't deleted, by id; nil when there's
// none such.
func liveField(ctx context.Context, table uuid.UUID, id string) (*model.Field, error) {
	fields, err := model.ListFields(ctx, table)
	if err != nil {
		return nil, fmt.Errorf("failed to load fields")
	}
	for _, f := range fields {
		if f.Id.String() == id {
			return f, nil
		}
	}
	return nil, nil
}

// TrashTable is a deleted table, as the tables' trash lists it.
type TrashTable struct {
	Table         *model.DataTable `json:"table"`
	DeletedBy     *uuid.UUID       `json:"deleted_by,omitempty"`
	DeletedByName string           `json:"deleted_by_name,omitempty"`
}

// TablesTrash is the deleted tables actor may bring back: their own, or every
// one for an admin, newest first.
func TablesTrash(ctx context.Context, actor Actor) ([]TrashTable, error) {
	tables, by, err := model.DeletedTables(ctx, actor.UserID, actor.IsAdmin, trashPage)
	if err != nil {
		return nil, fmt.Errorf("failed to read the trash")
	}
	var people []uuid.UUID
	for _, id := range by {
		people = append(people, id)
	}
	names, _ := model.PeopleNames(ctx, people)
	out := make([]TrashTable, 0, len(tables))
	for _, t := range tables {
		item := TrashTable{Table: t}
		if who, ok := by[t.Id]; ok {
			w := who
			item.DeletedBy, item.DeletedByName = &w, names[who]
		}
		out = append(out, item)
	}
	return out, nil
}

// RestoreTable brings a deleted table back, for its creator or an admin.
func RestoreTable(ctx context.Context, id uuid.UUID, actor Actor) (*model.DataTable, error) {
	t, err := model.GetDeletedTable(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to read the trash")
	}
	if t == nil {
		return nil, errNotFound
	}
	if !canManage(t, actor) {
		return nil, errForbidden
	}
	if err := model.RestoreTable(ctx, id); err != nil {
		return nil, errNotFound
	}
	t.DeletedAt = nil
	tellLinkedLater(ctx, id)
	return t, nil
}
