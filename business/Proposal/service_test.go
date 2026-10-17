package business

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	models "github.com/akashc777/OneCamp/models/postgres/Proposal"
	"github.com/google/uuid"
)

// memStore is the table in memory, with the same claim rule.
type memStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]models.Row
	now  func() time.Time
}

func newMemStore(now func() time.Time) *memStore {
	return &memStore{rows: map[uuid.UUID]models.Row{}, now: now}
}

func (m *memStore) Insert(_ context.Context, r *models.Row) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[r.ID] = *r
	return nil
}

func (m *memStore) Get(_ context.Context, id uuid.UUID) (*models.Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (m *memStore) List(_ context.Context, kind string, res uuid.UUID, statuses []string, limit int) ([]models.Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.Row
	for _, r := range m.rows {
		for _, s := range statuses {
			if r.ResourceKind == kind && r.ResourceUUID == res && r.Status == s {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func (m *memStore) CountOpen(_ context.Context, kind string, res uuid.UUID) (int, error) {
	n := 0
	for _, r := range m.rows {
		if r.ResourceKind == kind && r.ResourceUUID == res && (r.Status == "open" || r.Status == "stale" || r.Status == "preparing") {
			n++
		}
	}
	return n, nil
}

func (m *memStore) Claim(_ context.Context, id uuid.UUID, pending json.RawMessage) (*models.Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return nil, nil
	}
	if r.PendingAt != nil && m.now().Sub(*r.PendingAt) < models.PendingTTL {
		return nil, nil
	}
	at := m.now()
	r.Pending, r.PendingAt = pending, &at
	m.rows[id] = r
	return &r, nil
}

func (m *memStore) Release(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[id]
	r.Pending, r.PendingAt = nil, nil
	m.rows[id] = r
	return nil
}

func (m *memStore) Save(_ context.Context, r *models.Row) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	saved := *r
	saved.Pending, saved.PendingAt = nil, nil
	m.rows[r.ID] = saved
	return nil
}

func (m *memStore) Delete(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, id)
	return nil
}

func (m *memStore) Sweep(context.Context, time.Duration) (int64, error) { return 0, nil }

// fakeKind decides access from a table and records the steps it is asked to run.
type fakeKind struct {
	levels map[string]accessBusiness.Level
	locked bool
	runs   []Pending
	run    func(p Proposal) error
}

func (f *fakeKind) Access(_ context.Context, who accessBusiness.Subject, _ string) (accessBusiness.Decision, error) {
	return accessBusiness.Decision{Level: f.levels[who.UserUUID], Locked: f.locked}, nil
}

func (f *fakeKind) Run(_ context.Context, _ accessBusiness.Subject, p Proposal) error {
	f.runs = append(f.runs, *p.Pending)
	if f.run != nil {
		return f.run(p)
	}
	return nil
}

type recorder struct {
	mu     sync.Mutex
	events []resourceEventBusiness.Event
}

func (r *recorder) Emit(_ context.Context, e resourceEventBusiness.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

var (
	owner     = accessBusiness.Subject{UserUID: "0x1", UserUUID: uuid.NewString()}
	editor    = accessBusiness.Subject{UserUID: "0x2", UserUUID: uuid.NewString()}
	commenter = accessBusiness.Subject{UserUID: "0x3", UserUUID: uuid.NewString()}
	viewer    = accessBusiness.Subject{UserUID: "0x4", UserUUID: uuid.NewString()}
	doc       = uuid.NewString()
)

type world struct {
	svc   *service
	kind  *fakeKind
	store *memStore
	bus   *recorder
	clock *time.Time
}

func setup(t *testing.T) *world {
	t.Helper()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	clock := &now
	tick := func() time.Time { return *clock }
	store := newMemStore(tick)
	kind := &fakeKind{levels: map[string]accessBusiness.Level{
		owner.UserUUID: accessBusiness.LevelFull, editor.UserUUID: accessBusiness.LevelEdit,
		commenter.UserUUID: accessBusiness.LevelComment, viewer.UserUUID: accessBusiness.LevelView,
	}}
	name := "test-" + uuid.NewString()
	Kinds.Register(name, kind)
	bus := &recorder{}
	old := resourceEventBusiness.Bus
	resourceEventBusiness.Bus = bus
	t.Cleanup(func() {
		resourceEventBusiness.Bus = old
		Kinds.mu.Lock()
		delete(Kinds.kinds, name)
		Kinds.mu.Unlock()
	})
	kindName = name
	return &world{svc: &service{store: store, now: tick}, kind: kind, store: store, bus: bus, clock: clock}
}

var kindName string

func hunk(id string) Hunk {
	return Hunk{ID: id, Op: "replace", Level: "inline", Path: []int{0}, From: 0, To: 4,
		Before: json.RawMessage(`[{"type":"text","text":"Thur"}]`), After: json.RawMessage(`[{"type":"text","text":"Fri"}]`)}
}

func suggestion(n int) Proposal {
	p := Proposal{
		ResourceKind: kindName, ResourceUUID: doc,
		Anchor:   threadBusiness.Anchor{Kind: threadBusiness.AnchorYRange, Start: []byte{1}, End: []byte{2}},
		BaseHash: "abc", Base: json.RawMessage(`[]`), Proposed: json.RawMessage(`[]`),
		// A client cannot claim to be someone else.
		AuthorUUID: owner.UserUUID, AuthorKind: AuthorAgent,
	}
	for i := 1; i <= n; i++ {
		p.Hunks = append(p.Hunks, hunk("h"+string(rune('0'+i))))
	}
	return p
}

func TestSuggestingIsCommentingAndAuthorshipIsTheServers(t *testing.T) {
	w := setup(t)
	ctx := context.Background()
	if _, err := w.svc.Create(ctx, viewer, suggestion(1)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a viewer suggesting: %v, want forbidden", err)
	}
	p, err := w.svc.Create(ctx, commenter, suggestion(2))
	if err != nil {
		t.Fatal(err)
	}
	if p.AuthorKind != AuthorPerson || p.AuthorUUID != commenter.UserUUID || p.Status != StatusOpen {
		t.Fatalf("a commenter's suggestion is theirs and open: %+v", p)
	}
	agent := commenter
	agent.AgentUUID = uuid.NewString()
	a, err := w.svc.Create(ctx, agent, Proposal{
		ResourceKind: kindName, ResourceUUID: doc, RunID: "run-1",
		Anchor: threadBusiness.Anchor{Kind: threadBusiness.AnchorNone}, Proposed: json.RawMessage(`[]`), Hunks: []Hunk{hunk("h1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.AuthorKind != AuthorAgent || a.AuthorUUID != agent.AgentUUID || a.AskerUUID != commenter.UserUUID || a.RunID != "run-1" {
		t.Fatalf("an agent's suggestion names the agent and its asker: %+v", a)
	}
	pathless := suggestion(1)
	pathless.Hunks[0].Path = nil
	if got, err := w.svc.Create(ctx, commenter, pathless); err != nil || got.Hunks[0].Path == nil {
		t.Fatalf("a change with no path is stored with an empty one: %v %+v", err, got.Hunks)
	}
	if len(w.bus.events) != 3 || w.bus.events[0].Kind != resourceEventBusiness.Kind(kindName+".proposal.created") {
		t.Fatalf("each suggestion emits one event: %+v", w.bus.events)
	}
}

func TestALockedResourceTakesNoSuggestions(t *testing.T) {
	w := setup(t)
	w.kind.locked = true
	if _, err := w.svc.Create(context.Background(), owner, suggestion(1)); !errors.Is(err, ErrLocked) {
		t.Fatalf("got %v, want locked", err)
	}
}

func TestShapeIsChecked(t *testing.T) {
	w := setup(t)
	ctx := context.Background()
	for name, mutate := range map[string]func(*Proposal){
		"no changes":    func(p *Proposal) { p.Hunks = nil },
		"duplicate ids": func(p *Proposal) { p.Hunks = []Hunk{hunk("h1"), hunk("h1")} },
		"bad level":     func(p *Proposal) { p.Hunks[0].Level = "word" },
		"bad range":     func(p *Proposal) { p.Hunks[0].From = 9 },
		"bad anchor":    func(p *Proposal) { p.Anchor.Kind = "page" },
		"not json":      func(p *Proposal) { p.Proposed = json.RawMessage(`{`) },
		"long note":     func(p *Proposal) { p.Note = string(make([]rune, 501)) },
		"decided hunk":  func(p *Proposal) { p.Hunks[0].Decision = DecisionAccepted },
	} {
		p := suggestion(1)
		mutate(&p)
		got, err := w.svc.Create(ctx, owner, p)
		var invalidErr *InvalidError
		if name == "decided hunk" {
			// A client cannot send a decision in: it is cleared.
			if err != nil || got.Hunks[0].Decision != "" {
				t.Errorf("%s: %v %+v", name, err, got.Hunks)
			}
			continue
		}
		if !errors.As(err, &invalidErr) {
			t.Errorf("%s: got %v, want invalid", name, err)
		}
	}
}

func TestAcceptingTwoOfThreeOnTheServer(t *testing.T) {
	w := setup(t)
	ctx := context.Background()
	p, _ := w.svc.Create(ctx, commenter, suggestion(3))
	if _, err := w.svc.Decide(ctx, commenter, p.ID, Decision{Accept: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a commenter accepting: %v", err)
	}
	got, err := w.svc.Decide(ctx, editor, p.ID, Decision{Accept: true, HunkIDs: []string{"h3", "h1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.kind.runs) != 1 || w.kind.runs[0].Mode != ModeApply || len(w.kind.runs[0].HunkIDs) != 2 || w.kind.runs[0].HunkIDs[0] != "h1" {
		t.Fatalf("the server step is one apply of h1 and h3, in order: %+v", w.kind.runs)
	}
	if got.Status != StatusOpen || got.Hunks[0].Decision != DecisionAccepted || got.Hunks[1].Decision != "" || got.Hunks[2].Decision != DecisionAccepted {
		t.Fatalf("h2 is still open: %+v", got)
	}
	got, err = w.svc.Decide(ctx, owner, p.ID, Decision{Accept: false})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPartial || got.Hunks[1].Decision != DecisionDeclined || len(w.kind.runs) != 1 {
		t.Fatalf("declining the rest makes it partial, with no step run: %+v", got)
	}
	if _, err := w.svc.Decide(ctx, owner, p.ID, Decision{Accept: true}); !errors.Is(err, ErrDecided) {
		t.Fatalf("deciding again: %v", err)
	}
	last := w.bus.events[len(w.bus.events)-1]
	if last.Kind != resourceEventBusiness.Kind(kindName+".proposal.declined") || len(last.Subjects) != 1 || last.Subjects[0] != commenter.UserUUID {
		t.Fatalf("the proposer hears of the decision: %+v", last)
	}
	// Nothing is left claimed.
	if r, _ := w.store.Get(ctx, uuid.MustParse(p.ID)); r.PendingAt != nil {
		t.Fatal("a finished decision left its claim")
	}
}

func TestAConflictChangesNothingAndAllConflictingIsStale(t *testing.T) {
	w := setup(t)
	ctx := context.Background()
	p, _ := w.svc.Create(ctx, commenter, suggestion(2))
	w.kind.run = func(Proposal) error { return &ConflictError{HunkIDs: []string{"h2"}} }
	_, err := w.svc.Decide(ctx, editor, p.ID, Decision{Accept: true})
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.HunkIDs[0] != "h2" {
		t.Fatalf("got %v", err)
	}
	got, _ := w.svc.Get(ctx, editor, p.ID)
	if got.Status != StatusOpen || got.Hunks[0].Decision != "" {
		t.Fatalf("a refused apply records nothing: %+v", got)
	}
	w.kind.run = func(Proposal) error { return &ConflictError{HunkIDs: []string{"h1", "h2"}} }
	if _, err := w.svc.Decide(ctx, editor, p.ID, Decision{Accept: true}); !errors.As(err, &conflict) {
		t.Fatal(err)
	}
	got, _ = w.svc.Get(ctx, editor, p.ID)
	if got.Status != StatusStale {
		t.Fatalf("every change conflicting makes it stale: %s", got.Status)
	}
	// What is left: insert it below.
	w.kind.run = nil
	got, err = w.svc.Decide(ctx, editor, p.ID, Decision{Accept: true, InsertBelow: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusAccepted || got.AppliedMode != "insert_below" || w.kind.runs[len(w.kind.runs)-1].Mode != ModeInsertBelow {
		t.Fatalf("inserted below: %+v", got)
	}
}

func TestAClientApplyRunsNoStep(t *testing.T) {
	w := setup(t)
	ctx := context.Background()
	p, _ := w.svc.Create(ctx, commenter, suggestion(1))
	got, err := w.svc.Decide(ctx, editor, p.ID, Decision{Accept: true, Apply: ApplyClient})
	if err != nil || got.Status != StatusAccepted || len(w.kind.runs) != 0 {
		t.Fatalf("%v %+v %+v", err, got, w.kind.runs)
	}
	// The reviewer's editor could not apply it after all: they take it back.
	if _, err := w.svc.Reopen(ctx, owner, p.ID, []string{"h1"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("someone else reopening: %v", err)
	}
	got, err = w.svc.Reopen(ctx, editor, p.ID, []string{"h1"})
	if err != nil || got.Status != StatusOpen || got.Hunks[0].Decision != "" {
		t.Fatalf("%v %+v", err, got)
	}
	_, _ = w.svc.Decide(ctx, editor, p.ID, Decision{Accept: true, Apply: ApplyClient})
	*w.clock = w.clock.Add(reopenWindow + time.Second)
	if _, err := w.svc.Reopen(ctx, editor, p.ID, []string{"h1"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reopening late: %v", err)
	}
}

func TestOneDecisionAtATime(t *testing.T) {
	w := setup(t)
	ctx := context.Background()
	p, _ := w.svc.Create(ctx, commenter, suggestion(1))
	// Another reviewer's step is in flight.
	if _, err := w.store.Claim(ctx, uuid.MustParse(p.ID), json.RawMessage(`{"mode":"apply"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Decide(ctx, owner, p.ID, Decision{Accept: true}); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want busy", err)
	}
	// A step that never finished lets go.
	*w.clock = w.clock.Add(models.PendingTTL + time.Second)
	if _, err := w.svc.Decide(ctx, owner, p.ID, Decision{Accept: true}); err != nil {
		t.Fatal(err)
	}
}

func TestWithdrawAndVisibility(t *testing.T) {
	w := setup(t)
	ctx := context.Background()
	p, _ := w.svc.Create(ctx, commenter, suggestion(1))
	stranger := accessBusiness.Subject{UserUID: "0x9", UserUUID: uuid.NewString()}
	if _, err := w.svc.Get(ctx, stranger, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone who can't see the doc: %v", err)
	}
	if _, err := w.svc.List(ctx, stranger, accessBusiness.Resource{Kind: kindName, UUID: doc}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("listing for someone who can't see the doc: %v", err)
	}
	list, err := w.svc.List(ctx, viewer, accessBusiness.Resource{Kind: kindName, UUID: doc})
	if err != nil || len(list) != 1 {
		t.Fatalf("a reader sees the suggestions: %v %d", err, len(list))
	}
	if _, err := w.svc.Withdraw(ctx, viewer, p.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a reader withdrawing: %v", err)
	}
	got, err := w.svc.Withdraw(ctx, commenter, p.ID)
	if err != nil || got.Status != StatusWithdrawn {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := w.svc.Decide(ctx, owner, p.ID, Decision{Accept: true}); !errors.Is(err, ErrDecided) {
		t.Fatalf("accepting a withdrawn suggestion: %v", err)
	}
}

func TestAServerMadeProposalIsPreparedOrDropped(t *testing.T) {
	w := setup(t)
	ctx := context.Background()
	side := collabSide{s: w.svc}
	agent := editor
	agent.AgentUUID = uuid.NewString()
	draft := Proposal{ResourceKind: kindName, ResourceUUID: doc, Draft: &Draft{HTML: "<p>Friday.</p>", Target: Target{Section: "Launch"}}}
	w.kind.run = func(p Proposal) error {
		step, err := side.Step(ctx, p.ID)
		if err != nil || step.Pending.Mode != ModePrepare || step.Pending.HTML != "<p>Friday.</p>" || step.Pending.Target.Section != "Launch" {
			t.Fatalf("the op reads the step: %v %+v", err, step.Pending)
		}
		return side.Prepared(ctx, p.ID, Prepared{
			Anchor: threadBusiness.Anchor{Kind: threadBusiness.AnchorYRange, Start: []byte{1}, End: []byte{2}},
			Base:   json.RawMessage(`[]`), BaseHash: "h", Proposed: json.RawMessage(`[]`), Hunks: []Hunk{hunk("h1")},
		})
	}
	got, err := w.svc.Create(ctx, agent, draft)
	if err != nil || got.Status != StatusOpen || len(got.Hunks) != 1 || got.AuthorKind != AuthorAgent {
		t.Fatalf("%v %+v", err, got)
	}
	// A prepare that fails leaves nothing behind.
	w.kind.run = func(Proposal) error { return ErrNotFound }
	if _, err := w.svc.Create(ctx, agent, draft); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if n, _ := w.store.CountOpen(ctx, kindName, uuid.MustParse(doc)); n != 1 {
		t.Fatalf("the failed one was kept: %d open", n)
	}
}
