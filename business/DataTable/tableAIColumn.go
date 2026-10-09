package business

// Per-row AI autofill (Notion "AI Autofill"): a column whose cell values are
// produced by an AI prompt over each row. The column is an ordinary field whose
// config carries {"ai":{"prompt":"..."}}; FillAIColumn evaluates that prompt
// against each row's other column values via the shared AI chokepoint (per-user
// model, circuit breaker, rate limit, token budget — same residency rules as
// every other AI call) and writes the cell. Reuses the Data Table model +
// permission model; no parallel AI runtime.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
// failing the whole fill. Returns counts. Idempotent: re-running replaces the
// prior AI value.
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

	res := &FillResult{}
	for _, rw := range rows {
		if err := cb.Allow(); err != nil {
			break // circuit open: stop cleanly, report what we filled
		}
		if rlErr := svc.Resiliency.CheckRateLimit(ctx, userUUID); rlErr != nil {
			break
		}
		filled, stop := fillOneCell(ctx, llm, cb, t, fields, rw, target, prompt)
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

// fillOneCell evaluates one AI column's prompt for one row and writes the cell
// through the shared AI chokepoint. It is the single generic primitive behind
// both on-demand batch fill (FillAIColumn) and continuous autofill
// (AutofillRowOnWrite). The caller must already have passed cb.Allow() and the
// rate-limit gate for this call. Returns (filled, stop): filled is whether a
// value was written; stop signals the caller to break its loop (never used
// here but reserved so a future hard-stop error can propagate). Best-effort: a
// model error records the breaker and returns (false, false) so a batch keeps
// going. Persisting via writeCell -> model.UpdateRowValues (not the CreateRow/
// UpdateRow business path) means an autofill write never re-triggers row events.
func fillOneCell(ctx context.Context, llm ai.LLMProvider, cb *ai.CircuitBreaker, t *model.DataTable, fields []*model.Field, rw *model.Row, target *model.Field, prompt string) (filled bool, stop bool) {
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
	if len(answer) > maxAICellChars {
		answer = answer[:maxAICellChars]
	}
	if answer == "" {
		return false, false
	}
	if uerr := writeCell(ctx, t, rw, target.Id.String(), answer); uerr != nil {
		helpers.LogErrorWithContext(ctx, "fillOneCell write cell err: %v", uerr)
		return false, false
	}
	return true, false
}

// AutofillRowOnWrite recomputes every AI column marked auto=true for a single
// row after it is created or edited — Notion-style continuous AI autofill. It is
// best-effort and safe to run in a goroutine: it runs AS the writing actor
// through the shared AI service (per-user model, circuit breaker, rate limit,
// token budget, residency), and never loops because fillOneCell persists via
// the model layer directly, so an autofill write does not re-dispatch
// table.row.updated. The actor has already been permission-checked by the
// CreateRow/UpdateRow caller. On both create and edit it recomputes the auto
// columns so a cell stays derived from the row's latest inputs.
func AutofillRowOnWrite(ctx context.Context, t *model.DataTable, row *model.Row, actor Actor) {
	if t == nil || row == nil {
		return
	}
	fields, ferr := model.ListFields(ctx, t.Id)
	if ferr != nil {
		return
	}
	var autoCols []*model.Field
	for _, f := range fields {
		if p, auto := aiConfigFromConfig(f.Config); p != "" && auto {
			autoCols = append(autoCols, f)
		}
	}
	if len(autoCols) == 0 {
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
	for _, target := range autoCols {
		if err := cb.Allow(); err != nil {
			break
		}
		if rlErr := svc.Resiliency.CheckRateLimit(ctx, userUUID); rlErr != nil {
			break
		}
		prompt, _ := aiConfigFromConfig(target.Config)
		if _, stop := fillOneCell(ctx, llm, cb, t, fields, row, target, prompt); stop {
			break
		}
	}
}

// writeCell merges a single field's value into a row and persists + broadcasts
// it (so the live grid updates).
func writeCell(ctx context.Context, t *model.DataTable, rw *model.Row, fieldID, value string) error {
	var values map[string]interface{}
	if err := json.Unmarshal([]byte(emptyObjIfBlank(rw.Values)), &values); err != nil {
		values = map[string]interface{}{}
	}
	values[fieldID] = value
	// Formula, rollup and other tables' link values are worked out on each
	// read, never stored.
	values, _ = withoutComputedValues(ctx, t.Id, values)
	blob, merr := json.Marshal(values)
	if merr != nil {
		return merr
	}
	updated, uerr := model.UpdateRowValues(ctx, t.Id, rw.Id, string(blob), rw.Position)
	if uerr != nil {
		return uerr
	}
	broadcastRow(t.Id.String(), "updated", updated)
	go tellLinkedTables(context.WithoutCancel(ctx), t.Id, rw.Id, false)
	return nil
}
