package business

// The row ledger (tables A2, design/tables-vs-airtable/PROGRAM.md §4.2 #62):
// every write to a table's rows is a change set, stored with the write in its
// transaction (models/postgres/DataTable ledger.go), so each row has a
// history, a person can undo and redo what they did, and deleted rows, fields
// and tables wait in the trash with who deleted them.
//
// This file holds what every write shares: the change set it starts, the
// ledger's observers run in its transaction, and what readers, webhooks and
// agents are told once it's stored (announce). Batches, undo, history and the
// trash are in ledgerRows.go.

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	position "github.com/akashc777/OneCamp/business/Tree/position"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// keys places rows: business/Tree/position's fractional keys, which sort as
// strings, so a row goes between two others, or moves, rewriting its key
// alone.
var keys model.Keys = position.Spread

// maxBatch is the most rows one batch writes (PROGRAM.md A2): a paste, a bulk
// delete, a duplicate.
const maxBatch = 1000

// rowsTold is the most row ids one live message names: past it, open views
// read the table again.
const rowsTold = 500

// newChangeSet starts a change set of kind for actor's write to table: who
// (the person; the agent bound to their API token, if any) and how.
func newChangeSet(ctx context.Context, table uuid.UUID, actor Actor, kind rowLedger.ChangeKind) *model.ChangeSet {
	how := actor.Kind
	if how == "" {
		how = rowLedger.ActorPerson
	}
	cs := &model.ChangeSet{TableID: table, Actor: actor.UserID, ActorKind: string(how), Kind: string(kind)}
	if agent, err := uuid.Parse(helpers.GetApiAgentID(ctx)); err == nil {
		cs.Agent = agent
	}
	return cs
}

// observe runs the ledger's observers in a write's transaction, once its
// change set is stored (business/RowLedger Observers): an error undoes the
// write.
func observe(ctx context.Context, tx *sql.Tx, cs *model.ChangeSet) error {
	obs := rowLedger.Observers.All()
	if len(obs) == 0 {
		return nil
	}
	public := publicSet(cs)
	for _, o := range obs {
		if o.Committing == nil {
			continue
		}
		if err := o.Committing(ctx, tx, public); err != nil {
			return err
		}
	}
	return nil
}

// publicSet is a change set as business/RowLedger says it.
func publicSet(cs *model.ChangeSet) *rowLedger.ChangeSet {
	out := &rowLedger.ChangeSet{
		ID: cs.ID, Table: cs.TableID, Kind: rowLedger.ChangeKind(cs.Kind), At: cs.At,
		Rows: cs.Rows, Fields: cs.Fields,
		By: rowLedger.By{Kind: rowLedger.ActorKind(cs.ActorKind)},
	}
	out.By.User, out.By.Agent = cs.Actor, cs.Agent
	if cs.Undoes != uuid.Nil {
		u := cs.Undoes
		out.Undoes = &u
	}
	out.Changes = make([]rowLedger.Change, len(cs.Changes))
	for i, c := range cs.Changes {
		out.Changes[i] = rowLedger.Change{Row: c.Row, Field: c.Field, Before: c.Before, After: c.After}
	}
	return out
}

// What a change set's write already tells, as today's single-row writes do,
// and so what announce adds.
type telling int

const (
	// liveAndHooks: announce sends the live message and the webhook.
	liveAndHooks telling = iota
	// hooksOnly: the write sends its own live message (a single row's, as
	// today, or "fields" or "links"), and announce the table.rows.changed webhook.
	hooksOnly
	// neither: an AI fill, which sends its own live message and which no
	// webhook hears, so an agent's reply can't set off another fill.
	neither
)

// announce is what follows a stored change set: the ledger's observers that
// listen after commit, the Resource Event, one live message for the table's
// open views (unless the write sends its own), and one table.rows.changed
// webhook naming every row and what happened to it (unless it's an AI fill),
// in place of one event a row. Nothing for a change set that changed nothing.
func announce(ctx context.Context, t *model.DataTable, cs *model.ChangeSet, how telling) {
	if cs == nil || len(cs.Changes) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if obs := rowLedger.Observers.All(); len(obs) > 0 {
		public := publicSet(cs)
		for _, o := range obs {
			if o.Committed != nil {
				o.Committed(ctx, public)
			}
		}
	}
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind: resourceEventBusiness.TableRowsChanged, ResourceKind: "table", ResourceUUID: t.Id.String(),
		Actor: resourceEventBusiness.Actor{UserUUID: actorUUID(cs)}, At: cs.At,
		Data: map[string]string{
			"change_set_id": cs.ID.String(),
			"change":        cs.Kind,
			"rows":          strconv.Itoa(cs.Rows),
			"fields":        strings.Join(cs.Fields, ","),
		},
	})
	if how == liveAndHooks {
		mqttBusiness.PublishTableRow(t.Id.String(), liveChanged(cs))
	}
	if how != neither {
		payload := rowsChanged(t, cs)
		helpers.GoSafeNamed("tables.rows.changed", func() { webhookBusiness.DispatchEvent(ctx, "table.rows.changed", payload) })
	}
}

func actorUUID(cs *model.ChangeSet) string {
	if cs.Actor == uuid.Nil {
		return ""
	}
	return cs.Actor.String()
}

// rowsByEvent is a change set's rows by what happened to them: made,
// deleted, brought back, or otherwise changed (cells, place or links), each
// once, in the order the change set made them; and each changed row's
// fields.
func rowsByEvent(cs *model.ChangeSet) (created, updated, deleted, restored []uuid.UUID, fields map[uuid.UUID][]string) {
	whole := map[uuid.UUID]string{}
	for _, c := range cs.Changes {
		if e := c.RowEvent(); e != "" {
			whole[c.Row] = e
		}
	}
	seen := map[uuid.UUID]bool{}
	fields = map[uuid.UUID][]string{}
	seenField := map[uuid.UUID]map[string]bool{}
	for _, c := range cs.Changes {
		if f := strings.TrimPrefix(c.Field, model.LinksPrefix); f != model.WholeRow && f != model.FieldPosition {
			if seenField[c.Row] == nil {
				seenField[c.Row] = map[string]bool{}
			}
			if !seenField[c.Row][f] {
				seenField[c.Row][f] = true
				fields[c.Row] = append(fields[c.Row], f)
			}
		}
		if seen[c.Row] {
			continue
		}
		seen[c.Row] = true
		switch whole[c.Row] {
		case model.RowCreated:
			created = append(created, c.Row)
		case model.RowDeleted:
			deleted = append(deleted, c.Row)
		case model.RowRestored:
			restored = append(restored, c.Row)
		default:
			updated = append(updated, c.Row)
		}
	}
	return created, updated, deleted, restored, fields
}

// rowsChanged is the table.rows.changed webhook's payload (docs/Webhooks.md):
// the change set, what kind of write it was, and every row it touched by what
// happened to it, with the fields it changed row by row, so a consumer needs
// no read per row. Keys naming ids end in _id or _ids, which a webhook sent
// only ids keeps (business/Webhook outgoingPrivacy.go); no cell's value is
// sent, as no row event sends one. A field's change (an option renamed, a
// type changed) may change every row: it names at most maxBatch, and
// rows_changed says how many it changed. Pure.
func rowsChanged(t *model.DataTable, cs *model.ChangeSet) map[string]interface{} {
	created, updated, deleted, restored, fields := rowsByEvent(cs)
	total := len(created) + len(updated) + len(deleted) + len(restored)
	ids := func(rows []uuid.UUID) []string {
		out := make([]string, 0, len(rows))
		for _, id := range rows {
			out = append(out, id.String())
		}
		return out
	}
	if room := max(0, maxBatch-len(created)-len(deleted)-len(restored)); len(updated) > room {
		updated = updated[:room]
	}
	rowFields := map[string][]string{}
	for _, list := range [][]uuid.UUID{created, updated, restored} {
		for _, id := range list {
			if f := fields[id]; len(f) > 0 {
				rowFields[id.String()] = f
			}
		}
	}
	fieldIDs := cs.Fields
	if fieldIDs == nil {
		fieldIDs = []string{}
	}
	return map[string]interface{}{
		"table_id":         t.Id.String(),
		"table_name":       t.Name,
		"change_set_id":    cs.ID.String(),
		"change":           cs.Kind,
		"rows_changed":     total,
		"created_row_ids":  ids(created),
		"updated_row_ids":  ids(updated),
		"deleted_row_ids":  ids(deleted),
		"restored_row_ids": ids(restored),
		"field_ids":        fieldIDs,
		"row_field_ids":    rowFields,
	}
}

// liveChanged is the one live message a change set of many rows sends on its
// table's topic: the rows by what happened to them, at most rowsTold in all
// (more: open views read the table again), and whether rows moved (moved:
// they read it again too, as rows changed places). Pure.
func liveChanged(cs *model.ChangeSet) map[string]interface{} {
	created, updated, deleted, restored, _ := rowsByEvent(cs)
	left := rowsTold
	take := func(rows []uuid.UUID) []string {
		out := []string{}
		for _, id := range rows {
			if left == 0 {
				break
			}
			out = append(out, id.String())
			left--
		}
		return out
	}
	msg := map[string]interface{}{
		"action":     "changed",
		"change_set": cs.ID.String(),
		"created":    take(created),
		"updated":    take(updated),
		"deleted":    take(deleted),
		"restored":   take(restored),
	}
	if len(created)+len(updated)+len(deleted)+len(restored) > rowsTold {
		msg["more"] = true
	}
	// Rows moved: their places changed, which open views read again whole.
	for _, c := range cs.Changes {
		if c.Field == model.FieldPosition {
			msg["moved"] = true
			break
		}
	}
	return msg
}

// trashPurge and historyPurge are the ledger's sweeps (business/RowLedger
// SweepTrashPurge, SweepHistoryPurge): removing what has been in the trash,
// and changes kept, past their policies' days. Both are 0, keep, until the
// owner decides (PROGRAM.md §13 O3), so each runs and removes nothing.
func trashPurge(ctx context.Context, days int, now time.Time) error {
	if days <= 0 {
		return nil
	}
	rows, fields, tables, err := model.PurgeTrash(ctx, now.AddDate(0, 0, -days))
	if err == nil && rows+fields+tables > 0 {
		helpers.LogInfoWithContext(ctx, "tables.trash.purge: removed %d rows, %d fields and %d tables", rows, fields, tables)
	}
	return err
}

func historyPurge(ctx context.Context, days int, now time.Time) error {
	if days <= 0 {
		return nil
	}
	n, err := model.PurgeHistory(ctx, now.AddDate(0, 0, -days))
	if err == nil && n > 0 {
		helpers.LogInfoWithContext(ctx, "tables.history.purge: removed %d change sets", n)
	}
	return err
}

// changedIn is the fields whose cells a change set changed: what a row's AI
// columns that read them refill for.
func changedIn(cs *model.ChangeSet, row uuid.UUID) map[string]bool {
	out := map[string]bool{}
	for _, c := range cs.Changes {
		if c.Row == row && c.Field != model.WholeRow && c.Field != model.FieldPosition && !strings.HasPrefix(c.Field, model.LinksPrefix) {
			out[c.Field] = true
		}
	}
	return out
}
