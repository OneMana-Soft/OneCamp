package business

// Per-row AI autofill (Notion "AI Autofill"): a column whose cell values are
// produced by an AI prompt over each row. The column is an ordinary field whose
// config carries {"ai":{"prompt":"..."}}; FillAIColumn evaluates that prompt
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

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

const (
	maxAIFillRows      = 200
	maxAICellChars     = 600
	aiCellPromptBudget = 2000
)

// FillResult reports the outcome of an AI-column fill.
type FillResult struct {
	Filled  int `json:"filled"`
	Skipped int `json:"skipped"`
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

// FillAIColumn evaluates an AI column's prompt over each row (all rows, or the
// given subset) and writes the result. Runs AS the actor through the shared AI
// service. Best-effort per row: a failed cell is skipped (counted), never
// failing the whole fill. Returns counts. A fill of every row leaves the cells
// a person wrote as they are (counted as skipped); naming rows is "Fill
// again", which refills them too and takes off their mark.
func FillAIColumn(ctx context.Context, tableId, fieldId uuid.UUID, rowIds []uuid.UUID, actor Actor) (*FillResult, error) {
	ctx = asViewer(ctx, actor)
	t, err := loadViewable(ctx, tableId, actor)
	if err != nil {
		return nil, err
	}
	fields, ferr := model.ListFields(ctx, tableId)
	if ferr != nil {
		return nil, fmt.Errorf("failed to load columns")
	}
	var target *model.Field
	for _, f := range fields {
		if f.Id == fieldId {
			target = f
			break
		}
	}
	if target == nil {
		return nil, errNotFound
	}
	prompt := aiPromptFromConfig(target.Config)
	if prompt == "" {
		return nil, fmt.Errorf("this column is not an AI column")
	}

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}
	userUUID := actor.UserID.String()
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return nil, fmt.Errorf("AI is not enabled for this workspace")
	}

	// Resolve the rows to fill.
	var rows []*model.Row
	if len(rowIds) > 0 {
		for _, rid := range rowIds {
			if len(rows) >= maxAIFillRows {
				break
			}
			if rw, rerr := model.GetRowByID(ctx, tableId, rid); rerr == nil && rw != nil {
				rows = append(rows, rw)
			}
		}
	} else {
		rows, err = model.ListRows(ctx, tableId, maxAIFillRows, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to load rows")
		}
	}

	again := len(rowIds) > 0
	todo, skipped := fillTargets(rows, target.Id.String(), again)
	res := &FillResult{Skipped: skipped}
	for _, rw := range todo {
		if err := cb.Allow(); err != nil {
			break // circuit open: stop cleanly, report what we filled
		}
		if rlErr := svc.Resiliency.CheckRateLimit(ctx, userUUID); rlErr != nil {
			break
		}
		filled, stop := fillOneCell(ctx, llm, cb, t, fields, rw, target, prompt, again)
		if stop {
			break
		}
		if filled {
			res.Filled++
		} else {
			res.Skipped++
		}
	}
	return res, nil
}

// fillTargets is the rows a fill of field writes: every one it was given by
// name ("Fill again"), else those whose cell no person wrote; skipped counts
// the rest. Pure.
func fillTargets(rows []*model.Row, field string, again bool) (todo []*model.Row, skipped int) {
	if again {
		return rows, 0
	}
	for _, rw := range rows {
		if slices.Contains(rw.AIKept, field) {
			skipped++
			continue
		}
		todo = append(todo, rw)
	}
	return todo, skipped
}

// fillOneCell evaluates one AI column's prompt for one row and writes the cell
// through the shared AI chokepoint. It is the single generic primitive behind
// both on-demand batch fill (FillAIColumn) and continuous autofill
// (AutofillRowOnWrite). The caller must already have passed cb.Allow() and the
// rate-limit gate for this call. Returns (filled, stop): filled is whether a
// value was written; stop signals the caller to break its loop (never used
// here but reserved so a future hard-stop error can propagate). Best-effort: a
// model error records the breaker and returns (false, false) so a batch keeps
// going. Persisting via writeCell -> model.WriteAICell (not the CreateRow/
// UpdateRow business path) means an autofill write never re-triggers row events.
// again writes over a cell a person wrote ("Fill again").
func fillOneCell(ctx context.Context, llm ai.LLMProvider, cb *ai.CircuitBreaker, t *model.DataTable, fields []*model.Field, rw *model.Row, target *model.Field, prompt string, again bool) (filled bool, stop bool) {
	// The answer is written only while the cells it's worked out from are as
	// they're read here.
	inputs := readCells(fields, rw, prompt)
	// The prompt sees the row's formulas too; they go into a copy, as the
	// row itself is written back. It's read as a guest: the answer is stored
	// for every reader, so it can't come from a table only this one opens.
	view := *rw
	withComputed(asGuest(ctx), fields, []*model.Row{&view})
	cellPrompt := buildCellPrompt(prompt, fields, view.Values, target.Id.String())
	answer, cerr := ai.ChatWithRescue(ctx, llm, []ai.ChatMessage{
		{Role: "system", Content: "You fill a single spreadsheet cell. Reply with only the cell value, concise and plain."},
		{Role: "user", Content: cellPrompt},
	}, ai.ChatOptions{Temperature: 0.2, MaxTokens: 256})
	if cerr != nil {
		cb.RecordResult(cerr)
		return false, false
	}
	cb.RecordSuccess()
	answer = strings.TrimSpace(answer)
	// Cut by characters: a cut by bytes split a letter of an answer in Hindi.
	if r := []rune(answer); len(r) > maxAICellChars {
		answer = string(r[:maxAICellChars])
	}
	if answer == "" {
		return false, false
	}
	if uerr := writeCell(ctx, t, rw, target, answer, inputs, again); uerr != nil {
		if !errors.Is(uerr, model.ErrAIKept) && !errors.Is(uerr, model.ErrCellChanged) { // written while the model answered
			helpers.LogErrorWithContext(ctx, "fillOneCell write cell err: %v", uerr)
		}
		return false, false
	}
	return true, false
}

// AutofillRowOnWrite refills a row's AI columns marked auto=true after a write
// changed its cells (changed, by field id) — Notion-style continuous AI
// autofill — those of them autofillDue says: a column whose prompt reads none
// of the changed cells isn't asked again, nor one whose cell the write set or
// a person wrote. It is best-effort and safe to run in a goroutine: it runs AS
// the writing actor through the shared AI service (per-user model, circuit
// breaker, rate limit, token budget, residency), and never loops because
// fillOneCell persists via the model layer directly, so an autofill write does
// not re-dispatch table.row.updated. The actor has already been
// permission-checked by the CreateRow/UpdateRow caller.
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

	svc := ai.GetService()
	if svc == nil || !svc.IsEnabled() {
		return
	}
	userUUID := actor.UserID.String()
	llm, cb := svc.ResolveUserModel(ctx, userUUID)
	if llm == nil {
		return
	}
	for _, target := range due {
		if err := cb.Allow(); err != nil {
			break
		}
		if rlErr := svc.Resiliency.CheckRateLimit(ctx, userUUID); rlErr != nil {
			break
		}
		prompt, _ := aiConfigFromConfig(target.Config)
		if _, stop := fillOneCell(ctx, llm, cb, t, fields, row, target, prompt, false); stop {
			break
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

// writeCell writes an AI column's answer into a row and broadcasts it (so the
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
func writeCell(ctx context.Context, t *model.DataTable, rw *model.Row, target *model.Field, value, inputs string, again bool) error {
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
	// undo, and heard by no webhook, as a fill never set off row events.
	cs := &model.ChangeSet{TableID: t.Id, ActorKind: string(rowLedger.ActorAutomation), Kind: string(rowLedger.ChangeAI)}
	updated, uerr := model.WriteAICell(ctx, t.Id, rw.Id, fieldID, string(blob), read[fieldID], inputs, again, maxValuesBytes, cs, observe)
	if uerr != nil {
		return uerr
	}
	announce(ctx, t, cs, neither)
	broadcastRow(t.Id.String(), "updated", updated)
	go tellLinkedTables(context.WithoutCancel(ctx), t.Id, rw.Id, false)
	return nil
}
