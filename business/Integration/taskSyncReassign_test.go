package Integration

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	integrationModel "github.com/akashc777/OneCamp/models/postgres/Integration"
	models "github.com/akashc777/OneCamp/models/postgres/Task"
	"github.com/google/uuid"
	"google.golang.org/api/calendar/v3"
)

// personCal is one person's Google calendar: the events in it and every
// call it was asked, as "DELETE <id>", "PATCH <id>" or "POST".
type personCal struct {
	mu           sync.Mutex
	prefix       string
	events       map[string]bool
	calls        []string
	n            int
	deleteStatus int // a DELETE answers this instead (401, 403, ...) when set; -1 drops the connection
	svc          *calendar.Service
}

func (c *personCal) serve(w http.ResponseWriter, r *http.Request) {
	const base = "/calendars/primary/events"
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, base), "/")
	c.mu.Lock()
	if id == "" && r.Method == http.MethodPost {
		c.n++
		id = fmt.Sprintf("%s%d", c.prefix, c.n)
		c.events[id] = true
		c.calls = append(c.calls, "POST")
		c.mu.Unlock()
		fmt.Fprintf(w, `{"id":%q}`, id)
		return
	}
	c.calls = append(c.calls, r.Method+" "+id)
	held := c.events[id]
	refused := c.deleteStatus
	if held && r.Method == http.MethodDelete && refused == 0 {
		delete(c.events, id)
	}
	c.mu.Unlock()
	if r.Method == http.MethodDelete && refused == -1 {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
		return
	}
	if r.Method == http.MethodDelete && refused != 0 {
		w.WriteHeader(refused)
		fmt.Fprintf(w, `{"error":{"code":%d,"message":"refused","errors":[{"reason":"forbidden"}]}}`, refused)
		return
	}
	if !held {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":404,"message":"Not Found"}}`))
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	fmt.Fprintf(w, `{"id":%q}`, id)
}

func (c *personCal) snapshot() (calls []string, events []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for e := range c.events {
		events = append(events, e)
	}
	return append([]string(nil), c.calls...), events
}

// reassignWorld is a task handed from A to B: the one Google id the task's
// row keeps, the person its graph entry names, each person's link to Google
// and each person's calendar. The task sync's reads and writes go to it.
type reassignWorld struct {
	mu       sync.Mutex
	task     uuid.UUID
	googleID *string
	assignee uuid.UUID
	links    map[uuid.UUID]*integrationModel.Integration
	cals     map[uuid.UUID]*personCal
	// stores counts the reads and writes a pass makes (row, graph, link, set,
	// clear); ctxs are the contexts it made them with, in order.
	stores map[string]int
	ctxs   []context.Context
}

// touched is one store call a pass made with ctx.
func (w *reassignWorld) touched(ctx context.Context, store string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stores[store]++
	w.ctxs = append(w.ctxs, ctx)
}

// storeCalls is how many reads and writes each store has had so far.
func (w *reassignWorld) storeCalls() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]int{}
	for store, n := range w.stores {
		out[store] = n
	}
	return out
}

func newReassignWorld(t *testing.T) *reassignWorld {
	t.Helper()
	w := &reassignWorld{task: uuid.New(), links: map[uuid.UUID]*integrationModel.Integration{}, cals: map[uuid.UUID]*personCal{}, stores: map[string]int{}}
	if helpers.MessageLogs == nil {
		discard := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
		t.Cleanup(func() { helpers.MessageLogs = nil })
	}
	prevLink, prevRow, prevGraph, prevSet, prevCal := integrationOf, taskRowOf, taskGraphOf, setTaskGoogleID, calendarOfLink
	prevClear := clearTaskGoogleID
	t.Cleanup(func() {
		integrationOf, taskRowOf, taskGraphOf, setTaskGoogleID, calendarOfLink = prevLink, prevRow, prevGraph, prevSet, prevCal
		clearTaskGoogleID = prevClear
	})
	integrationOf = func(ctx context.Context, _ string, id uuid.UUID, _ string) (*integrationModel.Integration, error) {
		w.touched(ctx, "link")
		return w.links[id], nil
	}
	taskRowOf = func(ctx context.Context, id uuid.UUID) (*models.Task, error) {
		w.touched(ctx, "row")
		w.mu.Lock()
		defer w.mu.Unlock()
		row := &models.Task{Id: id}
		if w.googleID != nil {
			g := *w.googleID
			row.TaskGoogleCalendarId = &g
		}
		return row, nil
	}
	taskGraphOf = func(ctx context.Context, id string, _ string) (*dgraphStruct.DgraphTask, error) {
		w.touched(ctx, "graph")
		w.mu.Lock()
		defer w.mu.Unlock()
		due := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		return &dgraphStruct.DgraphTask{Uuid: id, Name: "Payroll for Q3", DueDate: &due,
			Assignee: &dgraphStruct.DgraphUser{Uuid: w.assignee.String()}}, nil
	}
	setTaskGoogleID = func(ctx context.Context, _ uuid.UUID, id *string) error {
		w.touched(ctx, "set")
		w.mu.Lock()
		defer w.mu.Unlock()
		w.googleID = id
		return nil
	}
	// The row's id goes only while it is still the one named, as the UPDATE's
	// WHERE has it.
	clearTaskGoogleID = func(ctx context.Context, _ uuid.UUID, was string) error {
		w.touched(ctx, "clear")
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.googleID != nil && *w.googleID == was {
			w.googleID = nil
		}
		return nil
	}
	calendarOfLink = func(_ context.Context, link *integrationModel.Integration) (*calendar.Service, error) {
		return w.cals[link.EntityId].svc, nil
	}
	return w
}

// person adds someone with a calendar holding events; token "" is a link with
// no refresh token (disconnected, or Google refused it).
func (w *reassignWorld) person(t *testing.T, prefix, token string, taskSync bool, events ...string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	link := &integrationModel.Integration{EntityId: id, TaskSyncEnabled: taskSync}
	if token != "" {
		link.RefreshToken = &token
	}
	w.links[id] = link
	c := &personCal{prefix: prefix, events: map[string]bool{}}
	for _, e := range events {
		c.events[e] = true
	}
	c.svc = fakeGoogle(t, c.serve)
	w.cals[id] = c
	return id
}

func (w *reassignWorld) storedID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.googleID == nil {
		return ""
	}
	return *w.googleID
}

// handedOver is the task as a reassign leaves it: its graph names to, its row
// still keeps from's event id.
func (w *reassignWorld) handedOver(to uuid.UUID, fromEvent string) {
	w.assignee = to
	w.googleID = &fromEvent
}

// move is the reassign door's one call: the task goes from from to to.
func (w *reassignWorld) move(t *testing.T, from, to uuid.UUID) {
	t.Helper()
	name := func(id uuid.UUID) string {
		if id == uuid.Nil {
			return ""
		}
		return id.String()
	}
	if err := <-MoveTaskGoogleCopy(context.Background(), w.task.String(), name(from), name(to)); err != nil {
		t.Fatal(err)
	}
}

// V-1114 (a): A switched task sync off but is still linked; the task moves to
// B. A's copy of the task, title and all, comes out of A's calendar, through
// A's own link: our copy, not A's calendar.
func TestReassignRemovesTheOldAssigneesCopyWhenTheirTaskSyncIsOff(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "a-refresh", false, "evA")
	b := w.person(t, "b", "", false)
	w.handedOver(b, "evA")

	w.move(t, a, b)
	calls, events := w.cals[a].snapshot()
	if len(calls) != 1 || calls[0] != "DELETE evA" || len(events) != 0 {
		t.Fatalf("A's calendar was asked %v and still holds %v, want one DELETE evA and nothing left", calls, events)
	}
	if got := w.storedID(); got != "" {
		t.Errorf("the task still keeps %q after A's copy went", got)
	}
}

// V-1114 (b), recorded (V-1116): A's link has no refresh token (disconnected,
// or Google refused it). Nothing can reach A's calendar, so A's copy stays,
// and once B's copy is written over the task's one id, nothing remembers A's.
// This passes on purpose: no queue (MAIN's ruling, 16 Nov).
func TestReassignFromAnUnlinkedOwnerToday(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "", true, "evA")
	b := w.person(t, "b", "b-refresh", true)
	w.handedOver(b, "evA")

	w.move(t, a, b)
	aCalls, aEvents := w.cals[a].snapshot()
	if len(aCalls) != 0 || len(aEvents) != 1 || aEvents[0] != "evA" {
		t.Fatalf("A's calendar: asked %v, holds %v; recorded: no call, evA stays", aCalls, aEvents)
	}
	if got := w.storedID(); got != "b1" {
		t.Fatalf("the task keeps %q; recorded: B's b1, so evA is kept nowhere", got)
	}
}

// V-1114 (c): B's sync never sends A's event id to B's own calendar; B gets
// a fresh copy without first asking for one that was never B's.
func TestReassignNewAssigneeNeverAsksForTheOldOwnersEventId(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "a-refresh", false, "evA")
	b := w.person(t, "b", "b-refresh", true)
	w.handedOver(b, "evA")

	w.move(t, a, b)
	calls, _ := w.cals[b].snapshot()
	if len(calls) != 1 || calls[0] != "POST" {
		t.Fatalf("B's calendar was asked %v, want only one POST: A's evA is not B's to patch", calls)
	}
}

// V-1114 (c), the race: both linked with task sync on. A's removal and B's
// copy once ran side by side on the task's one id, and A's clear could land
// after B's new id, so B's next edit made B a second copy. In order, B's
// edit patches the one copy B has.
func TestReassignRaceLeavesTheNewAssigneeOneCopy(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "a-refresh", true, "evA")
	b := w.person(t, "b", "b-refresh", true)
	w.handedOver(b, "evA")

	w.move(t, a, b)
	// B edits the task.
	if err := SyncTaskToGoogleCalendar(context.Background(), w.task.String(), b.String()); err != nil {
		t.Fatal(err)
	}
	if _, events := w.cals[b].snapshot(); len(events) != 1 {
		t.Fatalf("B's calendar holds %v after an edit, want one copy (the task keeps %q)", events, w.storedID())
	}
}

// V-1114 (c), the other order: had B's copy landed first, A's removal would
// read B's id and ask A's calendar for B's event, leaving A's own copy.
func TestReassignOldOwnerRemovesTheirOwnCopyAfterTheNewOne(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "a-refresh", true, "evA")
	b := w.person(t, "b", "b-refresh", true)
	w.handedOver(b, "evA")

	w.move(t, a, b)
	calls, events := w.cals[a].snapshot()
	if len(events) != 0 || len(calls) != 1 || calls[0] != "DELETE evA" {
		t.Fatalf("A's calendar was asked %v and still holds %v, want one DELETE evA and evA gone", calls, events)
	}
}

// An answer from A's calendar that no retry changes (401: the link expired or
// was revoked; 403: it may no longer touch the calendar; 404: the copy isn't
// there) ends the removal: asked once, never again, and the id is forgotten,
// so B still gets one copy of their own and never sees A's id.
func TestMoveTaskGoogleCopyEndsAtAnAnswerNoRetryChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int // 0 the DELETE works, -1 the connection drops
		orphan bool
	}{
		{"success", 0, false}, {"404", 404, false}, {"410", 410, false},
		{"401", 401, true}, {"403", 403, true},
		{"500", 500, true}, {"429", 429, true}, {"network", -1, true},
	} {
		status := tc.status
		t.Run(tc.name, func(t *testing.T) {
			w := newReassignWorld(t)
			logs := captureLogs(t)
			a := w.person(t, "a", "a-refresh", false, "evA")
			b := w.person(t, "b", "b-refresh", true)
			w.cals[a].deleteStatus = status
			w.handedOver(b, "evA")

			w.move(t, a, b)
			if calls, _ := w.cals[a].snapshot(); len(calls) != 1 || calls[0] != "DELETE evA" {
				t.Fatalf("A's calendar was asked %v, want one DELETE evA and no retry", calls)
			}
			if calls, _ := w.cals[b].snapshot(); len(calls) != 1 || calls[0] != "POST" {
				t.Fatalf("B's calendar was asked %v, want only one POST", calls)
			}
			if got := w.storedID(); got != "b1" {
				t.Fatalf("the task keeps %q, want B's b1", got)
			}
			// A copy left in A's calendar is named once, as an orphan (V-1116).
			lines := logs.with("orphan")
			if want := map[bool]int{true: 1, false: 0}[tc.orphan]; len(lines) != want || (want == 1 && !strings.Contains(lines[0], "evA")) {
				t.Fatalf("the log names an orphan in %q, want %d line(s) naming evA", lines, want)
			}
		})
	}
}

// Every hand-over the reassign door makes: the old copy goes and the new
// person, if any, holds exactly one.
func TestMoveTaskGoogleCopyByHandOver(t *testing.T) {
	for _, tc := range []struct {
		name            string
		aHas, bHas      []string
		stored          string
		fromA, toB      bool
		wantA, wantB    int
		wantB0, wantSet string
	}{
		{name: "reassign A to B", aHas: []string{"evA"}, stored: "evA", fromA: true, toB: true, wantA: 0, wantB: 1, wantSet: "b1"},
		{name: "unassign A", aHas: []string{"evA"}, stored: "evA", fromA: true, wantA: 0, wantB: 0, wantSet: ""},
		{name: "assign B from nobody", toB: true, wantB: 1, wantSet: "b1"},
		{name: "B again", bHas: []string{"b0"}, stored: "b0", toB: true, wantB: 1, wantB0: "b0", wantSet: "b0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newReassignWorld(t)
			a := w.person(t, "a", "a-refresh", false, tc.aHas...)
			b := w.person(t, "b", "b-refresh", true, tc.bHas...)
			from, to := uuid.Nil, uuid.Nil
			if tc.fromA {
				from = a
			}
			if tc.toB {
				to = b
			}
			w.assignee = to
			if tc.stored != "" {
				w.handedOver(to, tc.stored)
			}
			if tc.name == "B again" {
				from = b
			}

			w.move(t, from, to)
			if _, ev := w.cals[a].snapshot(); len(ev) != tc.wantA {
				t.Errorf("A's calendar holds %v, want %d", ev, tc.wantA)
			}
			if _, ev := w.cals[b].snapshot(); len(ev) != tc.wantB || (tc.wantB0 != "" && ev[0] != tc.wantB0) {
				t.Errorf("B's calendar holds %v, want %d", ev, tc.wantB)
			}
			if got := w.storedID(); got != tc.wantSet {
				t.Errorf("the task keeps %q, want %q", got, tc.wantSet)
			}
		})
	}
}
