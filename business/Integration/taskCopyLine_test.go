package Integration

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// logLines is the task sync's log as a test reads it.
type logLines struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// with is every line that holds word.
func (l *logLines) with(word string) (out []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range strings.Split(l.b.String(), "\n") {
		if strings.Contains(line, word) {
			out = append(out, line)
		}
	}
	return out
}

func captureLogs(t *testing.T) *logLines {
	t.Helper()
	prev := helpers.MessageLogs
	l := &logLines{}
	lg := log.New(l, "", 0)
	helpers.MessageLogs = &helpers.Message{InfoLog: lg, ErrorLog: lg}
	t.Cleanup(func() { helpers.MessageLogs = prev })
	return l
}

// holdDeletes makes person's DELETEs wait: entered closes when the first one
// arrives, and each goes on once release is closed.
func (w *reassignWorld) holdDeletes(t *testing.T, person uuid.UUID) (entered, release chan struct{}) {
	t.Helper()
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	c := w.cals[person]
	c.svc = fakeGoogle(t, func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			once.Do(func() { close(entered) })
			<-release
		}
		c.serve(rw, r)
	})
	return entered, release
}

// passEntered waits for a held DELETE to reach Google, as archiveEnds waits
// for a pass's end: one that never comes fails t instead of hanging the package.
func passEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never sent its DELETE")
	}
}

func (w *reassignWorld) assign(to uuid.UUID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.assignee = to
}

// ranBeside is whether the pass behind done finished while another pass on the
// same task was still in flight; one that waits its turn is still waiting.
func ranBeside(done <-chan error) (err error, beside bool) {
	select {
	case err = <-done:
		return err, true
	case <-time.After(200 * time.Millisecond):
		return nil, false
	}
}

func await(t *testing.T, what string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never ended", what)
	}
}

func (w *reassignWorld) holdsOneTrackedCopy(t *testing.T, who string, person uuid.UUID, when string) {
	t.Helper()
	if _, events := w.cals[person].snapshot(); len(events) != 1 || w.storedID() != events[0] {
		t.Errorf("%s %s's calendar holds %v and the row keeps %q, want one copy whose id the row keeps", when, who, events, w.storedID())
	}
}

// CR-369 (a): A -> B, then B -> C while A's DELETE is still in flight. The
// second reassign waits its turn; C ends with one copy, and C's edit adds none.
func TestCR369TwoQuickReassignsKeepCsOneCopy(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "a-refresh", true, "evA")
	b := w.person(t, "b", "b-refresh", true)
	c := w.person(t, "c", "c-refresh", true)
	w.handedOver(b, "evA")
	entered, release := w.holdDeletes(t, a)

	first := MoveTaskGoogleCopy(context.Background(), w.task.String(), a.String(), b.String())
	passEntered(t, entered)
	w.assign(c)
	second := MoveTaskGoogleCopy(context.Background(), w.task.String(), b.String(), c.String())
	err, beside := ranBeside(second)
	if beside {
		t.Errorf("B -> C finished (err %v) while A's DELETE was in flight, want it to wait its turn", err)
	}
	close(release)
	await(t, "A -> B", first)
	if !beside {
		await(t, "B -> C", second)
	}
	w.holdsOneTrackedCopy(t, "C", c, "after A->B->C")
	if err := SyncTaskToGoogleCalendar(context.Background(), w.task.String(), c.String()); err != nil {
		t.Fatal(err)
	}
	w.holdsOneTrackedCopy(t, "C", c, "after C's edit")
	if _, bEvents := w.cals[b].snapshot(); len(bEvents) != 0 {
		t.Errorf("B's calendar holds %v, want none", bEvents)
	}
}

// CR-369 (b): B edits the task while the A -> B hand-over's DELETE is still in
// flight. The edit's sync waits for the hand-over; B ends with one copy.
func TestCR369EditDuringHandOverKeepsBsOneCopy(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "a-refresh", true, "evA")
	b := w.person(t, "b", "b-refresh", true)
	w.handedOver(b, "evA")
	entered, release := w.holdDeletes(t, a)

	move := MoveTaskGoogleCopy(context.Background(), w.task.String(), a.String(), b.String())
	passEntered(t, entered)
	edit := make(chan error, 1)
	go func() { edit <- SyncTaskToGoogleCalendar(context.Background(), w.task.String(), b.String()) }()
	err, beside := ranBeside(edit)
	if beside {
		t.Errorf("B's edit synced (err %v) while A's DELETE was in flight, want it to wait for the hand-over", err)
	}
	close(release)
	await(t, "A -> B", move)
	if !beside {
		await(t, "B's edit", edit)
	}
	w.holdsOneTrackedCopy(t, "B", b, "after the edit during the hand-over")
}

// CR-368 N1: A -> B, then straight back B -> A, the correction's goroutine
// scheduled first. The hand-overs run in the order they were asked for: A ends
// with exactly one tracked copy, no untracked evA, and A's next edit adds none.
func TestReassignThereAndBackRunsInTheOrderAsked(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "a-refresh", true, "evA")
	b := w.person(t, "b", "b-refresh", true)
	w.handedOver(b, "evA")
	entered, release := w.holdDeletes(t, a)

	first := MoveTaskGoogleCopy(context.Background(), w.task.String(), a.String(), b.String())
	passEntered(t, entered)
	w.assign(a)
	back := MoveTaskGoogleCopy(context.Background(), w.task.String(), b.String(), a.String())
	err, beside := ranBeside(back)
	if beside {
		t.Errorf("B -> A finished (err %v) before A -> B, want the order asked", err)
	}
	close(release)
	await(t, "A -> B", first)
	if !beside {
		await(t, "B -> A", back)
	}
	w.holdsOneTrackedCopy(t, "A", a, "after A->B->A")
	if err := SyncTaskToGoogleCalendar(context.Background(), w.task.String(), a.String()); err != nil {
		t.Fatal(err)
	}
	w.holdsOneTrackedCopy(t, "A", a, "after A's edit")
	if _, bEvents := w.cals[b].snapshot(); len(bEvents) != 0 {
		t.Errorf("B's calendar holds %v, want none", bEvents)
	}
}

// Two server instances share no line: the second runs B -> C to the end while
// the first's A -> B DELETE is in flight. The first's late clear finds C's id
// in the row and leaves it, so C keeps one tracked copy.
func TestTwoServersReassigningAtOnceKeepCsCopy(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "a-refresh", true, "evA")
	b := w.person(t, "b", "b-refresh", true)
	c := w.person(t, "c", "c-refresh", true)
	w.handedOver(b, "evA")
	entered, release := w.holdDeletes(t, a)

	first := MoveTaskGoogleCopy(context.Background(), w.task.String(), a.String(), b.String())
	passEntered(t, entered)
	w.assign(c)
	ownLine := taskCopyLine
	taskCopyLine = &helpers.InOrder{} // the other instance's
	second := MoveTaskGoogleCopy(context.Background(), w.task.String(), b.String(), c.String())
	taskCopyLine = ownLine
	await(t, "B -> C on the other instance", second)
	close(release)
	await(t, "A -> B", first)

	w.holdsOneTrackedCopy(t, "C", c, "after both instances' reassigns")
	if err := SyncTaskToGoogleCalendar(context.Background(), w.task.String(), c.String()); err != nil {
		t.Fatal(err)
	}
	w.holdsOneTrackedCopy(t, "C", c, "after C's edit")
}

// A Google DELETE that never answers holds the hand-over only as long as one
// call's bound: the move ends, names the copy left behind, and B gets theirs.
func TestMoveTaskGoogleCopyEndsWhenGoogleHangs(t *testing.T) {
	w := newReassignWorld(t)
	a := w.person(t, "a", "a-refresh", true, "evA")
	b := w.person(t, "b", "b-refresh", true)
	w.handedOver(b, "evA")
	logs := captureLogs(t)
	prev := googleCallTimeout
	googleCallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { googleCallTimeout = prev })
	hung := make(chan struct{})
	c := w.cals[a]
	c.svc = fakeGoogle(t, func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			select {
			case <-hung:
			case <-r.Context().Done():
			}
			return
		}
		c.serve(rw, r)
	})
	t.Cleanup(func() { close(hung) })

	select {
	case err := <-MoveTaskGoogleCopy(context.Background(), w.task.String(), a.String(), b.String()):
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the hand-over still waits on a hung Google DELETE after 3s, want it to end within one call's bound (%v)", googleCallTimeout)
	}
	if lines := logs.with("orphan"); len(lines) != 1 || !strings.Contains(lines[0], "evA") {
		t.Errorf("the log names an orphan in %q, want one line naming evA", lines)
	}
	w.holdsOneTrackedCopy(t, "B", b, "after the hung DELETE")
}
