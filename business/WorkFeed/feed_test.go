package business

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
)

// fakeLog stands in for task_changes and a project's topic in one test.
type fakeLog struct {
	next       int64 // the sequence the next stored change takes
	rows       []Row
	writes     int
	failWrite  error
	published  []Entry
	hidden     map[int64]bool // rows this reader may not see
	failSight  error
	oldest     int64 // the oldest sequence still kept
	askedLimit int
}

func swapIn(t *testing.T, l *fakeLog) {
	t.Helper()
	a, r, h, m, p, c := appendRows, rowsAfter, headAndOldest, mayRead, publishOne, Changes
	t.Cleanup(func() { appendRows, rowsAfter, headAndOldest, mayRead, publishOne, Changes = a, r, h, m, p, c })
	appendRows = func(_ context.Context, cs []Change) ([]int64, error) {
		l.writes++
		if l.failWrite != nil {
			return nil, l.failWrite
		}
		seqs := make([]int64, len(cs))
		for i, c := range cs {
			seqs[i] = l.next
			l.rows = append(l.rows, Row{Sequence: l.next, Change: c})
			l.next++
		}
		return seqs, nil
	}
	rowsAfter = func(_ context.Context, after int64, limit int) ([]Row, error) {
		l.askedLimit = limit
		var out []Row
		for _, r := range l.rows {
			if r.Sequence > after && len(out) < limit {
				out = append(out, r)
			}
		}
		return out, nil
	}
	headAndOldest = func(context.Context) (int64, int64, error) {
		if len(l.rows) == 0 {
			return 0, 0, nil
		}
		return l.rows[len(l.rows)-1].Sequence, l.oldest, nil
	}
	mayRead = func(_ context.Context, _ accessBusiness.Subject, rows []Row) (map[int64]bool, error) {
		if l.failSight != nil {
			return nil, l.failSight
		}
		ok := map[int64]bool{}
		for _, r := range rows {
			ok[r.Sequence] = !l.hidden[r.Sequence]
		}
		return ok, nil
	}
	publishOne = func(_ context.Context, e Entry) { l.published = append(l.published, e) }
	Changes = Log
}

var reader = accessBusiness.Subject{UserUUID: "reader"}

func change(task, kind string) Change {
	return Change{Kind: kind, ResourceKind: "task", ResourceUUID: task, ProjectUUID: "p1",
		Diff: json.RawMessage(`{"task_priority":"high"}`), Actor: "u1", Source: "app",
		At: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
}

// Each change is numbered in the order it was made, answered with what it
// changed, and told to its project's open views once.
func TestRecordNumbersEachChangeInOrderAndPublishesItOnce(t *testing.T) {
	l := &fakeLog{next: 41}
	swapIn(t, l)
	got := Record(context.Background(), change("t1", "task.updated"), change("t2", "task.deleted"))
	if len(got) != 2 || got[0].Sequence != 41 || got[1].Sequence != 42 {
		t.Fatalf("entries %+v, want 41 then 42", got)
	}
	e := got[0]
	if e.Kind != "task.updated" || e.ResourceKind != "task" || e.ResourceUUID != "t1" || e.ProjectUUID != "p1" ||
		string(e.Diff) != `{"task_priority":"high"}` || e.Actor != "u1" || e.Source != "app" || !e.At.Equal(change("", "").At) {
		t.Errorf("entry %+v doesn't carry its change", e)
	}
	if !reflect.DeepEqual(l.published, got) {
		t.Errorf("published %+v, want each entry once, in order: %+v", l.published, got)
	}
}

// A log that can't take the change never fails the write that made it, and
// nothing the log didn't keep is told to anyone. No changes, no write.
func TestRecordNeverFailsTheWrite(t *testing.T) {
	l := &fakeLog{next: 1, failWrite: errors.New("the database is gone")}
	swapIn(t, l)
	if got := Record(context.Background(), change("t1", "task.updated")); len(got) != 0 {
		t.Errorf("entries %+v from a log that kept nothing", got)
	}
	if len(l.published) != 0 {
		t.Errorf("published %+v that the log never kept", l.published)
	}
	l.failWrite = nil
	l.writes = 0
	Record(context.Background())
	if l.writes != 0 {
		t.Errorf("%d writes for no changes", l.writes)
	}
}

// A reader catches up on what they may see, in order, and their place moves
// past what they may not, so they never ask for it again.
func TestSinceAnswersOnlyWhatTheReaderMaySee(t *testing.T) {
	l := &fakeLog{next: 5, hidden: map[int64]bool{6: true}}
	swapIn(t, l)
	Record(context.Background(), change("t1", "task.updated"), change("secret", "task.updated"), change("t2", "task.created"))
	got, next, err := Changes.Since(context.Background(), reader, "4", 100)
	if err != nil || len(got) != 2 || got[0].Sequence != 5 || got[1].Sequence != 7 || next != "7" {
		t.Fatalf("got %+v next %q err %v, want 5 and 7, next 7", got, next, err)
	}
	l.hidden = map[int64]bool{5: true, 6: true, 7: true}
	got, next, err = Changes.Since(context.Background(), reader, "4", 100)
	if err != nil || len(got) != 0 || next != "7" {
		t.Errorf("got %+v next %q err %v, want nothing and next 7", got, next, err)
	}
}

// A reader with no place yet gets the log's head and nothing else: they've
// just read their lists whole.
func TestSinceFromNowhereAnswersTheHead(t *testing.T) {
	l := &fakeLog{next: 9}
	swapIn(t, l)
	Record(context.Background(), change("t1", "task.updated"))
	got, next, err := Changes.Since(context.Background(), reader, "", 100)
	if err != nil || len(got) != 0 || next != "9" {
		t.Errorf("got %+v next %q err %v, want the head 9", got, next, err)
	}
}

// A place older than the log keeps is refused, so the reader reads its lists
// again instead of missing what was pruned; a place that isn't one is refused.
func TestSinceRefusesAPlaceTheLogNoLongerHas(t *testing.T) {
	l := &fakeLog{next: 10, oldest: 10}
	swapIn(t, l)
	Record(context.Background(), change("t1", "task.updated"), change("t2", "task.updated"))
	if _, _, err := Changes.Since(context.Background(), reader, "3", 100); !errors.Is(err, ErrCursorExpired) {
		t.Errorf("err %v, want ErrCursorExpired", err)
	}
	if got, _, err := Changes.Since(context.Background(), reader, "9", 100); err != nil || len(got) != 2 {
		t.Errorf("got %+v err %v: 9 is just before the oldest kept", got, err)
	}
	for _, bad := range []string{"abc", "-1", "1.5"} {
		if _, _, err := Changes.Since(context.Background(), reader, Cursor(bad), 100); !errors.Is(err, ErrBadCursor) {
			t.Errorf("%q: err %v, want ErrBadCursor", bad, err)
		}
	}
}

// One answer holds at most MaxSince entries, whatever is asked.
func TestSinceCapsTheAnswer(t *testing.T) {
	l := &fakeLog{next: 1}
	swapIn(t, l)
	Record(context.Background(), change("t1", "task.updated"))
	for _, asked := range []int{0, -1, MaxSince + 1} {
		if _, _, err := Changes.Since(context.Background(), reader, "0", asked); err != nil || l.askedLimit != MaxSince {
			t.Errorf("asked %d: read %d (err %v), want %d", asked, l.askedLimit, err, MaxSince)
		}
	}
	if _, _, _ = Changes.Since(context.Background(), reader, "0", 2); l.askedLimit != 2 {
		t.Errorf("asked 2, read %d", l.askedLimit)
	}
}

// When who may see what can't be read, nothing is answered and the reader
// keeps their place: the feed fails closed. Until business/Task says who may
// see what, it is closed.
func TestSinceFailsClosed(t *testing.T) {
	l := &fakeLog{next: 1, failSight: errors.New("the graph is gone")}
	swapIn(t, l)
	Record(context.Background(), change("t1", "task.updated"))
	if got, next, err := Changes.Since(context.Background(), reader, "0", 100); err == nil || len(got) != 0 || next != "0" {
		t.Errorf("got %+v next %q err %v, want an error and place 0", got, next, err)
	}
	mayRead = closedSight
	if _, _, err := Changes.Since(context.Background(), reader, "0", 100); !errors.Is(err, ErrNoSight) {
		t.Errorf("err %v, want ErrNoSight before business/Task registers", err)
	}
}

// A place past the newest change is a log rewound (a database restored from
// a backup): the reader reads its lists again rather than wait, missing
// everything, until the log passes its place. The head itself is a place.
func TestSinceRefusesAPlacePastTheHead(t *testing.T) {
	l := &fakeLog{next: 1}
	swapIn(t, l)
	Record(context.Background(), change("t1", "task.updated"), change("t2", "task.updated"))
	if _, _, err := Changes.Since(context.Background(), reader, "7", 100); !errors.Is(err, ErrCursorExpired) {
		t.Errorf("past the head: err %v, want ErrCursorExpired", err)
	}
	if got, next, err := Changes.Since(context.Background(), reader, "2", 100); err != nil || len(got) != 0 || next != "2" {
		t.Errorf("at the head: got %+v next %q err %v, want nothing new", got, next, err)
	}
}

// A prune that takes changes just before they are read never makes the
// reader step over them: the place is checked after the read, and is gone.
func TestSinceNeverStepsOverWhatAPruneTookMeanwhile(t *testing.T) {
	l := &fakeLog{next: 1}
	swapIn(t, l)
	Record(context.Background(), change("t1", "task.updated"), change("t2", "task.updated"),
		change("t3", "task.updated"), change("t4", "task.updated"))
	read := rowsAfter
	rowsAfter = func(ctx context.Context, after int64, limit int) ([]Row, error) {
		l.rows, l.oldest = l.rows[3:], 4 // the prune took 1 to 3 first
		return read(ctx, after, limit)
	}
	if got, _, err := Changes.Since(context.Background(), reader, "1", 100); !errors.Is(err, ErrCursorExpired) {
		t.Errorf("got %+v err %v: 2 and 3 went before the read; want ErrCursorExpired", got, err)
	}
}

// Telling a batch reads no restriction again: each change came with its
// batch's one read (RecordTasks). An open task's change is told one by one
// with the task marked open (helpers.WithOpenTasks), so business/Mqtt's rule
// reads nothing; a restricted task's is told as its project's one Reread,
// naming nothing, as that rule tells it, and never one by one.
func TestRecordTellsWhatItKnowsWithoutReadingAgain(t *testing.T) {
	l := &fakeLog{next: 1}
	swapIn(t, l)
	saved := publishReread
	t.Cleanup(func() { publishReread = saved })
	var reread []string
	publishReread = func(_ context.Context, project string) { reread = append(reread, project) }
	var markedOpen []bool
	publishOne = func(ctx context.Context, e Entry) {
		l.published = append(l.published, e)
		markedOpen = append(markedOpen, helpers.TasksOpen(ctx, e.ResourceUUID))
	}
	const open, kept, other = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333"
	restricted := change(kept, "task.updated")
	restricted.Restricted = true
	elsewhere := change(other, "task.created")
	elsewhere.ProjectUUID = "p2"
	Record(context.Background(), change(open, "task.updated"), restricted, restricted, elsewhere)
	if len(l.published) != 2 || l.published[0].ResourceUUID != open || l.published[1].ResourceUUID != other ||
		!reflect.DeepEqual(markedOpen, []bool{true, true}) {
		t.Errorf("told one by one %+v, marked open %v; want the two open tasks, each marked open", l.published, markedOpen)
	}
	if !reflect.DeepEqual(reread, []string{"p1"}) {
		t.Errorf("told to read again %v; want p1 once, for its restricted task", reread)
	}
}
