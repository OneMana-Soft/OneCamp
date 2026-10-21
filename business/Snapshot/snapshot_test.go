package business

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	snapshotModels "github.com/akashc777/OneCamp/models/postgres/DocSnapshot"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Fakes: the store, the ledger, the idle marks, the access rules and the live
// port, so the rules are tested without Postgres, Redis, MinIO or the graph.
// ---------------------------------------------------------------------------

type memStore struct {
	mu     sync.Mutex
	rows   map[uuid.UUID]*snapshotModels.DocSnapshot
	bodies map[uuid.UUID]string
}

func newMemStore() *memStore {
	return &memStore{rows: map[uuid.UUID]*snapshotModels.DocSnapshot{}, bodies: map[uuid.UUID]string{}}
}

func (m *memStore) put(_ context.Context, row *snapshotModels.DocSnapshot, body string) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row.ID = uuid.New()
	row.StoredBytes = len(body) / 3
	cp := *row
	m.rows[row.ID] = &cp
	m.bodies[row.ID] = body
	return row.ID, nil
}

func (m *memStore) sorted(docUUID string) []*snapshotModels.DocSnapshot {
	var out []*snapshotModels.DocSnapshot
	for _, r := range m.rows {
		if r.DocUUID == docUUID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (m *memStore) latest(_ context.Context, docUUID string) (*snapshotModels.DocSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if all := m.sorted(docUUID); len(all) > 0 {
		return all[0], nil
	}
	return nil, nil
}

func (m *memStore) list(_ context.Context, docUUID string, before time.Time, limit int) ([]*snapshotModels.DocSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.sorted(docUUID)
	if !before.IsZero() {
		var older []*snapshotModels.DocSnapshot
		for _, r := range all {
			if r.CreatedAt.Before(before) {
				older = append(older, r)
			}
		}
		all = older
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (m *memStore) get(_ context.Context, docUUID string, id uuid.UUID) (*snapshotModels.DocSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rows[id]; r != nil && r.DocUUID == docUUID {
		return r, nil
	}
	return nil, nil
}

func (m *memStore) body(_ context.Context, docUUID string, id uuid.UUID) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.rows[id]; r != nil && r.DocUUID == docUUID {
		return m.bodies[id], nil
	}
	return "", ErrNotFound
}

func (m *memStore) setName(_ context.Context, docUUID string, id uuid.UUID, name *string, pinned bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[id]
	if r == nil || r.DocUUID != docUUID {
		return false, nil
	}
	r.Name, r.Pinned = name, pinned
	return true, nil
}

func (m *memStore) keptBytes(_ context.Context, docUUID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, r := range m.sorted(docUUID) {
		if r.Pinned || r.Name != nil {
			n += int64(r.StoredBytes)
		}
	}
	return n, nil
}

func (m *memStore) retention(_ context.Context, docUUID string) ([]snapshotModels.RetentionRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []snapshotModels.RetentionRow
	for _, r := range m.sorted(docUUID) {
		out = append(out, snapshotModels.RetentionRow{ID: r.ID, Kind: r.Kind, Kept: r.Pinned || r.Name != nil, StoredBytes: r.StoredBytes, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

func (m *memStore) remove(_ context.Context, ids []uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		if r := m.rows[id]; r != nil && !r.Pinned && r.Name == nil {
			delete(m.rows, id)
			delete(m.bodies, id)
		}
	}
	return nil
}

func (m *memStore) docsBefore(_ context.Context, cutoff time.Time, after string, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, r := range m.rows {
		if r.CreatedAt.Before(cutoff) && !seen[r.DocUUID] && r.DocUUID > after {
			seen[r.DocUUID] = true
			out = append(out, r.DocUUID)
		}
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type memLedger struct {
	mu   sync.Mutex
	sets map[string][]Contributor
}

func (l *memLedger) Add(_ context.Context, kind, id string, who []Contributor) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sets[kind+id] = append(l.sets[kind+id], who...)
}

func (l *memLedger) Members(_ context.Context, kind, id string) []Contributor {
	l.mu.Lock()
	defer l.mu.Unlock()
	return sortContributors(append([]Contributor(nil), l.sets[kind+id]...))
}

func (l *memLedger) Drain(_ context.Context, kind, id string) []Contributor {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := sortContributors(l.sets[kind+id])
	delete(l.sets, kind+id)
	return out
}

type memMarks struct {
	mu sync.Mutex
	at map[string]int64
}

func (m *memMarks) Mark(_ context.Context, _ string, id string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.at[id] = at.UnixMilli()
}

func (m *memMarks) Due(_ context.Context, _ string, before time.Time, n int) []Mark {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Mark
	for id, at := range m.at {
		if at <= before.UnixMilli() {
			out = append(out, Mark{UUID: id, At: at})
		}
	}
	return out
}

func (m *memMarks) Claim(_ context.Context, _ string, mk Mark) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if at, ok := m.at[mk.UUID]; ok && at == mk.At {
		delete(m.at, mk.UUID)
		return true
	}
	return false
}

type rulesFake struct {
	accessBusiness.Resolver
	level   accessBusiness.Level
	deleted bool
	owner   bool
}

func (r rulesFake) Resolve(context.Context, accessBusiness.Subject, accessBusiness.Resource) (accessBusiness.Decision, error) {
	return accessBusiness.Decision{Level: r.level, Deleted: r.deleted, Owner: r.owner}, nil
}

// world swaps every fake in, with a clock the test moves.
type world struct {
	store *memStore
	clock time.Time
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{store: newMemStore(), clock: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	prevStore, prevLedger, prevMarks, prevNow, prevRules, prevBody := store, ledger, marks, now, accessBusiness.Rules, storedBody
	store = w.store
	ledger = &memLedger{sets: map[string][]Contributor{}}
	marks = &memMarks{at: map[string]int64{}}
	now = func() time.Time { return w.clock }
	accessBusiness.Rules = rulesFake{level: accessBusiness.LevelEdit}
	t.Cleanup(func() {
		store, ledger, marks, now, accessBusiness.Rules, storedBody = prevStore, prevLedger, prevMarks, prevNow, prevRules, prevBody
	})
	return w
}

// liveFake is the collaboration service's live port: it holds one doc's body,
// answers reads and replaceAll like the service (the body before, for
// snapshot_first), and records what it was asked.
type liveFake struct {
	mu    sync.Mutex
	body  string
	calls []map[string]any
}

func (f *liveFake) start(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		defer f.mu.Unlock()
		req["_op"] = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.calls = append(f.calls, req)
		data := map[string]any{"blocks": 1, "persisted": true}
		switch req["_op"] {
		case "read":
			data = map[string]any{"html": f.body, "lineage": "L", "open": true}
		case "replaceAll", "append":
			if req["snapshot_first"] == true {
				data["before"] = map[string]any{"html": f.body, "lineage": "L"}
			}
			if req["_op"] == "replaceAll" {
				f.body = req["html"].(string)
			} else {
				f.body += req["html"].(string)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"msg": "ok", "data": data})
	}))
	t.Cleanup(srv.Close)
	t.Setenv(collab.LiveURLEnv, srv.URL)
	t.Setenv("INTERNAL_SECRET", "k")
}

const docID = "6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f"

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

// A version someone named or pinned is never pruned, however many come after it
// and however old it is; the rest thin out by the tiers.
func TestNamedAndPinnedVersionsSurviveFiftyNewer(t *testing.T) {
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	now := start.Add(60 * 24 * time.Hour)
	named, pinned := uuid.New(), uuid.New()
	rows := []planRow{
		{ID: named, Kept: true, Bytes: 10, CreatedAt: start},
		{ID: pinned, Kept: true, Bytes: 10, CreatedAt: start.Add(time.Minute)},
	}
	for i := 0; i < 50; i++ {
		rows = append(rows, planRow{ID: uuid.New(), Bytes: 10, CreatedAt: start.Add(time.Duration(i+1) * 26 * time.Hour)})
	}
	drop := planPrune(rows, now, Policy{Retention: 30 * 24 * time.Hour, MaxBytes: 1 << 20, KeepNewest: 3})
	for _, id := range drop {
		if id == named || id == pinned {
			t.Fatal("a named or pinned version was pruned")
		}
	}
	if len(drop) == 0 {
		t.Fatal("nothing past the retention period was pruned")
	}
}

func TestVersionsThinByTier(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	p := Policy{Retention: 30 * 24 * time.Hour, MaxBytes: 1 << 30, KeepNewest: 3}
	mk := func(age time.Duration, agent bool) planRow {
		return planRow{ID: uuid.New(), AgentRun: agent, Bytes: 1, CreatedAt: now.Add(-age)}
	}
	today := []planRow{mk(time.Minute, false), mk(2*time.Minute, false), mk(3*time.Minute, false)}
	// Two days ago, three in one hour: the newest of them stays.
	hour := []planRow{mk(48*time.Hour+10*time.Minute, false), mk(48*time.Hour+20*time.Minute, false), mk(48*time.Hour+30*time.Minute, false)}
	// An agent run's versions stay whole for the week.
	runs := []planRow{mk(72*time.Hour+time.Minute, true), mk(72*time.Hour+2*time.Minute, true)}
	// Ten days ago, three in one day: the newest stays.
	day := []planRow{mk(10*24*time.Hour+time.Hour, false), mk(10*24*time.Hour+2*time.Hour, false), mk(10*24*time.Hour+3*time.Hour, false)}
	old := []planRow{mk(40*24*time.Hour, false)}
	var all []planRow
	for _, g := range [][]planRow{today, hour, runs, day, old} {
		all = append(all, g...)
	}
	dropped := map[uuid.UUID]bool{}
	for _, id := range planPrune(all, now, p) {
		dropped[id] = true
	}
	want := map[uuid.UUID]bool{hour[1].ID: true, hour[2].ID: true, day[1].ID: true, day[2].ID: true, old[0].ID: true}
	for _, r := range all {
		if dropped[r.ID] != want[r.ID] {
			t.Errorf("version %s old: dropped=%v, want %v", now.Sub(r.CreatedAt), dropped[r.ID], want[r.ID])
		}
	}
}

// Past the byte cap the oldest versions nobody kept go first; the newest few
// and the kept ones never do.
func TestTheByteCapTakesTheOldestUnkeptFirst(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	var rows []planRow
	for i := 0; i < 10; i++ {
		rows = append(rows, planRow{ID: uuid.New(), Bytes: 100, CreatedAt: now.Add(-time.Duration(i) * time.Minute), Kept: i == 9})
	}
	drop := planPrune(rows, now, Policy{Retention: 30 * 24 * time.Hour, MaxBytes: 450, KeepNewest: 3})
	if len(drop) != 6 {
		t.Fatalf("dropped %d, want 6 (450 bytes hold 4 of 10, one of them kept)", len(drop))
	}
	for _, id := range drop {
		for i, r := range rows {
			if r.ID == id && (i < 3 || r.Kept) {
				t.Errorf("dropped version %d (newest few or kept)", i)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The ledger and the version policy on save
// ---------------------------------------------------------------------------

// A version credits everyone who edited since the version before it, an agent
// with the person who asked it, each once.
func TestTheLedgerCreditsEveryoneBetweenTwoVersions(t *testing.T) {
	w := newWorld(t)
	save := func(body string, people []string, agents ...docBusiness.AgentContributor) {
		onDocSave(context.Background(), docBusiness.Change{DocUUID: docID, Body: body, Contributors: people, Agents: agents, SavedAt: w.clock})
	}
	save("<p>1</p>", []string{"ana"}) // the first save takes a version
	w.clock = w.clock.Add(time.Minute)
	save("<p>2</p>", []string{"ben"})
	w.clock = w.clock.Add(time.Minute)
	save("<p>3</p>", nil, docBusiness.AgentContributor{AgentUUID: "captain", AskerUUID: "cara"})
	w.clock = w.clock.Add(time.Minute)
	save("<p>4</p>", []string{"ben"})

	// Two minutes later, idle: one version for the three saves since.
	w.clock = w.clock.Add(idleAfter + time.Second)
	storedBody = func(context.Context, string) (string, error) { return "<p>4</p>", nil }
	if err := takeIdleVersions(context.Background()); err != nil {
		t.Fatal(err)
	}
	all, _ := w.store.list(context.Background(), docID, time.Time{}, 10)
	if len(all) != 2 {
		t.Fatalf("%d versions, want 2", len(all))
	}
	first, idle := all[1], all[0]
	if first.Reason != snapshotModels.ReasonInterval || len(first.Contributors) != 1 || first.Contributors[0].UserUUID != "ana" {
		t.Errorf("first version %s %+v", first.Reason, first.Contributors)
	}
	want := []snapshotModels.Contributor{{UserUUID: "ben"}, {AgentUUID: "captain", AskerUUID: "cara"}}
	if idle.Reason != snapshotModels.ReasonIdle || len(idle.Contributors) != 2 || idle.Contributors[0] != want[0] || idle.Contributors[1] != want[1] {
		t.Errorf("idle version %s %+v, want %+v", idle.Reason, idle.Contributors, want)
	}
	if strings.Join(idle.ContributorUUIDs, ",") != "ben,cara" {
		t.Errorf("people credited %v", idle.ContributorUUIDs)
	}
	// Taken once: a second instance finds nothing to take.
	if err := takeIdleVersions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again, _ := w.store.list(context.Background(), docID, time.Time{}, 10); len(again) != 2 {
		t.Errorf("the idle version was taken twice")
	}
}

// A save that deletes most of a doc keeps what it was.
func TestAMassDeletionKeepsTheBodyBefore(t *testing.T) {
	w := newWorld(t)
	onDocSave(context.Background(), docBusiness.Change{DocUUID: docID, Body: "<p>x</p>", Previous: "<p>" + strings.Repeat("everything ", 100) + "</p>", Contributors: []string{"ana"}, SavedAt: w.clock})
	all, _ := w.store.list(context.Background(), docID, time.Time{}, 10)
	if len(all) != 1 || all[0].Reason != snapshotModels.ReasonMassDelete || !strings.Contains(w.store.bodies[all[0].ID], "everything") {
		t.Fatalf("versions %+v", all)
	}
}

// ---------------------------------------------------------------------------
// Agent runs, restore, access
// ---------------------------------------------------------------------------

// An agent's write keeps the body it found as an agent_run version, through the
// Live Doc Writer; Revert (a restore of it) puts back exactly that body, live,
// and keeps the agent's version of the body first.
func TestAnAgentRunsVersionAndRevertRestoreExactly(t *testing.T) {
	w := newWorld(t)
	live := &liveFake{body: "<h1>Launch</h1><p>Ship on Thursday.</p>"}
	live.start(t)
	before := live.body

	r, err := collab.Live.ReplaceAll(context.Background(), docID, "<h1>Launch</h1><p>The agent's rewrite.</p>", collab.Options{
		By: collab.By{UserUUID: "sam", AgentUUID: "captain"}, AgentEdit: true, RunID: "run-7", SnapshotFirst: true,
	})
	if err != nil || r.VersionID == "" {
		t.Fatalf("agent write: %+v %v", r, err)
	}
	row, _ := w.store.get(context.Background(), docID, uuid.MustParse(r.VersionID))
	if row.Kind != snapshotModels.KindAgentRun || row.RunID != "run-7" || row.Reason != snapshotModels.ReasonBeforeWrite || w.store.bodies[row.ID] != before {
		t.Fatalf("agent run version %+v body %q", row, w.store.bodies[row.ID])
	}
	if len(row.Contributors) != 1 || row.Contributors[0] != (snapshotModels.Contributor{AgentUUID: "captain", AskerUUID: "sam"}) {
		t.Errorf("the run's version names %+v", row.Contributors)
	}

	who := accessBusiness.Subject{UserUUID: "sam", UserUID: "0x1"}
	w.clock = w.clock.Add(time.Minute)
	if err := Snapshots.Restore(context.Background(), who, docKind, docID, r.VersionID); err != nil {
		t.Fatal(err)
	}
	if live.body != before {
		t.Fatalf("after Revert the doc is %q, want %q", live.body, before)
	}
	last := live.calls[len(live.calls)-1]
	if last["_op"] != "replaceAll" || last["snapshot_first"] != true || last["html"] != before {
		t.Errorf("revert asked %v", last)
	}
	// The agent's text was kept first, as the version before the restore.
	all, _ := w.store.list(context.Background(), docID, time.Time{}, 10)
	if len(all) != 2 || all[0].Reason != snapshotModels.ReasonManual || !strings.Contains(w.store.bodies[all[0].ID], "The agent's rewrite.") {
		t.Errorf("versions after revert: %+v", all)
	}
}

// Seeing, naming and restoring versions takes edit access; a doc in the trash
// keeps its history there.
func TestVersionsTakeEditAccess(t *testing.T) {
	newWorld(t)
	who := accessBusiness.Subject{UserUUID: "u"}
	for _, c := range []struct {
		rules rulesFake
		want  error
	}{
		{rulesFake{level: accessBusiness.LevelNone}, ErrNotFound},
		{rulesFake{level: accessBusiness.LevelView}, ErrForbidden},
		{rulesFake{level: accessBusiness.LevelComment}, ErrForbidden},
		{rulesFake{level: accessBusiness.LevelFull, deleted: true}, ErrInTrash},
		// In the trash, today's resolver gives no level: its owner is told,
		// anyone else finds nothing.
		{rulesFake{level: accessBusiness.LevelNone, deleted: true, owner: true}, ErrInTrash},
		{rulesFake{level: accessBusiness.LevelNone, deleted: true}, ErrNotFound},
	} {
		accessBusiness.Rules = c.rules
		if _, err := Snapshots.List(context.Background(), who, docKind, docID, time.Time{}); !errors.Is(err, c.want) {
			t.Errorf("level %v deleted %v: %v, want %v", c.rules.level, c.rules.deleted, err, c.want)
		}
		if err := Snapshots.Restore(context.Background(), who, docKind, docID, uuid.NewString()); !errors.Is(err, c.want) {
			t.Errorf("restore at level %v: %v", c.rules.level, err)
		}
	}
	accessBusiness.Rules = rulesFake{level: accessBusiness.LevelEdit}
	if _, err := Snapshots.List(context.Background(), who, docKind, docID, time.Time{}); err != nil {
		t.Errorf("an editor: %v", err)
	}
	if _, err := Snapshots.List(context.Background(), who, "calendar", docID, time.Time{}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("an unknown kind: %v", err)
	}
}

// Naming and pinning keep a version within the doc's byte cap.
func TestNamingKeepsAVersionWithinTheCap(t *testing.T) {
	w := newWorld(t)
	t.Setenv("DOC_SNAPSHOT_MAX_MB", "1")
	who := accessBusiness.Subject{UserUUID: "u"}
	small, _ := keepDocVersion(context.Background(), docID, "<p>small</p>", keepAs{reason: snapshotModels.ReasonSaved, kind: KindDoc})
	if err := Snapshots.Name(context.Background(), who, docKind, docID, small.ID, "Launch draft", true); err != nil {
		t.Fatal(err)
	}
	got, _ := w.store.get(context.Background(), docID, uuid.MustParse(small.ID))
	if got.Name == nil || *got.Name != "Launch draft" || !got.Pinned {
		t.Errorf("named %+v", got)
	}
	big, _ := keepDocVersion(context.Background(), docID, strings.Repeat("x", 4<<20), keepAs{reason: snapshotModels.ReasonSaved, kind: KindDoc})
	if err := Snapshots.Name(context.Background(), who, docKind, docID, big.ID, "Too big", false); !errors.Is(err, ErrKeptFull) {
		t.Errorf("past the cap: %v", err)
	}
	if err := Snapshots.Name(context.Background(), who, docKind, docID, small.ID, strings.Repeat("n", 121), false); !errors.Is(err, ErrBadName) {
		t.Errorf("a long name: %v", err)
	}
}

// A version taken on request reads the live doc, typing included.
func TestATakenVersionReadsTheLiveDoc(t *testing.T) {
	w := newWorld(t)
	live := &liveFake{body: "<p>Typed a moment ago.</p>"}
	live.start(t)
	who := accessBusiness.Subject{UserUUID: "u"}
	v, err := Snapshots.Take(context.Background(), Take{ResourceKind: docKind, ResourceUUID: docID, Name: "Before the review", Who: &who, By: Contributor{UserUUID: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "Before the review" || v.Reason != snapshotModels.ReasonSaved || w.store.bodies[uuid.MustParse(v.ID)] != "<p>Typed a moment ago.</p>" {
		t.Errorf("version %+v", v)
	}
	accessBusiness.Rules = rulesFake{level: accessBusiness.LevelView}
	if _, err := Snapshots.Take(context.Background(), Take{ResourceKind: docKind, ResourceUUID: docID, Who: &who}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a reader took a version: %v", err)
	}
}
