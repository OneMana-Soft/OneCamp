//go:build integration

package business

// Audit 31: a fill of an AI column stopped where the AI's rate stopped it and
// said it was done. It takes the cells that are empty and no person's, writes
// what the rate allows now, and leaves the rest to the background, saying how
// many are left; each background turn writes on at the rate's pace, while its
// person may still authorise work. A row's autofill leaves what the rate
// stopped to a turn the same way, through the same loop (fillCells).
//
// Run: integ_rel.sh <tree> <log> '^(TestAFillLeavesWhatTheRateStoppedToTheBackground|TestAFillStopsWhenItsPersonIsDeactivated|TestARowsAutofillLeavesWhatTheRateStoppedToItsTurn|TestARowsAutofillIsOneChangeSetItsWriterCanUndo|TestARowsAutofillUndoneBeforeItsTurnWritesNoMore|TestATurnCutOffByItsMinuteStillQueuesItsNext)$' with PKGS='./business/DataTable/'

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphUserModel "github.com/akashc777/OneCamp/models/dgraph/User"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// answers is a model that answers each cell with the next of A1, A2, ...,
// except its call cutAt, during which cutOff runs out the fill's time.
type answers struct {
	mu     sync.Mutex
	n      int
	cutAt  int
	cutOff func()
}

func (a *answers) Chat(context.Context, []ai.ChatMessage, ai.ChatOptions) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	if a.n == a.cutAt {
		a.cutOff()
		return "", context.DeadlineExceeded
	}
	return fmt.Sprintf("A%d", a.n), nil
}

func (a *answers) ChatStream(context.Context, []ai.ChatMessage, ai.ChatOptions) (<-chan ai.StreamChunk, error) {
	return nil, fmt.Errorf("not streamed")
}

func (a *answers) ProviderName() ai.ProviderType { return "test" }

// cutCtx is a context whose time runs out when cut is called, as the
// outbox's minute for its sink can during a turn's last call.
type cutCtx struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newCutCtx(parent context.Context) *cutCtx {
	return &cutCtx{Context: parent, done: make(chan struct{})}
}

func (c *cutCtx) Done() <-chan struct{} { return c.done }

func (c *cutCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *cutCtx) cut() { c.once.Do(func() { close(c.done) }) }

// fillScene is an Accounts table whose Summary is an AI column, over eight
// rows: a cell the AI answered, one a person wrote, one a person emptied
// (theirs still), then five empty ones, in the table's order. A model answers
// each cell A1, A2, ..., behind one breaker; the AI's rate allows the calls
// allowed says; the turns a fill leaves the background wait in queued; and
// the turns run as person, as they read now, or fail to read with lookup.
type fillScene struct {
	t       *testing.T
	w       *relationWorld
	tb      *model.DataTable
	notes   *model.Field
	summary *model.Field
	rows    []uuid.UUID
	model   *answers
	breaker *ai.CircuitBreaker
	allowed int
	queued  []outboxBusiness.Entry
	person  dgraphStruct.DgraphUser
	lookup  error
}

func newFillScene(t *testing.T) *fillScene {
	t.Helper()
	w := newRelationWorld(t)
	tb, _ := w.table("Accounts")
	s := &fillScene{t: t, w: w, tb: tb, model: &answers{}, breaker: ai.NewCircuitBreaker(1, time.Hour),
		notes:   w.field(tb, "Notes", model.FieldText, nil),
		summary: w.field(tb, "Summary", model.FieldText, map[string]interface{}{"ai": map[string]interface{}{"prompt": "Summarise {Notes}"}}),
		person:  dgraphStruct.DgraphUser{Uid: "0x1", Uuid: w.me.UserID.String()}}
	sum := s.summary.Id.String()
	for i := 0; i < 8; i++ {
		s.rows = append(s.rows, w.row(tb, map[string]interface{}{s.notes.Id.String(): fmt.Sprintf("note %d", i)}))
	}
	b, _ := json.Marshal(map[string]string{sum: "earlier"})
	if _, err := model.WriteAICell(w.ctx, tb.Id, s.rows[0], sum, string(b), nil, "", false, 100*1024, nil, nil); err != nil {
		t.Fatal(err)
	}
	for i, v := range []string{"mine", "emptied", ""} {
		if _, err := PatchRow(w.ctx, tb.Id, s.rows[min(i+1, 2)], map[string]interface{}{sum: v}, w.me); err != nil {
			t.Fatal(err)
		}
	}
	wasFiller, wasEnqueue, wasPerson, wasAdmin := fillerFor, enqueueFill, fillPerson, fillIsAdmin
	t.Cleanup(func() { fillerFor, enqueueFill, fillPerson, fillIsAdmin = wasFiller, wasEnqueue, wasPerson, wasAdmin })
	fillerFor = func(context.Context, string) (filler, bool) {
		return filler{llm: s.model, cb: s.breaker, allow: func(context.Context) bool {
			if s.allowed == 0 {
				return false
			}
			s.allowed--
			return true
		}}, true
	}
	// As the outbox's write does, a turn queued on a context whose time is
	// up isn't queued.
	enqueueFill = func(ctx context.Context, es ...outboxBusiness.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.queued = append(s.queued, es...)
		return nil
	}
	fillPerson = func(_ context.Context, id string) (*dgraphStruct.DgraphUser, error) {
		if s.lookup != nil {
			return nil, s.lookup
		}
		if id != s.person.Uuid {
			return nil, fmt.Errorf("no person %s", id)
		}
		p := s.person
		return &p, nil
	}
	fillIsAdmin = func(context.Context, *uuid.UUID) bool { return false }
	return s
}

// cell is row i's cell of f.
func (s *fillScene) cell(i int, f *model.Field) string {
	s.t.Helper()
	r, err := model.GetRowByID(s.w.ctx, s.tb.Id, s.rows[i])
	if err != nil || r == nil {
		s.t.Fatal(err)
	}
	var cells map[string]json.RawMessage
	_ = json.Unmarshal([]byte(r.Values), &cells)
	var v string
	_ = json.Unmarshal(cells[f.Id.String()], &v)
	return v
}

// cells are the Summary cells of rows from to to.
func (s *fillScene) cells(from, to int) []string {
	s.t.Helper()
	var out []string
	for i := from; i < to; i++ {
		out = append(out, s.cell(i, s.summary))
	}
	return out
}

// auto is a new AI column that fills itself from Notes (auto=true): a row's
// autofill refills it when the row's Notes change.
func (s *fillScene) auto(name string) *model.Field {
	s.t.Helper()
	return s.w.field(s.tb, name, model.FieldText, map[string]interface{}{"ai": map[string]interface{}{"prompt": name + " of {Notes}", "auto": true}})
}

// autofillRow runs row i's autofill as its writer, as a write that changed
// its Notes does.
func (s *fillScene) autofillRow(i int) {
	s.t.Helper()
	row, err := model.GetRowByID(s.w.ctx, s.tb.Id, s.rows[i])
	if err != nil || row == nil {
		s.t.Fatal(err)
	}
	AutofillRowOnWrite(s.w.ctx, s.tb, row, map[string]bool{s.notes.Id.String(): true}, s.w.me)
}

// turn runs the first turn queued, as the outbox's sink does.
func (s *fillScene) turn() error {
	s.t.Helper()
	return s.turnIn(s.w.ctx)
}

// turnIn runs the first turn queued in ctx.
func (s *fillScene) turnIn(ctx context.Context) error {
	s.t.Helper()
	if len(s.queued) == 0 {
		s.t.Fatal("no turn was queued")
	}
	e := s.queued[0]
	s.queued = s.queued[1:]
	return runAIFill(ctx, outboxBusiness.Event{Topic: e.Topic, Kind: e.Kind, Key: e.Key, Entities: e.Entities, Actor: e.Actor})
}

func TestAFillLeavesWhatTheRateStoppedToTheBackground(t *testing.T) {
	s := newFillScene(t)
	s.allowed = 2
	res, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 5 || res.Filled != 2 || res.Left != 3 || res.Skipped != 0 || res.More != 0 {
		t.Errorf("the fill says %+v; want 5 to fill, 2 filled, 3 left", *res)
	}
	if got, want := s.cells(0, 6), []string{"earlier", "mine", "", "A1", "A2", ""}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after the fill the cells are %q, want %q: an answer and a person's kept, the first empty ones filled", got, want)
	}
	if len(s.queued) != 1 || s.queued[0].Key != "ai_fill:"+s.tb.Id.String()+":"+s.summary.Id.String() || s.queued[0].Entities["budget"] != "3" {
		t.Fatalf("the rest wasn't queued as one turn of 3: %+v", s.queued)
	}
	if _, stamped := s.queued[0].Entities["admin"]; stamped {
		t.Error("the turn keeps whether its person was an admin when it was queued; it's read when the turn runs")
	}

	s.allowed = 2
	if err := s.turn(); err != nil {
		t.Fatalf("a turn: %v", err)
	}
	if got := s.cells(5, 8); fmt.Sprint(got) != fmt.Sprint([]string{"A3", "A4", ""}) {
		t.Errorf("after a turn the rest are %q", got)
	}
	if len(s.queued) != 1 || s.queued[0].Entities["budget"] != "1" {
		t.Fatalf("the turn didn't queue the next, of 1: %+v", s.queued)
	}
	next := s.queued[0]
	if err := s.turn(); err == nil {
		t.Error("a turn the rate stopped before it asked anything said it was done")
	}
	s.queued = append(s.queued, next)
	s.allowed = 5
	if err := s.turn(); err != nil {
		t.Fatalf("the last turn: %v", err)
	}
	if got := s.cells(7, 8); got[0] != "A5" || len(s.queued) != 0 || s.model.n != 5 {
		t.Errorf("the last cell is %q, %d turns queued, the model asked %d times; want A5, none, 5", got[0], len(s.queued), s.model.n)
	}
}

// A fill is work its person authorised: one queued by someone deactivated
// before it runs writes nothing, and one whose person is deactivated partway
// stops at its next turn (principalBusiness.Assess, asked at each turn). A
// person the graph no longer has ends it the same way, quietly, while a
// lookup the graph didn't answer is tried again.
func TestAFillStopsWhenItsPersonIsDeactivated(t *testing.T) {
	deactivated := time.Now().Add(-time.Minute)

	t.Run("before it runs", func(t *testing.T) {
		s := newFillScene(t)
		res, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me)
		if err != nil || res.Filled != 0 || res.Left != 5 || len(s.queued) != 1 {
			t.Fatalf("the fill says %+v (%v) with %d turns queued; want all 5 left to one turn", res, err, len(s.queued))
		}
		s.person.DeletedAt = &deactivated
		s.allowed = 5
		if err := s.turn(); err != nil {
			t.Fatalf("a deactivated person's turn: %v (want it ended, not tried again)", err)
		}
		if got := s.cells(3, 8); fmt.Sprint(got) != fmt.Sprint([]string{"", "", "", "", ""}) || s.model.n != 0 || len(s.queued) != 0 {
			t.Errorf("a deactivated person's fill wrote %q, asked the model %d times and queued %d turns; want nothing", got, s.model.n, len(s.queued))
		}
	})

	t.Run("partway", func(t *testing.T) {
		s := newFillScene(t)
		s.allowed = 2
		if _, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me); err != nil {
			t.Fatal(err)
		}
		s.allowed = 1
		if err := s.turn(); err != nil {
			t.Fatalf("a turn: %v", err)
		}
		if len(s.queued) != 1 {
			t.Fatalf("the turn didn't queue the next: %+v", s.queued)
		}
		s.person.DeletedAt = &deactivated
		s.allowed = 5
		if err := s.turn(); err != nil {
			t.Fatalf("the turn after the deactivation: %v (want it ended, not tried again)", err)
		}
		if got := s.cells(3, 8); fmt.Sprint(got) != fmt.Sprint([]string{"A1", "A2", "A3", "", ""}) || s.model.n != 3 || len(s.queued) != 0 {
			t.Errorf("after the deactivation the cells are %q, the model was asked %d times and %d turns are queued; want the 3 from before, and nothing more", got, s.model.n, len(s.queued))
		}
	})

	t.Run("when the graph no longer has them", func(t *testing.T) {
		s := newFillScene(t)
		if _, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me); err != nil || len(s.queued) != 1 {
			t.Fatalf("the fill: %v, %d turns queued", err, len(s.queued))
		}
		s.lookup = dgraphUserModel.ErrNotFound
		s.allowed = 5
		if err := s.turn(); err != nil {
			t.Fatalf("the turn of a person the graph doesn't have: %v (want it ended, not tried again for two hours)", err)
		}
		if got := s.cells(3, 8); fmt.Sprint(got) != fmt.Sprint([]string{"", "", "", "", ""}) || s.model.n != 0 || len(s.queued) != 0 {
			t.Errorf("the fill of a person the graph doesn't have wrote %q, asked the model %d times and queued %d turns; want nothing", got, s.model.n, len(s.queued))
		}
	})

	t.Run("while the graph doesn't answer", func(t *testing.T) {
		s := newFillScene(t)
		if _, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me); err != nil || len(s.queued) != 1 {
			t.Fatalf("the fill: %v, %d turns queued", err, len(s.queued))
		}
		s.lookup = errors.New("dgraph: deadline exceeded")
		s.allowed = 5
		if err := s.turn(); err == nil {
			t.Fatal("a turn whose person couldn't be read said it was done; want it tried again")
		}
		if got := s.cells(3, 8); fmt.Sprint(got) != fmt.Sprint([]string{"", "", "", "", ""}) || s.model.n != 0 {
			t.Errorf("a turn whose person couldn't be read wrote %q and asked the model %d times; want nothing yet", got, s.model.n)
		}
	})
}

// A row's autofill asks for what the AI's rate allows now and leaves the rest
// of its auto columns to a turn of their own, which writes them at the
// rate's pace: the one loop a column's fill and its turns use.
func TestARowsAutofillLeavesWhatTheRateStoppedToItsTurn(t *testing.T) {
	s := newFillScene(t)
	gist, tone := s.auto("Gist"), s.auto("Tone")
	s.allowed = 1
	s.autofillRow(4)
	if got := []string{s.cell(4, gist), s.cell(4, tone)}; fmt.Sprint(got) != fmt.Sprint([]string{"A1", ""}) {
		t.Errorf("after the autofill the row's Gist and Tone are %q; want Gist filled, Tone left", got)
	}
	if len(s.queued) != 1 || s.queued[0].Key != "ai_fill:"+s.tb.Id.String()+":row:"+s.rows[4].String() || s.queued[0].Entities["fields"] != tone.Id.String() {
		t.Fatalf("Tone wasn't left to the row's turn: %+v", s.queued)
	}
	next := s.queued[0]
	if err := s.turn(); err == nil {
		t.Error("a row's turn the rate stopped before it asked anything said it was done")
	}
	s.queued = append(s.queued, next)
	s.allowed = 1
	if err := s.turn(); err != nil {
		t.Fatalf("the row's turn: %v", err)
	}
	if got := s.cell(4, tone); got != "A2" || len(s.queued) != 0 || s.model.n != 2 {
		t.Errorf("after the row's turn Tone is %q, %d turns are queued and the model was asked %d times; want A2, none, 2", got, len(s.queued), s.model.n)
	}
}

// A row's autofill is one change set of its writer's, made for them by the
// AI as a column's fill is: what it writes in the request and what its turn
// writes later go into it, and the writer's undo that names it puts the row
// back whole. It's theirs alone: another member's undo is refused. Each cell
// was a change set of its own, no one's, which no one could undo.
func TestARowsAutofillIsOneChangeSetItsWriterCanUndo(t *testing.T) {
	s := newFillScene(t)
	gist, tone := s.auto("Gist"), s.auto("Tone")
	s.allowed = 1
	s.autofillRow(4)
	s.allowed = 1
	if err := s.turn(); err != nil {
		t.Fatalf("the row's turn: %v", err)
	}
	if got := []string{s.cell(4, gist), s.cell(4, tone)}; fmt.Sprint(got) != fmt.Sprint([]string{"A1", "A2"}) {
		t.Fatalf("after the autofill and its turn the row's Gist and Tone are %q; want A1, A2", got)
	}
	sets := s.fillSets()
	if len(sets) != 1 {
		t.Fatalf("the row's autofill is %d change sets of its writer's; want 1", len(sets))
	}
	if _, err := Undo(s.w.ctx, s.tb.Id, &sets[0], s.w.member); !errors.Is(err, errNotYours) {
		t.Errorf("another member's undo of the row's autofill: %v; want it refused as not theirs", err)
	}
	if _, err := Undo(s.w.ctx, s.tb.Id, &sets[0], s.w.me); err != nil {
		t.Fatalf("the writer undoes the row's autofill: %v", err)
	}
	if got := []string{s.cell(4, gist), s.cell(4, tone)}; fmt.Sprint(got) != fmt.Sprint([]string{"", ""}) {
		t.Errorf("after the writer's undo the row's Gist and Tone are %q; want both empty again", got)
	}
}

// A row's autofill undone while its turn waits writes no more: the turn's
// write is refused (model.ErrUndone), and the turn is done, not tried again.
func TestARowsAutofillUndoneBeforeItsTurnWritesNoMore(t *testing.T) {
	s := newFillScene(t)
	gist, tone := s.auto("Gist"), s.auto("Tone")
	s.allowed = 1
	s.autofillRow(4)
	sets := s.fillSets()
	if len(sets) != 1 {
		t.Fatalf("the row's autofill is %d change sets of its writer's; want 1", len(sets))
	}
	if _, err := Undo(s.w.ctx, s.tb.Id, &sets[0], s.w.me); err != nil {
		t.Fatalf("the writer undoes the row's autofill: %v", err)
	}
	s.allowed = 1
	if err := s.turn(); err != nil {
		t.Errorf("the turn of an autofill undone: %v; want it done", err)
	}
	if got := []string{s.cell(4, gist), s.cell(4, tone)}; fmt.Sprint(got) != fmt.Sprint([]string{"", ""}) || len(s.queued) != 0 {
		t.Errorf("after the undo and the turn the row's Gist and Tone are %q with %d turns queued; want both empty, none", got, len(s.queued))
	}
}

// A turn whose minute runs out during a call still queues its next, with
// the cell it was cut off at left in it (a turn that can't queue its next
// runs again with its old budget), and a call the fill's own time cut off
// isn't held against the model's breaker.
func TestATurnCutOffByItsMinuteStillQueuesItsNext(t *testing.T) {
	s := newFillScene(t)
	s.allowed = 2
	if _, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me); err != nil || len(s.queued) != 1 {
		t.Fatalf("the fill: %v, %d turns queued", err, len(s.queued))
	}
	ctx := newCutCtx(s.w.ctx)
	s.model.cutAt, s.model.cutOff = 4, ctx.cut
	s.allowed = 5
	if err := s.turnIn(ctx); err != nil {
		t.Fatalf("a turn cut off by its minute: %v (want its next queued)", err)
	}
	if got := s.cells(5, 8); fmt.Sprint(got) != fmt.Sprint([]string{"A3", "", ""}) {
		t.Errorf("after the cut-off turn the cells are %q, want the first answered and the rest empty", got)
	}
	if len(s.queued) != 1 || s.queued[0].Entities["budget"] != "2" {
		t.Fatalf("the cut-off turn didn't queue its next of 2: %+v", s.queued)
	}
	if st := s.breaker.State(); st != "closed" {
		t.Errorf("the call the fill's own minute cut off left the model's breaker %s; want closed", st)
	}
	s.allowed = 5
	if err := s.turn(); err != nil {
		t.Fatalf("the next turn: %v", err)
	}
	if got := s.cells(5, 8); fmt.Sprint(got) != fmt.Sprint([]string{"A3", "A5", "A6"}) || len(s.queued) != 0 {
		t.Errorf("after the next turn the cells are %q with %d turns queued; want all three filled, none", got, len(s.queued))
	}
}
