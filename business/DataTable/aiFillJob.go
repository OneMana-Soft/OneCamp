package business

// An AI fill's background turns (audit 31): a fill writes in the request
// what the AI's rate allows, and leaves the rest to an outbox event, whose
// sink writes on at the rate's pace and queues its next turn while there's
// more. Running as the person who asked, read as they are at each turn, it
// stops once they can no longer authorise work (principalBusiness.Assess:
// deactivated, say), can't read the table, the column is no longer an AI
// column, or AI is off for them. The turns of one column's fill take turns
// (the event's key), so two turns never ask for a cell at once; a fill's
// request can still ask for cells a turn is asking for, and then the later
// answer isn't written (ErrCellChanged), though it was asked for and paid.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

const (
	aiFillTopic = "tables"
	aiFillKind  = "ai_fill"
	aiFillSink  = "tables.ai_fill"
	// aiFillTurn is how long one background turn writes before it queues
	// the next, inside the outbox's minute for a sink.
	aiFillTurn = 45 * time.Second
	// queueRestFor bounds the write of a fill's rest to the outbox.
	queueRestFor = 10 * time.Second
)

// Seams for tests: enqueueFill writes a turn to the outbox, ready, and
// delivers it; fillPerson and fillIsAdmin read the person a turn runs as, as
// they are now: their graph user, with what principalBusiness.Assess reads
// (deleted_at, is_bot, is_external), and whether they're an admin.
var (
	enqueueFill = outboxBusiness.Enqueue
	fillPerson  = userDomain.GetDgraphUserInfoByUUID
	fillIsAdmin = userDomain.IsActiveAdmin
)

// errAIFillWaits is a turn the AI's rate stopped before it asked anything:
// the outbox tries it again after its backoff.
var errAIFillWaits = errors.New("the AI's rate allows no more calls yet")

func init() {
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: aiFillTopic, Name: aiFillSink, Run: runAIFill})
}

// aiFillJob is the rest of a fill: a column's fill (fill, migration 509),
// which says how far it has got; or, for a row's autofill, its auto columns
// in fields and the change set it writes into (into); by is who asked. A
// turn queued by a server from before fills took the whole column has no
// fill, and takes up to budget of the column's empty cells, as it did; a
// row's from before autofills had a change set has no into, and each of its
// writes is one of its own.
type aiFillJob struct {
	table, field, row, fill, into uuid.UUID
	fields                        []uuid.UUID
	budget                        int
	by                            Actor
}

// queueRest leaves j, the rest of a fill, to the background, on a context
// of its own: a request, or a turn's minute for the outbox's sink, may have
// run out during the fill's last call, and a turn that couldn't queue its
// next would run again with its old budget.
func queueRest(ctx context.Context, j aiFillJob) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queueRestFor)
	defer cancel()
	return enqueueFill(ctx, aiFillEntry(j))
}

// aiFillEntry is j as the outbox keeps it.
func aiFillEntry(j aiFillJob) outboxBusiness.Entry {
	e := outboxBusiness.Entry{Topic: aiFillTopic, Kind: aiFillKind, Actor: j.by.UserID.String(),
		Entities: map[string]string{"table": j.table.String()}}
	if j.row != uuid.Nil {
		ids := make([]string, len(j.fields))
		for i, f := range j.fields {
			ids[i] = f.String()
		}
		e.Key = "ai_fill:" + j.table.String() + ":row:" + j.row.String()
		e.Entities["row"], e.Entities["fields"] = j.row.String(), strings.Join(ids, ",")
		if j.into != uuid.Nil {
			e.Entities["change_set"] = j.into.String()
		}
	} else {
		e.Key = "ai_fill:" + j.table.String() + ":" + j.field.String()
		e.Entities["field"], e.Entities["budget"] = j.field.String(), strconv.Itoa(j.budget)
		if j.fill != uuid.Nil {
			e.Entities["fill"] = j.fill.String()
		}
	}
	return e
}

// jobOf reads an event's job back; false for one it can't read, which is
// done, as there's nothing to do. Whether its person is an admin is read when
// it runs, never kept in the event.
func jobOf(ev outboxBusiness.Event) (aiFillJob, bool) {
	var j aiFillJob
	var err error
	if j.by.UserID, err = uuid.Parse(ev.Actor); err != nil {
		return j, false
	}
	if j.table, err = uuid.Parse(ev.Entities["table"]); err != nil {
		return j, false
	}
	if row := ev.Entities["row"]; row != "" {
		if j.row, err = uuid.Parse(row); err != nil {
			return j, false
		}
		for _, s := range strings.Split(ev.Entities["fields"], ",") {
			f, err := uuid.Parse(s)
			if err != nil {
				return j, false
			}
			j.fields = append(j.fields, f)
		}
		if cs := ev.Entities["change_set"]; cs != "" {
			if j.into, err = uuid.Parse(cs); err != nil {
				return j, false
			}
		}
		return j, true
	}
	if j.field, err = uuid.Parse(ev.Entities["field"]); err != nil {
		return j, false
	}
	if fill := ev.Entities["fill"]; fill != "" {
		if j.fill, err = uuid.Parse(fill); err != nil {
			return j, false
		}
	}
	j.budget, err = strconv.Atoi(ev.Entities["budget"])
	return j, err == nil && j.budget > 0
}

// runAIFill is one background turn of a fill (the outbox's sink).
func runAIFill(ctx context.Context, ev outboxBusiness.Event) error {
	j, ok := jobOf(ev)
	if !ok {
		return nil
	}
	// A fill is work its person authorised: each turn asks whether they still
	// may, and one who can't, or whom the graph no longer has, ends it, as a
	// table they can no longer read does.
	person, err := fillPerson(ctx, j.by.UserID.String())
	switch {
	case errors.Is(err, helpers.ErrNotFound):
		person = nil // no one to authorise it: Assess refuses, as for someone deactivated
	case err != nil:
		return err // the graph didn't answer: the outbox tries the turn again
	}
	if may := principalBusiness.Assess(person); !may.Allowed {
		helpers.LogWarnWithContext(ctx, "tables: the AI fill of table %s stops: %s", j.table, may.Reason)
		return j.stop(ctx)
	}
	j.by.IsAdmin = fillIsAdmin(ctx, &j.by.UserID)
	ctx = asViewer(ctx, j.by)
	t, err := loadViewable(ctx, j.table, j.by)
	if errors.Is(err, errNotFound) || errors.Is(err, errForbidden) {
		return j.stop(ctx) // gone, or no longer theirs to read
	}
	if err != nil {
		return err
	}
	fields, err := model.ListFields(ctx, j.table)
	if err != nil {
		return err
	}
	f, ok := fillerFor(ctx, j.by.UserID.String())
	if !ok {
		return j.stop(ctx)
	}
	if j.row != uuid.Nil {
		return fillRowTurn(ctx, f, t, fields, j)
	}
	target := aiColumn(fields, j.field)
	prompt := ""
	if target != nil {
		prompt = aiPromptFromConfig(target.Config)
	}
	if prompt == "" {
		return j.stop(ctx)
	}
	if j.fill != uuid.Nil {
		return fillTurn(ctx, f, t, fields, target, prompt, j)
	}
	rows, err := model.RowsToFill(ctx, j.table, j.field.String(), j.budget)
	if err != nil {
		return err
	}
	asked, _ := fillCells(ctx, f, t, fields, cellsOfColumn(rows, target, prompt), false, time.Now().Add(aiFillTurn), nil)
	switch {
	case len(rows) == 0:
		return nil
	case asked == 0:
		return errAIFillWaits
	case asked < len(rows):
		j.budget -= asked
		return queueRest(ctx, j)
	}
	return nil
}

// fillRowTurn refills the auto columns a row's write left to the background:
// each still an auto AI column the row's writer hasn't since written, into
// the autofill's change set. One undone since ends it.
func fillRowTurn(ctx context.Context, f filler, t *model.DataTable, fields []*model.Field, j aiFillJob) error {
	rw, err := model.GetRowByID(ctx, j.table, j.row)
	if err != nil {
		return err
	}
	if rw == nil {
		return nil
	}
	var due []*model.Field
	for _, id := range j.fields {
		if target := aiColumn(fields, id); target != nil {
			if prompt, auto := aiConfigFromConfig(target.Config); prompt != "" && auto {
				due = append(due, target)
			}
		}
	}
	cells := cellsOfRow(rw, due)
	run := &fillRun{into: j.into}
	asked, _ := fillCells(ctx, f, t, fields, cells, false, time.Now().Add(aiFillTurn), run)
	switch {
	case asked == len(cells) || run.ended():
		return nil
	case asked == 0:
		return errAIFillWaits
	}
	j.fields = columnsOf(cells[asked:])
	return queueRest(ctx, j)
}

// stop ends a column's fill that can't go on (its person may no longer
// authorise it, the table or the column is gone, AI is off for them), so
// the grid stops showing it as running; a job with no fill has nothing to
// stop. It is done either way.
func (j aiFillJob) stop(ctx context.Context) error {
	if j.fill == uuid.Nil {
		return nil
	}
	if f, err := model.StopAIFill(ctx, j.table, j.fill, uuid.Nil); err == nil && f != nil {
		broadcastFill(j.table.String(), f)
	}
	return nil
}

// fillTurn is one background turn of a column's fill: from the row the fill
// reached, it reads a page of the rows still to fill and writes for
// aiFillTurn, as the AI's rate allows, then moves the fill on and queues its
// next turn while it runs. A fill stopped, undone or done ends.
func fillTurn(ctx context.Context, f filler, t *model.DataTable, fields []*model.Field, target *model.Field, prompt string, j aiFillJob) error {
	fill, err := model.GetAIFill(ctx, j.table, j.fill)
	if err != nil {
		return err
	}
	if fill == nil || fill.State != model.AIFillRunning || fill.ChangeSet == nil {
		return nil
	}
	rows, err := model.RowsToFillAfter(ctx, j.table, target.Id.String(), fill.CursorKey, fill.CursorID, min(fill.Left(), aiFillPage))
	if err != nil {
		return err
	}
	run := &fillRun{into: *fill.ChangeSet, fill: fill.ID, table: t.Id}
	asked, filled := fillCells(ctx, f, t, fields, cellsOfColumn(rows, target, prompt), false, time.Now().Add(aiFillTurn), run)
	if len(rows) > 0 && asked == 0 && !run.ended() {
		return errAIFillWaits
	}
	fill = advanceFill(ctx, t, fill, rows, asked, filled, len(rows) < aiFillPage, run)
	if fill.State != model.AIFillRunning {
		return nil
	}
	j.budget = fill.Left()
	return queueRest(ctx, j)
}
