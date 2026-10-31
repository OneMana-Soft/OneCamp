package business

// The tables programme's contracts (design/tables-vs-airtable/PROGRAM.md §5):
// this package registers its writer and its ledger (tables A2: change sets,
// undo, history, the trash), its row reads, its worked-out cells and its
// access rule with the contracts the programme's packages build on, and the
// Access Resolver's "table" kind. The contracts import no business package, so
// registering from here cycles nothing.

import (
	"context"
	"fmt"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	computedStore "github.com/akashc777/OneCamp/business/ComputedStore"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	rowQuery "github.com/akashc777/OneCamp/business/RowQuery"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func init() {
	rowLedger.Rows = ledgerWriter{}
	rowLedger.Log = ledgerLog{}
	rowQuery.Rows = keyedRows{}
	computedStore.Values = computedValues{}
	tableAccess.Tables = todaysRule{}
	accessBusiness.RegisterKind(tableAccess.KindTable, tableKind{})
}

// actorOf is the Actor a contract's who is.
func actorOf(who tableAccess.Who) Actor {
	return Actor{UserID: who.User, IsAdmin: who.Admin}
}

// actorBy is the Actor a contract's By is: who, and how they act.
func actorBy(by rowLedger.By) Actor {
	a := actorOf(by.Who)
	a.Kind = by.Kind
	return a
}

// ledgerWriter writes through the row ledger: one row as CreateRow, PatchRow
// and DeleteRow write it (today's per-row events and all), more as one batch.
type ledgerWriter struct{}

func (ledgerWriter) Create(ctx context.Context, by rowLedger.By, table uuid.UUID, rows []rowLedger.RowInput, at rowLedger.Position) ([]*model.Row, error) {
	if len(rows) == 1 && at.After == nil && at.Before == nil {
		r, err := CreateRow(ctx, table, RowInput{Values: rows[0].Cells}, actorBy(by))
		if err != nil {
			return nil, err
		}
		return []*model.Row{r}, nil
	}
	in := BatchInput{Create: make([]RowInput, len(rows)), At: &PlaceInput{After: at.After, Before: at.Before}}
	for i, r := range rows {
		in.Create[i] = RowInput{Values: r.Cells}
	}
	done, err := Batch(ctx, table, in, actorBy(by))
	if err != nil {
		return nil, err
	}
	return done.Created, nil
}

func (ledgerWriter) Patch(ctx context.Context, by rowLedger.By, table uuid.UUID, patches []rowLedger.RowPatch) ([]*model.Row, error) {
	if len(patches) == 1 && patches[0].Version == 0 {
		r, err := PatchRow(ctx, table, patches[0].Row, patches[0].Cells, actorBy(by))
		if err != nil {
			return nil, err
		}
		return []*model.Row{r}, nil
	}
	in := BatchInput{Patch: make([]PatchInput, len(patches))}
	for i, p := range patches {
		in.Patch[i] = PatchInput{Row: p.Row, Values: p.Cells, Version: p.Version}
	}
	done, err := Batch(ctx, table, in, actorBy(by))
	if err != nil {
		return nil, err
	}
	return done.Updated, nil
}

func (ledgerWriter) Delete(ctx context.Context, by rowLedger.By, table uuid.UUID, rows []uuid.UUID) error {
	if len(rows) == 1 {
		return DeleteRow(ctx, table, rows[0], actorBy(by))
	}
	_, err := Batch(ctx, table, BatchInput{Delete: rows}, actorBy(by))
	return err
}

// ledgerLog reads and reverses the ledger's change sets for the contract.
type ledgerLog struct{}

func (ledgerLog) History(ctx context.Context, who tableAccess.Who, table, row uuid.UUID, page rowLedger.Page) (*rowLedger.HistoryPage, error) {
	h, err := RowHistory(ctx, table, row, page.Before, actorOf(who))
	if err != nil {
		return nil, err
	}
	out := &rowLedger.HistoryPage{Sets: make([]rowLedger.ChangeSet, 0, len(h.Items)), More: h.More}
	for _, item := range h.Items {
		cs := rowLedger.ChangeSet{ID: item.ChangeSet, Table: table, Kind: rowLedger.ChangeKind(item.Kind), At: item.At,
			By: rowLedger.By{Kind: rowLedger.ActorKind(item.ActorKind)}}
		if item.ActorID != nil {
			cs.By.User = *item.ActorID
		}
		for _, c := range item.Changes {
			cs.Changes = append(cs.Changes, rowLedger.Change{Row: row, Field: c.Field, Before: c.Before, After: c.After})
		}
		out.Sets = append(out.Sets, cs)
	}
	return out, nil
}

func (ledgerLog) Undo(ctx context.Context, by rowLedger.By, table uuid.UUID, changeSet *uuid.UUID) (*rowLedger.ChangeSet, error) {
	done, err := Undo(ctx, table, changeSet, actorBy(by))
	if err != nil {
		return nil, err
	}
	return reversed(table, by, done), nil
}

func (ledgerLog) Redo(ctx context.Context, by rowLedger.By, table uuid.UUID) (*rowLedger.ChangeSet, error) {
	done, err := Redo(ctx, table, actorBy(by))
	if err != nil {
		return nil, err
	}
	return reversed(table, by, done), nil
}

// reversed is an undo's or a redo's change set as the contract says it.
func reversed(table uuid.UUID, by rowLedger.By, done *UndoResult) *rowLedger.ChangeSet {
	undid := done.Reversed
	kind := rowLedger.ChangeUndo
	if done.Kind == string(rowLedger.ChangeUndo) {
		kind = rowLedger.ChangeRedo
	}
	return &rowLedger.ChangeSet{ID: done.ChangeSet, Table: table, By: by, Kind: kind, Undoes: &undid}
}

func (ledgerLog) Trash(ctx context.Context, who tableAccess.Who, table uuid.UUID, page rowLedger.Page) (*rowLedger.TrashPage, error) {
	trash, err := TableTrash(ctx, table, page.Before, actorOf(who))
	if err != nil {
		return nil, err
	}
	out := &rowLedger.TrashPage{More: trash.More}
	for _, r := range trash.Rows {
		out.Items = append(out.Items, rowLedger.TrashItem{Kind: "row", ID: r.ID})
	}
	for _, f := range trash.Fields {
		out.Items = append(out.Items, rowLedger.TrashItem{Kind: "field", ID: f.Field.Id})
	}
	return out, nil
}

func (ledgerLog) Restore(ctx context.Context, by rowLedger.By, table uuid.UUID, items []rowLedger.TrashItem) (*rowLedger.ChangeSet, error) {
	var in RestoreInput
	for _, it := range items {
		switch it.Kind {
		case "row":
			in.Rows = append(in.Rows, it.ID)
		case "field":
			in.Fields = append(in.Fields, it.ID)
		default:
			return nil, fmt.Errorf("the trash holds rows and fields, not %q", it.Kind)
		}
	}
	done, err := RestoreFromTrash(ctx, table, in, actorBy(by))
	if err != nil {
		return nil, err
	}
	return &rowLedger.ChangeSet{ID: done.ChangeSet, Table: table, By: by, Kind: rowLedger.ChangeRestore, Rows: len(done.Rows)}, nil
}

// keyedRows reads a table's rows in their order a window at a time, worked
// out for the reader. The cursor is the last row's place and id (keyset), so
// a window anywhere in a table reads only its own rows.
type keyedRows struct{}

// maxWindow is the most rows a window holds: what ListRows answers at most.
const maxWindow = 1000

func (keyedRows) Window(ctx context.Context, who tableAccess.Who, table uuid.UUID, from rowQuery.Cursor, n int) (*rowQuery.Window, error) {
	key, after, err := readCursor(from)
	if err != nil {
		return nil, err
	}
	if n <= 0 || n > maxWindow {
		n = maxWindow
	}
	a := actorOf(who)
	ctx = asViewer(ctx, a)
	if _, err := loadViewable(ctx, table, a); err != nil {
		return nil, err
	}
	rows, err := model.ListRowsAfter(ctx, table, key, after, n)
	if err != nil {
		return nil, errRowsUnavailable
	}
	if rows, err = readWithComputed(ctx, table, rows); err != nil {
		return nil, err
	}
	w := &rowQuery.Window{Rows: rows}
	if len(rows) == n {
		last := rows[len(rows)-1]
		w.Next = rowQuery.Cursor(last.PositionKey + "|" + last.Id.String())
	}
	return w, nil
}

// readCursor is a window's cursor read: the last row's key and id; none for
// the first window.
func readCursor(c rowQuery.Cursor) (string, uuid.UUID, error) {
	if c == "" {
		return "", uuid.Nil, nil
	}
	i := strings.LastIndexByte(string(c), '|')
	if i < 0 {
		return "", uuid.Nil, fmt.Errorf("that isn't a place in this table's rows")
	}
	id, err := uuid.Parse(string(c[i+1:]))
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("that isn't a place in this table's rows")
	}
	return string(c[:i]), id, nil
}

func (keyedRows) Count(ctx context.Context, who tableAccess.Who, table uuid.UUID) (int, error) {
	if err := CanView(ctx, table, actorOf(who)); err != nil {
		return 0, err
	}
	n, err := model.CountRows(ctx, table)
	if err != nil {
		return 0, errRowsUnavailable
	}
	return n, nil
}

// readTime works computed cells out on each read, for the reader.
type readTime struct{}

func (readTime) Read(ctx context.Context, who tableAccess.Who, fields []*model.Field, rows []*model.Row) error {
	withComputed(asViewer(ctx, actorOf(who)), fields, rows)
	return nil
}

// todaysRule is canView, canManage and the agent asker rule (opens): who may
// read a table writes its rows; an admin or its creator changes it.
type todaysRule struct{}

func (todaysRule) Resolve(ctx context.Context, who tableAccess.Who, table uuid.UUID) (tableAccess.Decision, error) {
	t, err := model.GetTableByID(ctx, table)
	if err != nil {
		return tableAccess.Decision{}, fmt.Errorf("failed to load table")
	}
	if t == nil {
		return tableAccess.Decision{}, tableAccess.ErrNotFound
	}
	a := actorOf(who)
	view := opens(ctx, t, a)
	return tableAccess.Decision{View: view, Edit: view, Manage: view && canManage(t, a)}, nil
}

// reasonAdmin is a table an admin opens because they're an admin, which
// every admin does today (PROGRAM.md §13 O6 asks whether they should).
const reasonAdmin = "admin"

// tableKind is the Access Resolver's "table": full for an admin or the
// table's creator, edit (its rows) for anyone in the workspace on a table
// shown to the workspace, none otherwise; a deleted table is none, with Was
// saying what they had.
type tableKind struct{}

func (tableKind) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	wanted := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if parsed, err := uuid.Parse(id); err == nil {
			wanted = append(wanted, parsed)
		}
	}
	tables, err := model.TablesByIDs(ctx, wanted)
	if err != nil {
		return nil, err
	}
	user, _ := uuid.Parse(who.UserUUID)
	a := Actor{UserID: user, IsAdmin: who.Admin}
	out := make(map[string]accessBusiness.Decision, len(tables))
	for _, t := range tables {
		out[t.Id.String()] = tableDecision(t, a)
	}
	return out, nil
}

// tableDecision is today's rule for one table, as the Access Resolver says
// it. Pure.
func tableDecision(t *model.DataTable, a Actor) accessBusiness.Decision {
	var d accessBusiness.Decision
	switch {
	case a.UserID != uuid.Nil && t.CreatedBy == a.UserID:
		d.Level, d.Owner, d.Reasons = accessBusiness.LevelFull, true, []string{"owner"}
	case a.IsAdmin:
		d.Level, d.Reasons = accessBusiness.LevelFull, []string{reasonAdmin}
	case t.Visibility == model.VisibilityWorkspace:
		d.Level, d.Reasons = accessBusiness.LevelEdit, []string{"workspace"}
	}
	d.Was = d.Level
	if t.DeletedAt != nil {
		d.Deleted, d.Level, d.Reasons = true, accessBusiness.LevelNone, nil
	}
	d.Unlocked = d.Level
	return d
}
