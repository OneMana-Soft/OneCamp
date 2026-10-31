package business

// Per-row AI autofill (Notion "AI Autofill"): a column whose cell values are
// produced by an AI prompt over each row. The column is an ordinary field whose
// config carries {"ai":{"prompt":"..."}}; FillAIColumnWith evaluates that prompt
// against each row's other column values via the shared AI chokepoint (per-user
// model, circuit breaker, rate limit, token budget — same residency rules as
// every other AI call) and writes the cell. Reuses the Data Table model +
// permission model; no parallel AI runtime.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	// maxAIFillRows is how many rows "Fill again" takes.
	maxAIFillRows = 200
	// LegacyAIFillCells is how many cells a fill takes for a web from before
	// fills took the whole column.
	LegacyAIFillCells = maxAIFillRows
	// maxAIFillCells is how many cells one fill of a column takes: every
	// empty one, up to this.
	maxAIFillCells = 10000
	// aiFillConfirmOver is how many cells a fill takes before it asks its
	// person first: each is one call to their model.
	aiFillConfirmOver = 500
	// aiFillPage is how many rows one turn of a fill reads.
	aiFillPage = 100
	// aiFillRequestFor is how long a fill's request writes before it leaves
	// the rest to the background.
	aiFillRequestFor   = 20 * time.Second
	maxAICellChars     = 600
	aiCellPromptBudget = 2000
)

// FillResult reports the outcome of an AI-column fill.
type FillResult struct {
	Filled  int `json:"filled"`
	Skipped int `json:"skipped"`
	// Left is how many of the fill's cells are still to be written: they're
	// filled in the background, as the AI's rate allows.
	Left int `json:"left"`
	// Total is how many cells the fill takes, at most maxAIFillCells.
	Total int `json:"total"`
	// More is how many empty cells the column has past those: the next fill
	// takes them.
	More int `json:"more"`
	// NeedsConfirm is a fill of more than aiFillConfirmOver cells that its
	// person hasn't confirmed: nothing was asked or written. Total says how
	// many cells it would take.
	NeedsConfirm bool `json:"needs_confirm,omitempty"`
	// Fill is the column's fill: this one, or the one that was running
	// already. Its progress comes over the table's live topic.
	Fill *model.AIFill `json:"fill,omitempty"`
	// ChangeSet is the change set the fill's writes are, which its person's
	// undo names to undo the whole fill.
	ChangeSet *uuid.UUID `json:"change_set,omitempty"`
}

// aiConfigFromConfig extracts an AI column's prompt and its continuous-autofill
// flag from a field's config JSON ({"ai":{"prompt":"...","auto":true}}). auto
// opts the column into recompute-on-write (Notion-style continuous autofill);
// when false the column is on-demand/batch only. Returns ("", false) when the
// field is not an AI column. Pure.
func aiConfigFromConfig(config string) (prompt string, auto bool) {
	if strings.TrimSpace(config) == "" {
		return "", false
	}
	var c struct {
		AI struct {
			Prompt string `json:"prompt"`
			Auto   bool   `json:"auto"`
		} `json:"ai"`
	}
	if err := json.Unmarshal([]byte(config), &c); err != nil {
		return "", false
	}
	return strings.TrimSpace(c.AI.Prompt), c.AI.Auto
}

// aiPromptFromConfig extracts an AI column's prompt from a field's config JSON
// ({"ai":{"prompt":"..."}}). Returns "" when the field is not an AI column.
// Pure.
func aiPromptFromConfig(config string) string {
	prompt, _ := aiConfigFromConfig(config)
	return prompt
}

// renderCellValue turns a stored cell value into a short plain string for the
// prompt context. Pure.
func renderCellValue(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case bool:
		if val {
			return "yes"
		}
		return "no"
	case float64:
		// JSON numbers; render integers cleanly.
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%g", val)
	case []interface{}:
		parts := make([]string, 0, len(val))
		for _, e := range val {
			if s := strings.TrimSpace(renderCellValue(e)); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	default:
		b, _ := json.Marshal(val)
		return string(b)
	}
}

// buildCellPrompt builds the per-row instruction: the column's AI prompt plus
// the row's OTHER column values as "Name: value" context (the target column and
// other AI columns are excluded so a fill can't feed on itself). Pure +
// unit-tested. valuesJSON is the row's stored values (keyed by field id).
func buildCellPrompt(columnPrompt string, fields []*model.Field, valuesJSON, targetFieldID string) string {
	var values map[string]interface{}
	_ = json.Unmarshal([]byte(emptyObjIfBlank(valuesJSON)), &values)

	var ctxLines strings.Builder
	for _, f := range fields {
		if f.Id.String() == targetFieldID {
			continue
		}
		if aiPromptFromConfig(f.Config) != "" {
			continue // don't feed one AI column into another
		}
		raw, ok := values[f.Id.String()]
		if !ok {
			continue
		}
		s := strings.TrimSpace(renderCellValue(raw))
		if s == "" {
			continue
		}
		ctxLines.WriteString("- " + strings.TrimSpace(f.Name) + ": " + s + "\n")
	}

	var b strings.Builder
	b.WriteString(strings.TrimSpace(columnPrompt))
	if ctxLines.Len() > 0 {
		b.WriteString("\n\nRow data:\n")
		b.WriteString(strings.TrimSpace(ctxLines.String()))
	}
	b.WriteString("\n\nReturn ONLY the value for this cell as plain text — no preamble, labels, or quotes.")
	out := b.String()
	if len(out) > aiCellPromptBudget {
		out = out[:aiCellPromptBudget]
	}
	return out
}

func emptyObjIfBlank(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// filler is what fills cells for one person: their model and its breaker,
// and the AI's rate limit, which allow says whether one more call may go now.
type filler struct {
	llm   ai.LLMProvider
	cb    *ai.CircuitBreaker
	allow func(ctx context.Context) bool
}

// fillerFor is a person's filler, through the shared AI service (their model,
// the circuit breaker, the per-person rate limit); false where AI is off for
// them. A seam for tests.
var fillerFor = func(ctx context.Context, user string) (filler, bool) {
	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return filler{}, false
	}
	llm, cb := svc.ResolveUserModel(ctx, user)
	if llm == nil {
		return filler{}, false
	}
	return filler{llm: llm, cb: cb, allow: func(ctx context.Context) bool {
		return cb.Allow() == nil && svc.Resiliency.CheckRateLimit(ctx, user) == nil
	}}, true
}

// FillInput is what a fill is asked for: RowIds for "Fill again" on those
// rows, none for the column's empty cells. Confirm is the person's yes to a
// fill of more than aiFillConfirmOver cells (nil is yes). Most, when set,
// takes fewer cells than maxAIFillCells: a web from before fills took the
// whole column sends no confirm, and its fill takes maxAIFillRows, as it did
// (the controller says so).
type FillInput struct {
	RowIds  []uuid.UUID
	Confirm *bool
	Most    int
}

// FillAIColumnWith fills an AI column. A fill of the column takes its cells
// that are empty and no person's, every one up to maxAIFillCells, in the
// table's order: what the AI's rate allows in aiFillRequestFor is written in
// the request, and the rest in the background, turn after turn (an outbox
// event, aiFillSink), so it says how many are left rather than that it's
// done. Past aiFillConfirmOver cells it asks first (NeedsConfirm). One fill
// runs per column: asking again while one runs answers that one. Its writes
// are one change set, its person's (model.AIFill, migration 509), which an
// undo naming it reverses whole. Naming rows is "Fill again": those rows are
// refilled in the request, whoever wrote them, into one change set too, and
// any the rate didn't reach are counted as skipped. A cell the model fails is
// skipped, never failing the fill.
func FillAIColumnWith(ctx context.Context, tableId, fieldId uuid.UUID, in FillInput, actor Actor) (*FillResult, error) {
	ctx = asViewer(ctx, actor)
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	fields, ferr := model.ListFields(ctx, tableId)
	if ferr != nil {
		return nil, fmt.Errorf("failed to load columns")
	}
	target := aiColumn(fields, fieldId)
	if target == nil {
		return nil, errNotFound
	}
	prompt := aiPromptFromConfig(target.Config)
	if prompt == "" {
		return nil, fmt.Errorf("this column is not an AI column")
	}
	f, ok := fillerFor(ctx, actor.UserID.String())
	if !ok {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	if len(in.RowIds) > 0 {
		return fillAgain(ctx, f, t, fields, target, prompt, in.RowIds, actor)
	}

	res := &FillResult{}
	if running, rerr := model.RunningAIFill(ctx, tableId, target.Id); rerr != nil {
		return nil, fmt.Errorf("failed to read the column's fill")
	} else if running != nil {
		return runningFill(running), nil
	}
	empty, cerr := model.CountRowsToFill(ctx, tableId, target.Id.String())
	if cerr != nil {
		return nil, fmt.Errorf("failed to load rows")
	}
	most := maxAIFillCells
	if in.Most > 0 {
		most = min(in.Most, maxAIFillCells)
	}
	res.Total = min(empty, most)
	res.More = max(empty-res.Total, 0)
	if res.Total == 0 {
		return res, nil
	}
	if in.Confirm != nil && !*in.Confirm && res.Total > aiFillConfirmOver {
		res.NeedsConfirm = true
		return res, nil
	}
	cs := fillChangeSet(ctx, t.Id, actor)
	fill, serr := model.StartAIFill(ctx, t.Id, target.Id, actor.UserID, res.Total, cs)
	if errors.Is(serr, model.ErrAIFillRunning) && fill != nil {
		return runningFill(fill), nil
	}
	if serr != nil {
		return nil, fmt.Errorf("failed to start the fill")
	}
	res.ChangeSet = &cs.ID
	rows, err := model.RowsToFillAfter(ctx, tableId, target.Id.String(), "", uuid.Nil, min(res.Total, aiFillPage))
	if err != nil {
		return nil, fmt.Errorf("failed to load rows")
	}
	run := &fillRun{into: cs.ID, fill: fill.ID, table: t.Id}
	asked, filled := fillCells(ctx, f, t, fields, cellsOfColumn(rows, target, prompt), false, time.Now().Add(aiFillRequestFor), run)
	res.Filled, res.Skipped = filled, asked-filled
	fill = advanceFill(ctx, t, fill, rows, asked, filled, len(rows) < aiFillPage, run)
	res.Fill = fill
	if fill.State == model.AIFillRunning {
		if qerr := queueRest(ctx, aiFillJob{table: tableId, field: target.Id, fill: fill.ID, budget: fill.Left(), by: actor}); qerr != nil {
			helpers.LogErrorWithContext(ctx, "FillAIColumnWith: the rest of the fill wasn't queued: %v", qerr)
			if stopped, _ := model.StopAIFill(context.WithoutCancel(ctx), t.Id, fill.ID, uuid.Nil); stopped != nil {
				res.Fill = stopped
			}
			res.Skipped += fill.Left()
		} else {
			res.Left = fill.Left()
		}
	}
	return res, nil
}

// fillAgain refills rowIds' cells of target in the request, whoever wrote
// them, into one change set of the actor's, which their undo can name; the
// rows the AI's rate didn't reach are counted as skipped.
func fillAgain(ctx context.Context, f filler, t *model.DataTable, fields []*model.Field, target *model.Field, prompt string, rowIds []uuid.UUID, actor Actor) (*FillResult, error) {
	rowIds = rowIds[:min(len(rowIds), maxAIFillRows)]
	found, err := model.RowsByIDs(ctx, t.Id, rowIds)
	if err != nil {
		return nil, fmt.Errorf("failed to load rows")
	}
	byID := make(map[uuid.UUID]*model.Row, len(found))
	for _, rw := range found {
		byID[rw.Id] = rw
	}
	var rows []*model.Row // in the order they were named, each once
	for _, rid := range rowIds {
		if rw := byID[rid]; rw != nil {
			rows = append(rows, rw)
			delete(byID, rid)
		}
	}
	res := &FillResult{Total: len(rows)}
	if len(rows) == 0 {
		return res, nil
	}
	cs := fillChangeSet(ctx, t.Id, actor)
	if err := model.StartChangeSet(ctx, cs); err != nil {
		return nil, fmt.Errorf("failed to start the fill")
	}
	res.ChangeSet = &cs.ID
	asked, filled := fillCells(ctx, f, t, fields, cellsOfColumn(rows, target, prompt), true, time.Time{}, &fillRun{into: cs.ID})
	res.Filled, res.Skipped = filled, asked-filled+len(rows)-asked
	return res, nil
}

// fillChangeSet is a fill's change set: the actor's, so their undo can name
// it, but made for them by the AI (an automation), so an undo that names
// none passes it by (model.LatestToUndo) and a person's own edits stay first.
func fillChangeSet(ctx context.Context, table uuid.UUID, actor Actor) *model.ChangeSet {
	cs := newChangeSet(ctx, table, actor, rowLedger.ChangeAI)
	cs.ID, cs.ActorKind = uuid.New(), string(rowLedger.ActorAutomation)
	return cs
}

// runningFill is the answer to a fill asked for while one runs: that one.
func runningFill(f *model.AIFill) *FillResult {
	return &FillResult{Total: f.Total, Left: f.Left(), Filled: f.Filled, Skipped: f.Asked - f.Filled, Fill: f, ChangeSet: f.ChangeSet}
}

// fillRun is where a fill, or a row's autofill, writes and whether it goes
// on: into is its change set (uuid.Nil: each write its own, the AI's, as a
// turn queued by a server from before they had one writes), fill the fill
// (uuid.Nil for none), read before each cell so a fill stopped ends within a
// cell, and undone is set when a write finds the change set undone. A nil
// run is no fill.
type fillRun struct {
	into, fill, table uuid.UUID
	undone, stopped   bool
}

func (r *fillRun) changeSet() uuid.UUID {
	if r == nil {
		return uuid.Nil
	}
	return r.into
}

func (r *fillRun) ended() bool { return r != nil && (r.undone || r.stopped) }

// mayGoOn is whether the fill may ask for one more cell: it wasn't undone,
// and wasn't stopped (read now; a read that fails goes on, as the next turn
// reads it again).
func (r *fillRun) mayGoOn(ctx context.Context) bool {
	if r == nil {
		return true
	}
	if r.ended() {
		return false
	}
	if r.fill != uuid.Nil {
		if f, err := model.GetAIFill(ctx, r.table, r.fill); err == nil && (f == nil || f.State != model.AIFillRunning) {
			r.stopped = true
			return false
		}
	}
	return true
}

// advanceFill moves fill on by what a request or a turn did over rows (asked
// of them asked for, filled written), to the last row it asked for, ending
// it when the table has no rows past them (last, all asked), or when it was
// undone, and tells the table's open views. It answers the fill as it now is.
func advanceFill(ctx context.Context, t *model.DataTable, fill *model.AIFill, rows []*model.Row, asked, filled int, last bool, run *fillRun) *model.AIFill {
	ctx = context.WithoutCancel(ctx)
	key, row := "", uuid.Nil
	if asked > 0 {
		key, row = rows[asked-1].PositionKey, rows[asked-1].Id
	}
	done := run.undone || (last && asked == len(rows))
	now, err := model.AdvanceAIFill(ctx, t.Id, fill.ID, asked, filled, key, row, done)
	if err != nil || now == nil {
		return fill
	}
	broadcastFill(t.Id.String(), now)
	return now
}

// broadcastFill tells a table's open views how its fill stands.
func broadcastFill(tableID string, f *model.AIFill) {
	mqttBusiness.PublishTableRow(tableID, map[string]interface{}{"action": "ai_fill", "fill": f})
}

// StopAIFill stops the fill running for a column of a table, as actor: the
// person who started it, or one who manages the table. It answers the fill as
// it now is; nil when none runs.
func StopAIFill(ctx context.Context, tableId, fieldId uuid.UUID, actor Actor) (*model.AIFill, error) {
	ctx = asViewer(ctx, actor)
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	fill, err := model.RunningAIFill(ctx, tableId, fieldId)
	if err != nil {
		return nil, fmt.Errorf("failed to read the column's fill")
	}
	if fill == nil {
		return nil, nil
	}
	if (fill.StartedBy == nil || *fill.StartedBy != actor.UserID) && !canManage(t, actor) {
		return nil, errForbidden
	}
	now, err := model.StopAIFill(ctx, tableId, fill.ID, actor.UserID)
	if err != nil {
		return nil, fmt.Errorf("failed to stop the fill")
	}
	broadcastFill(tableId.String(), now)
	return now, nil
}

// AIFills are a table's fills that run, and those that ended in the last
// day: what an open grid shows the progress of.
func AIFills(ctx context.Context, tableId uuid.UUID, actor Actor) ([]*model.AIFill, error) {
	ctx = asViewer(ctx, actor)
	if _, err := loadViewable(ctx, tableId, actor); err != nil {
		return nil, err
	}
	fills, err := model.RecentAIFills(ctx, tableId)
	if err != nil {
		return nil, fmt.Errorf("failed to read the table's fills")
	}
	return fills, nil
}

// aiColumn is fields' field id, nil when there's none such.
func aiColumn(fields []*model.Field, id uuid.UUID) *model.Field {
	for _, f := range fields {
		if f.Id == id {
			return f
		}
	}
	return nil
}

// aiCell is one cell a fill asks the model for: target's cell of row, by
// prompt.
type aiCell struct {
	row    *model.Row
	target *model.Field
	prompt string
}

// cellsOfColumn are target's cells of rows.
func cellsOfColumn(rows []*model.Row, target *model.Field, prompt string) []aiCell {
	out := make([]aiCell, len(rows))
	for i, rw := range rows {
		out[i] = aiCell{row: rw, target: target, prompt: prompt}
	}
	return out
}

// cellsOfRow are row's cells of targets, auto AI columns, each by its prompt.
func cellsOfRow(row *model.Row, targets []*model.Field) []aiCell {
	out := make([]aiCell, len(targets))
	for i, target := range targets {
		prompt, _ := aiConfigFromConfig(target.Config)
		out[i] = aiCell{row: row, target: target, prompt: prompt}
	}
	return out
}

// columnsOf are the columns of cells, by id.
func columnsOf(cells []aiCell) []uuid.UUID {
	out := make([]uuid.UUID, len(cells))
	for i, c := range cells {
		out[i] = c.target.Id
	}
	return out
}

// fillCells fills each of cells in turn while the AI's rate allows, until
// the deadline (none when zero), and while the fill's own time lasts (ctx):
// the one loop of a column's fill, its background turns, a row's autofill
// and its turns. It answers how many it asked the model for, the cells from
// there on being left (the one a call was cut off at among them), and how
// many of those it wrote.
func fillCells(ctx context.Context, f filler, t *model.DataTable, fields []*model.Field, cells []aiCell, again bool, until time.Time, run *fillRun) (asked, filled int) {
	for _, c := range cells {
		if ctx.Err() != nil || (!until.IsZero() && time.Now().After(until)) || !run.mayGoOn(ctx) || !f.allow(ctx) {
			break
		}
		wrote := fillOneCell(ctx, f, t, fields, c.row, c.target, c.prompt, again, run)
		if !wrote && (ctx.Err() != nil || run.ended()) {
			break
		}
		asked++
		if wrote {
			filled++
		}
	}
	return asked, filled
}

// fillOneCell evaluates one AI column's prompt for one row and writes the cell
// through the shared AI chokepoint, and says whether it wrote one. It is the
// single generic primitive behind the fills (FillAIColumnWith, aiFillSink) and
// continuous autofill (AutofillRowOnWrite), through fillCells; the caller
// has had f.allow's yes. A model error records the breaker and fills
// nothing, so a batch keeps going. Persisting via writeCellInto ->
// model.WriteAICell (not the CreateRow/UpdateRow business path) means an
// autofill write never re-triggers row events. again writes over a cell a
// person wrote ("Fill again").
func fillOneCell(ctx context.Context, f filler, t *model.DataTable, fields []*model.Field, rw *model.Row, target *model.Field, prompt string, again bool, run *fillRun) bool {
	// The answer is written only while the cells it's worked out from are as
	// they're read here.
	inputs := readCells(fields, rw, prompt)
	// The prompt sees the row's formulas too; they go into a copy, as the
	// row itself is written back. It's read as a guest: the answer is stored
	// for every reader, so it can't come from a table only this one opens.
	view := *rw
	withComputed(asGuest(ctx), fields, []*model.Row{&view})
	cellPrompt := buildCellPrompt(prompt, fields, view.Values, target.Id.String())
	answer, cerr := ai.ChatWithRescue(ctx, f.llm, []ai.ChatMessage{
		{Role: "system", Content: "You fill a single spreadsheet cell. Reply with only the cell value, concise and plain."},
		{Role: "user", Content: cellPrompt},
	}, ai.ChatOptions{Temperature: 0.2, MaxTokens: 256})
	if cerr != nil {
		// A call cut off because the fill's own time ran out (a background
		// turn's minute) says nothing about the model, so its breaker isn't
		// told.
		if ctx.Err() == nil {
			f.cb.RecordResult(cerr)
		}
		return false
	}
	f.cb.RecordSuccess()
	answer = strings.TrimSpace(answer)
	// Cut by characters: a cut by bytes split a letter of an answer in Hindi.
	if r := []rune(answer); len(r) > maxAICellChars {
		answer = string(r[:maxAICellChars])
	}
	if answer == "" {
		return false
	}
	if uerr := writeCellInto(ctx, t, rw, target, answer, inputs, again, run.changeSet()); uerr != nil {
		if errors.Is(uerr, model.ErrUndone) { // the fill was undone while the model answered: it ends
			run.undone = true
			return false
		}
		if !errors.Is(uerr, model.ErrAIKept) && !errors.Is(uerr, model.ErrCellChanged) { // written while the model answered
			helpers.LogErrorWithContext(ctx, "fillOneCell write cell err: %v", uerr)
		}
		return false
	}
	return true
}

// AutofillRowOnWrite refills a row's AI columns marked auto=true after a write
// changed its cells (changed, by field id) — Notion-style continuous AI
// autofill — those of them autofillDue says: a column whose prompt reads none
// of the changed cells isn't asked again, nor one whose cell the write set or
// a person wrote. Those the AI's rate doesn't reach now are left to the
// background (aiFillSink), rather than dropped. Its writes, now and in the
// background, are one change set of the writer's, made for them by the AI as
// a column's fill is (fillChangeSet): their undo can name it and puts the row
// back whole, and an undo that names none passes it by. It is best-effort and safe to
// run in a goroutine: it runs AS the writing actor through the shared AI
// service (per-user model, circuit breaker, rate limit, token budget,
// residency), and never loops because fillOneCell persists via the model
// layer directly, so an autofill write does not re-dispatch
// table.row.updated. The actor has already been permission-checked by the
// CreateRow/UpdateRow caller.
func AutofillRowOnWrite(ctx context.Context, t *model.DataTable, row *model.Row, changed map[string]bool, actor Actor) {
	if t == nil || row == nil {
		return
	}
	fields, ferr := model.ListFields(ctx, t.Id)
	if ferr != nil {
		return
	}
	due := autofillDue(fields, row, changed)
	if len(due) == 0 {
		return
	}
	f, ok := fillerFor(ctx, actor.UserID.String())
	if !ok {
		return
	}
	cs := fillChangeSet(ctx, t.Id, actor)
	if err := model.StartChangeSet(ctx, cs); err != nil {
		helpers.LogErrorWithContext(ctx, "AutofillRowOnWrite: the row's autofill has no change set: %v", err)
		return
	}
	cells := cellsOfRow(row, due)
	run := &fillRun{into: cs.ID}
	asked, _ := fillCells(ctx, f, t, fields, cells, false, time.Time{}, run)
	if left := cells[asked:]; len(left) > 0 && !run.ended() {
		if err := queueRest(ctx, aiFillJob{table: t.Id, row: row.Id, fields: columnsOf(left), into: cs.ID, by: actor}); err != nil {
			helpers.LogErrorWithContext(ctx, "AutofillRowOnWrite: the row's autofill wasn't queued: %v", err)
		}
	}
}

// autofillDue is the auto AI columns a write that changed these cells (by
// field id) should fill again in row: those whose prompt reads a changed cell
// (promptReads), unless the write set the column's own cell or a person wrote
// it (row.AIKept). Every edit asked the model again for every auto column, and
// wrote its answer over a person's correction seconds later.
func autofillDue(fields []*model.Field, row *model.Row, changed map[string]bool) []*model.Field {
	isAI := map[string]bool{}
	for _, id := range aiColumns(fields) {
		isAI[id] = true
	}
	var due []*model.Field
	for _, f := range fields {
		prompt, auto := aiConfigFromConfig(f.Config)
		id := f.Id.String()
		if prompt == "" || !auto || changed[id] || slices.Contains(row.AIKept, id) {
			continue
		}
		reads, all := promptReads(prompt, fields)
		for c := range changed {
			if reads[c] || (all && !isAI[c]) {
				due = append(due, f)
				break
			}
		}
	}
	return due
}

// promptReads is the cells an AI column's prompt reads, by field id: the
// fields it names as a formula does ({Notes}), and those the formulas among
// them read; all is true when it names none, and so reads every cell but the
// AI columns', as the cell prompt shows them all (buildCellPrompt).
func promptReads(prompt string, fields []*model.Field) (reads map[string]bool, all bool) {
	byID := make(map[string]*model.Field, len(fields))
	for _, f := range fields {
		byID[f.Id.String()] = f
	}
	reads = map[string]bool{}
	var read func(f *model.Field)
	read = func(f *model.Field) {
		if f == nil || reads[f.Id.String()] {
			return
		}
		reads[f.Id.String()] = true
		if f.Type == model.FieldFormula {
			for _, ref := range formula.Refs(storedFormula(f)) {
				read(byID[strings.TrimPrefix(ref, "#")])
			}
		}
	}
	for _, name := range formula.TextRefs(prompt) {
		read(resolveField(fields, strings.TrimPrefix(name, "#")))
	}
	return reads, len(reads) == 0
}

// aiColumns is the ids of a table's AI columns.
func aiColumns(fields []*model.Field) []string {
	var ids []string
	for _, f := range fields {
		if aiPromptFromConfig(f.Config) != "" {
			ids = append(ids, f.Id.String())
		}
	}
	return ids
}

// changedCells is the cells, by field id, that differ between a row's values
// as they were (was) and are (is); the AI columns' marks (AIKeptKey) aren't a
// cell.
func changedCells(was, is string) map[string]bool {
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal([]byte(was), &before)
	_ = json.Unmarshal([]byte(is), &after)
	changed := map[string]bool{}
	for id, v := range after {
		if !sameJSON(before[id], v) {
			changed[id] = true
		}
	}
	for id := range before {
		if _, kept := after[id]; !kept {
			changed[id] = true
		}
	}
	delete(changed, model.AIKeptKey)
	return changed
}

// sameJSON is whether two JSON values are the same value, however they're
// written: spacing, key order and escapes aside. A row with a person's AI
// cells has its values written again as its marks are lifted out
// (model.liftAIKept), with "&" as "\u0026": compared as written, every such
// cell read as changed, and asked the row's AI columns again.
func sameJSON(a, b json.RawMessage) bool {
	if a == nil || b == nil {
		return bytes.Equal(a, b)
	}
	x, xerr := decodeJSON(a)
	y, yerr := decodeJSON(b)
	if xerr != nil || yerr != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(x, y)
}

// decodeJSON is a JSON value decoded, its numbers as written.
func decodeJSON(b json.RawMessage) (interface{}, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v interface{}
	err := d.Decode(&v)
	return v, err
}

// readCells is the cells an AI column's answer is worked out from, as the
// row the fill read (rw, as the row stores them) holds them, by field id:
// those its prompt reads (promptReads), or every cell but the AI columns'
// when it names none; JSON null for one the row hadn't. Those worked out on
// each read (formulas, rollups, links to tables) aren't stored; the fields a
// formula reads are among those the prompt reads.
func readCells(fields []*model.Field, rw *model.Row, prompt string) string {
	reads, all := promptReads(prompt, fields)
	isAI := map[string]bool{}
	for _, id := range aiColumns(fields) {
		isAI[id] = true
	}
	var cells map[string]json.RawMessage
	_ = json.Unmarshal([]byte(rw.Values), &cells)
	read := map[string]json.RawMessage{}
	for _, f := range fields {
		id := f.Id.String()
		_, isLink := linkOf(f)
		if isAI[id] || isLink || f.Type == model.FieldFormula || f.Type == model.FieldRollup || !(all || reads[id]) {
			continue
		}
		read[id] = json.RawMessage("null")
		if c, ok := cells[id]; ok {
			read[id] = c
		}
	}
	b, err := json.Marshal(read)
	if err != nil {
		return ""
	}
	return string(b)
}

// writeCellInto writes an AI column's answer into a row and broadcasts it (so the
// live grid updates). Only that cell: the row was read before the model was
// asked, seconds ago, and writing that copy back undid whatever anyone changed
// in the meantime, a cell or the row's place. A cell a person wrote since is
// left as they wrote it (model.ErrAIKept), unless this is "Fill again"; one
// that changed since the fill read it (rw), or whose row's cells the answer
// read (inputs, readCells) changed, is left as it now is, Fill again or not
// (model.ErrCellChanged): a newer fill answers for them.
//
// The answer is stored as the column's kind stores what any writer sends
// (business/FieldTypes): a number column's answer as a number, a select's as
// one of its options, or not at all when it isn't one.
//
// Its change is added to the change set into, a fill's or a row's
// autofill's, stored already (model.ChangeSet.Append); with none (uuid.Nil)
// the write is a change set of its own, the AI's. A change set undone
// already takes no more (model.ErrUndone).
func writeCellInto(ctx context.Context, t *model.DataTable, rw *model.Row, target *model.Field, value, inputs string, again bool, into uuid.UUID) error {
	fieldID := target.Id.String()
	cells, cerr := fieldTypes.Kinds.Check([]*model.Field{target}, map[string]interface{}{fieldID: value}, fieldTypes.Input{})
	if cerr != nil {
		return cerr
	}
	blob, merr := json.Marshal(map[string]interface{}{fieldID: cells[fieldID]})
	if merr != nil {
		return merr
	}
	var read map[string]json.RawMessage
	_ = json.Unmarshal([]byte(rw.Values), &read)
	// The AI's own change set (the row ledger): no person's, so on no one's
	// undo, and heard by no webhook, as a fill never set off row events. A
	// column's fill and a row's autofill write into their own one (into),
	// whose person can undo it whole by naming it; only a turn queued by a
	// server from before they had one writes a set per cell.
	cs := &model.ChangeSet{TableID: t.Id, ActorKind: string(rowLedger.ActorAutomation), Kind: string(rowLedger.ChangeAI)}
	if into != uuid.Nil {
		cs.ID, cs.Append = into, true
	}
	updated, uerr := model.WriteAICell(ctx, t.Id, rw.Id, fieldID, string(blob), read[fieldID], inputs, again, maxValuesBytes, cs, observe)
	if uerr != nil {
		return uerr
	}
	announce(ctx, t, cs, neither)
	broadcastRow(t.Id.String(), "updated", updated)
	helpers.GoSafeNamed("tellLinkedTables", func() { tellLinkedTables(context.WithoutCancel(ctx), t.Id, rw.Id, false) })
	return nil
}
