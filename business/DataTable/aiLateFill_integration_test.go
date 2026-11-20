//go:build integration

package business

// CR-426 F2: a row write reads its table's fields once and hands them to its
// cell index (rowQuery.WithFieldsRead), for that request alone. Its autofill
// goes on after the request, while the model answers, and writes the answer
// in a transaction of its own: it indexes that cell by the column as it is
// then, as every write did before the fields were held, never by the column
// as the request read it. Each writer that starts an autofill (CreateRow,
// UpdateRow, PatchRow, Batch) has its AI column retyped, number to text,
// while the model answers: the answer's entry is then the text column's.
//
// Run: integ_rel.sh <tree> <log> '^TestALateAutofillIndexesByTheFieldAsItIsNow$' with PKGS='./business/DataTable/'

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// heldAnswer is a model whose next call, once armed, says it was asked and
// answers only what it's then given; any other call answers "7" at once.
type heldAnswer struct {
	mu     sync.Mutex
	armed  bool
	asked  chan struct{}
	answer chan string
}

func (h *heldAnswer) arm() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.armed = true
}

func (h *heldAnswer) Chat(ctx context.Context, _ []ai.ChatMessage, _ ai.ChatOptions) (string, error) {
	h.mu.Lock()
	armed := h.armed
	h.armed = false
	h.mu.Unlock()
	if !armed {
		return "7", nil
	}
	h.asked <- struct{}{}
	select {
	case a := <-h.answer:
		return a, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (h *heldAnswer) ChatStream(context.Context, []ai.ChatMessage, ai.ChatOptions) (<-chan ai.StreamChunk, error) {
	return nil, fmt.Errorf("not streamed")
}

func (h *heldAnswer) ProviderName() ai.ProviderType { return "test" }

func TestALateAutofillIndexesByTheFieldAsItIsNow(t *testing.T) {
	s := newFillScene(t)
	w := s.w
	held := &heldAnswer{asked: make(chan struct{}, 1), answer: make(chan string, 1)}
	was := fillerFor
	t.Cleanup(func() { fillerFor = was })
	breaker := ai.NewCircuitBreaker(1, time.Hour)
	fillerFor = func(context.Context, string) (filler, bool) {
		return filler{llm: held, cb: breaker, allow: func(context.Context) bool { return true }}, true
	}
	notes := s.notes.Id.String()
	writers := []struct {
		name  string
		write func(t *testing.T, row uuid.UUID, note string) uuid.UUID
	}{
		{"CreateRow", func(t *testing.T, _ uuid.UUID, note string) uuid.UUID {
			made, err := CreateRow(w.ctx, s.tb.Id, RowInput{Values: map[string]interface{}{notes: note}}, w.me)
			if err != nil {
				t.Fatal(err)
			}
			return made.Id
		}},
		{"UpdateRow", func(t *testing.T, row uuid.UUID, note string) uuid.UUID {
			if _, err := UpdateRow(w.ctx, s.tb.Id, row, RowInput{Values: map[string]interface{}{notes: note}}, w.me); err != nil {
				t.Fatal(err)
			}
			return row
		}},
		{"PatchRow", func(t *testing.T, row uuid.UUID, note string) uuid.UUID {
			if _, err := PatchRow(w.ctx, s.tb.Id, row, map[string]interface{}{notes: note}, w.me); err != nil {
				t.Fatal(err)
			}
			return row
		}},
		{"Batch", func(t *testing.T, row uuid.UUID, note string) uuid.UUID {
			if _, err := Batch(w.ctx, s.tb.Id, BatchInput{Patch: []PatchInput{{Row: row, Values: map[string]interface{}{notes: note}}}}, w.me); err != nil {
				t.Fatal(err)
			}
			return row
		}},
	}
	for i, wr := range writers {
		t.Run(wr.name, func(t *testing.T) {
			score := w.field(s.tb, fmt.Sprintf("Score %d", i), model.FieldNumber,
				map[string]interface{}{"ai": map[string]interface{}{"prompt": "Score of {Notes}", "auto": true}})
			held.arm()
			row := wr.write(t, s.rows[3+i], fmt.Sprintf("note %d, again", i))
			select {
			case <-held.asked:
			case <-time.After(20 * time.Second):
				t.Fatal("the write's autofill never asked the model")
			}
			// The column changes while the model answers.
			text := model.FieldText
			if _, err := UpdateField(w.ctx, s.tb.Id, score.Id, FieldPatch{Type: &text}, w.me); err != nil {
				t.Fatal(err)
			}
			held.answer <- "42"
			key := score.Id.String()
			for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				r, err := model.GetRowByID(w.ctx, s.tb.Id, row)
				if err != nil || r == nil {
					t.Fatal(err)
				}
				var cells map[string]json.RawMessage
				_ = json.Unmarshal([]byte(r.Values), &cells)
				if v, ok := cells[key]; ok {
					if string(v) != "42" {
						t.Fatalf("the autofill wrote %s, want 42", v)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the autofill never wrote its answer")
				}
			}
			var txt sql.NullString
			var num sql.NullFloat64
			if err := postgresInit.DBConn.SqlDB.QueryRowContext(w.ctx,
				`SELECT t, n FROM data_table_cell_index WHERE field_id = $1 AND row_id = $2`, score.Id, row).Scan(&txt, &num); err != nil {
				t.Fatal(err)
			}
			if !txt.Valid || txt.String != "42" || num.Valid {
				t.Errorf("%s: the answer written after its column became text is indexed t=%v n=%v, want the text column's entry (t=42, no n): it was indexed by the column as the request read it", wr.name, txt, num)
			}
			// Gone, it fills no later writer's row.
			if err := DeleteField(w.ctx, s.tb.Id, score.Id, w.me); err != nil {
				t.Fatal(err)
			}
		})
	}
}
