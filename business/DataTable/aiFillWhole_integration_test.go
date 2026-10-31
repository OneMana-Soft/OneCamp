//go:build integration

package business

// An AI column's fill takes the whole column (TABLES-LEAD-26, 24 Oct): a
// fill took at most 200 cells, so a column of thousands was filled 200 at a
// time, each asked for again; and each cell the AI wrote was a change set of
// its own, no one's, so no one could undo a fill. Now one fill takes every
// empty cell (up to 10,000), in turns that walk the table once, asks first
// past 500 cells, can be stopped, and is one change set its person can undo.
//
// Run: integ_rel.sh <tree> <log> '^(TestAFill|TestARowsAutofill|TestATurn|TestAStoppedFill|TestAnUndoOfARunningFill)' with PKGS='./business/DataTable/'

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// failingAnswers answers each cell with the next of B1, B2, ..., and fails
// every call whose number is one of failAt, as a model can fail a cell.
type failingAnswers struct {
	mu     sync.Mutex
	n      int
	failAt map[int]bool
}

func (a *failingAnswers) Chat(context.Context, []ai.ChatMessage, ai.ChatOptions) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	if a.failAt[a.n] {
		return "", fmt.Errorf("the model failed call %d", a.n)
	}
	return fmt.Sprintf("B%d", a.n), nil
}

func (a *failingAnswers) ChatStream(context.Context, []ai.ChatMessage, ai.ChatOptions) (<-chan ai.StreamChunk, error) {
	return nil, fmt.Errorf("not streamed")
}

func (a *failingAnswers) ProviderName() ai.ProviderType { return "test" }

// withRows adds n rows to the scene's table, after its eight, each with a
// note and an empty Summary.
func (s *fillScene) withRows(n int) {
	s.t.Helper()
	for done := 0; done < n; {
		var in BatchInput
		for i := 0; i < min(n-done, maxBatch); i++ {
			in.Create = append(in.Create, RowInput{Values: map[string]interface{}{s.notes.Id.String(): fmt.Sprintf("note %d", 8+done+i)}})
		}
		res, err := Batch(s.w.ctx, s.tb.Id, in, s.w.me)
		if err != nil {
			s.t.Fatalf("add rows: %v", err)
		}
		for _, r := range res.Created {
			s.rows = append(s.rows, r.Id)
		}
		done += len(in.Create)
	}
}

// fillSets is the change sets of AI writes on the scene's table that are
// the person's: those their undo can name.
func (s *fillScene) fillSets() []uuid.UUID {
	s.t.Helper()
	rs, err := postgresInit.DBConn.SqlDB.QueryContext(s.w.ctx, `SELECT id FROM data_table_change_sets
		WHERE table_id = $1 AND kind = 'ai' AND actor = $2 ORDER BY at`, s.tb.Id, s.w.me.UserID)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rs.Close()
	var out []uuid.UUID
	for rs.Next() {
		var id uuid.UUID
		if err := rs.Scan(&id); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// turns runs the turns queued, as the outbox's sink does, until none is
// left, at most limit of them; it answers how many ran.
func (s *fillScene) turns(limit int) int {
	s.t.Helper()
	n := 0
	for len(s.queued) > 0 {
		if n++; n > limit {
			s.t.Fatalf("still turning after %d turns", limit)
		}
		if err := s.turn(); err != nil {
			s.t.Fatalf("turn %d: %v", n, err)
		}
	}
	return n
}

func TestAFillTakesTheWholeColumnInTurns(t *testing.T) {
	s := newFillScene(t)
	s.withRows(1000)
	failing := &failingAnswers{failAt: map[int]bool{7: true, 333: true, 900: true}}
	wasFiller := fillerFor
	fillerFor = func(ctx context.Context, who string) (filler, bool) {
		f, ok := wasFiller(ctx, who)
		f.llm = failing
		return f, ok
	}
	s.allowed = 40
	res, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1005 || res.More != 0 || res.Filled != 39 || res.Skipped != 1 || res.Left != 965 {
		t.Errorf("the fill says %+v; want all 1005 empty cells taken, 40 asked now (one failed) and 965 left", *res)
	}
	s.allowed = 1 << 30
	turns := s.turns(40)
	if turns < 10 {
		t.Errorf("the rest took %d turns; a turn reads at most %d rows", turns, aiFillPage)
	}
	if failing.n != 1005 {
		t.Errorf("the model was asked %d times for 1005 cells: a cell it failed was asked again, or one was missed", failing.n)
	}
	empty := 0
	for i := 3; i < len(s.rows); i++ {
		if s.cell(i, s.summary) == "" {
			empty++
		}
	}
	if empty != 3 {
		t.Errorf("%d cells are empty after the fill; want the 3 the model failed", empty)
	}
	if got := s.cells(0, 3); fmt.Sprint(got) != fmt.Sprint([]string{"earlier", "mine", ""}) {
		t.Errorf("the fill wrote over an answer or a person's cell: %q", got)
	}

	sets := s.fillSets()
	if len(sets) != 1 {
		t.Fatalf("the fill is %d change sets of the person's; want one they can undo", len(sets))
	}
	undo, err := Undo(s.w.ctx, s.tb.Id, &sets[0], s.w.me)
	if err != nil {
		t.Fatalf("undo the fill: %v", err)
	}
	if undo.Changes != 1002 || undo.Skipped != 0 {
		t.Errorf("the undo reversed %d and skipped %d; want the 1002 cells the fill wrote", undo.Changes, undo.Skipped)
	}
	for i := 3; i < len(s.rows); i += 97 {
		if v := s.cell(i, s.summary); v != "" {
			t.Errorf("row %d's cell is %q after the undo", i, v)
		}
	}
	if got := s.cells(0, 3); fmt.Sprint(got) != fmt.Sprint([]string{"earlier", "mine", ""}) {
		t.Errorf("the undo changed an answer or a person's cell: %q", got)
	}
}

func TestAFillAsksFirstPastFiveHundredCells(t *testing.T) {
	s := newFillScene(t)
	s.withRows(600)
	s.allowed = 3
	no, yes := false, true
	res, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{Confirm: &no}, s.w.me)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NeedsConfirm || res.Total != 605 || res.Fill != nil || s.model.n != 0 || len(s.queued) != 0 {
		t.Fatalf("a fill of 605 cells not confirmed says %+v, asked %d, queued %d; want it to ask first and do nothing", *res, s.model.n, len(s.queued))
	}
	res, err = FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{Confirm: &yes}, s.w.me)
	if err != nil {
		t.Fatal(err)
	}
	if res.NeedsConfirm || res.Fill == nil || res.Filled != 3 || res.Left != 602 || len(s.queued) != 1 {
		t.Fatalf("the confirmed fill says %+v, queued %d", *res, len(s.queued))
	}
	again, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{Confirm: &yes}, s.w.me)
	if err != nil {
		t.Fatal(err)
	}
	if again.Fill == nil || again.Fill.ID != res.Fill.ID || len(s.queued) != 1 || s.model.n != 3 {
		t.Errorf("a second fill while one runs started another (%+v, %d queued, %d asked); want the running one", again.Fill, len(s.queued), s.model.n)
	}
	legacy, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{Most: LegacyAIFillCells}, s.w.me)
	if err != nil || legacy.Fill == nil || legacy.Fill.ID != res.Fill.ID {
		t.Errorf("a web from before asked while the fill runs: %+v, %v", legacy, err)
	}
}

func TestAStoppedFillEndsAtItsNextCell(t *testing.T) {
	s := newFillScene(t)
	s.withRows(50)
	s.allowed = 4
	res, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me)
	if err != nil || res.Fill == nil {
		t.Fatalf("%+v, %v", res, err)
	}
	if _, err := StopAIFill(s.w.ctx, s.tb.Id, s.summary.Id, s.w.member); !errors.Is(err, errForbidden) {
		t.Errorf("a member who neither started the fill nor manages the table stopped it: %v", err)
	}
	stopped, err := StopAIFill(s.w.ctx, s.tb.Id, s.summary.Id, s.w.me)
	if err != nil || stopped == nil || stopped.State != model.AIFillStopped || stopped.Asked != 4 {
		t.Fatalf("stop: %+v, %v", stopped, err)
	}
	s.allowed = 1 << 30
	s.turns(2)
	if s.model.n != 4 {
		t.Errorf("the model was asked %d times; a stopped fill's turn asked on", s.model.n)
	}
	if fills, err := AIFills(s.w.ctx, s.tb.Id, s.w.member); err != nil || len(fills) != 1 || fills[0].State != model.AIFillStopped {
		t.Errorf("the table's fills, as a member reads them: %+v, %v", fills, err)
	}
	if again, err := StopAIFill(s.w.ctx, s.tb.Id, s.summary.Id, s.w.me); err != nil || again != nil {
		t.Errorf("stopping a column with no fill running: %+v, %v", again, err)
	}
}

func TestAnUndoOfARunningFillStopsItFirst(t *testing.T) {
	s := newFillScene(t)
	s.withRows(50)
	s.allowed = 10
	res, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me)
	if err != nil || res.ChangeSet == nil {
		t.Fatalf("%+v, %v", res, err)
	}
	undo, err := Undo(s.w.ctx, s.tb.Id, res.ChangeSet, s.w.me)
	if err != nil || undo.Changes != 10 {
		t.Fatalf("undo a running fill: %+v, %v", undo, err)
	}
	fill, _ := model.GetAIFill(s.w.ctx, s.tb.Id, res.Fill.ID)
	if fill == nil || fill.State != model.AIFillStopped {
		t.Errorf("the fill after its undo: %+v; want it stopped", fill)
	}
	s.allowed = 1 << 30
	s.turns(2)
	if s.model.n != 10 {
		t.Errorf("a turn asked the model on after the fill's undo: %d calls", s.model.n)
	}
	// A write into the undone change set is refused, as a turn already
	// asking when the undo came would find.
	sum := s.summary.Id.String()
	_, err = model.WriteAICell(s.w.ctx, s.tb.Id, s.rows[20], sum, `{"`+sum+`":"late"}`, nil, "", false, 100*1024,
		&model.ChangeSet{ID: *res.ChangeSet, TableID: s.tb.Id, Append: true, ActorKind: "automation", Kind: "ai"}, nil)
	if !errors.Is(err, model.ErrUndone) || s.cell(20, s.summary) != "" {
		t.Errorf("a write into an undone fill: %v, cell %q", err, s.cell(20, s.summary))
	}
}

// A turn queued by a server from before fills took the whole column has no
// fill: it takes up to its budget of the column's empty cells, as it did.
func TestAFillsTurnFromBeforeStillTakesItsBudget(t *testing.T) {
	s := newFillScene(t)
	s.allowed = 1 << 30
	s.queued = append(s.queued, aiFillEntry(aiFillJob{table: s.tb.Id, field: s.summary.Id, budget: 2, by: s.w.me}))
	if err := s.turn(); err != nil {
		t.Fatal(err)
	}
	if got := s.cells(3, 6); fmt.Sprint(got) != fmt.Sprint([]string{"A1", "A2", ""}) || len(s.queued) != 0 {
		t.Errorf("an old turn of 2 wrote %q and queued %d", got, len(s.queued))
	}
}

// A fill whose turns stopped coming (its outbox event tried until it was
// dead) holds its column for an hour at most: past that it isn't shown as
// running, and the next fill stops it and starts.
func TestAStaleFillDoesNotHoldItsColumn(t *testing.T) {
	s := newFillScene(t)
	s.withRows(20)
	s.allowed = 2
	first, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me)
	if err != nil || first.Fill == nil || first.Fill.State != model.AIFillRunning {
		t.Fatalf("%+v, %v", first, err)
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(s.w.ctx, `UPDATE data_table_ai_fills SET updated_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, first.Fill.ID); err != nil {
		t.Fatal(err)
	}
	if fills, err := AIFills(s.w.ctx, s.tb.Id, s.w.me); err != nil || len(fills) != 0 {
		t.Errorf("a stale fill is still shown: %+v, %v", fills, err)
	}
	s.allowed = 2
	second, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me)
	if err != nil || second.Fill == nil || second.Fill.ID == first.Fill.ID || second.Filled != 2 {
		t.Fatalf("a fill after a stale one: %+v, %v", second, err)
	}
	if old, _ := model.GetAIFill(s.w.ctx, s.tb.Id, first.Fill.ID); old == nil || old.State != model.AIFillStopped {
		t.Errorf("the stale fill is %+v; want it stopped", old)
	}
}

// TestAFillAsksEachCellOnceAroundRowsWithNoKey: a row with no key yet (made
// by a server from before row keys, until the server's first look keys it)
// has no place in the table's order for a fill's cursor to walk past. A fill
// whose turn ended on one went back to the start, so every cell the model had
// failed was asked again (CR-32's SF1). A fill now leaves such a row out.
func TestAFillAsksEachCellOnceAroundRowsWithNoKey(t *testing.T) {
	s := newFillScene(t)
	s.withRows(96) // 101 empty cells: the scene's 5 and these 96
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(s.w.ctx, `UPDATE data_table_rows SET position_key = NULL WHERE id IN ($1, $2)`, s.rows[100], s.rows[101]); err != nil {
		t.Fatal(err)
	}
	failAll := map[int]bool{}
	for i := 1; i <= 2000; i++ {
		failAll[i] = true
	}
	failing := &failingAnswers{failAt: failAll}
	wasFiller := fillerFor
	fillerFor = func(ctx context.Context, who string) (filler, bool) {
		f, ok := wasFiller(ctx, who)
		f.llm = failing
		return f, ok
	}
	s.allowed = 1 << 30
	res, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.me)
	if err != nil {
		t.Fatal(err)
	}
	// Its total is what it walks (main, 25 Oct): never 101 ending at 99.
	if res.Total != 99 {
		t.Errorf("the fill's total is %d; want the 99 cells it takes, not the 2 rows with no key", res.Total)
	}
	s.turns(20)
	if failing.n != 99 {
		t.Errorf("the model was asked %d times; want each of the 99 empty cells of keyed rows once, and none of the 2 rows with no key", failing.n)
	}
	if res.Fill != nil {
		if f, err := model.GetAIFill(s.w.ctx, s.tb.Id, res.Fill.ID); err != nil || f == nil || f.State != model.AIFillDone || f.Asked != f.Total {
			t.Errorf("the fill ended as %+v, %v; want done, with all of its total asked", f, err)
		}
	}
}

// TestAFillsStarterStopsItAndNoOtherMemberDoes: the person who started a
// fill may stop it, though they don't manage the table; a member who neither
// started it nor manages the table may not. The server checks who started it.
func TestAFillsStarterStopsItAndNoOtherMemberDoes(t *testing.T) {
	s := newFillScene(t)
	s.withRows(50)
	other := Actor{UserID: uuid.New()}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(s.w.ctx, `INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'other@example.test', 'other', 'other', NOW(), NOW())`, other.UserID); err != nil {
		t.Fatal(err)
	}
	s.allowed = 4
	res, err := FillAIColumnWith(s.w.ctx, s.tb.Id, s.summary.Id, FillInput{}, s.w.member)
	if err != nil || res.Fill == nil {
		t.Fatalf("a member starts a fill: %+v, %v", res, err)
	}
	if res.Fill.StartedBy == nil || *res.Fill.StartedBy != s.w.member.UserID {
		t.Fatalf("the fill says it was started by %v; want the member", res.Fill.StartedBy)
	}
	if _, err := StopAIFill(s.w.ctx, s.tb.Id, s.summary.Id, other); !errors.Is(err, errForbidden) {
		t.Errorf("another member, who neither started the fill nor manages the table, stopped it: %v", err)
	}
	stopped, err := StopAIFill(s.w.ctx, s.tb.Id, s.summary.Id, s.w.member)
	if err != nil || stopped == nil || stopped.State != model.AIFillStopped {
		t.Fatalf("the member who started the fill stops it: %+v, %v", stopped, err)
	}
}

// TestATablesFillsAreReadOnlyByWhoeverMayOpenIt: a table's fills (GET
// /tables/{id}/ai-fills) are its readers'; a member who can't open a private
// table is refused and told nothing of its fills.
func TestATablesFillsAreReadOnlyByWhoeverMayOpenIt(t *testing.T) {
	w := newRelationWorld(t)
	private, err := CreateTable(w.ctx, TableInput{Name: "Salaries", Visibility: "private"}, w.me)
	if err != nil {
		t.Fatal(err)
	}
	if fills, err := AIFills(w.ctx, private.Id, w.me); err != nil || len(fills) != 0 {
		t.Fatalf("its owner reads the fills of a table with none: %v, %v", fills, err)
	}
	if fills, err := AIFills(w.ctx, private.Id, w.member); !errors.Is(err, errForbidden) || fills != nil {
		t.Errorf("a member who can't open the table read its fills: %v, %v", fills, err)
	}
}
