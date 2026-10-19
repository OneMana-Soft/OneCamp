package Calendar

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	notificationPreferenceModel "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/google/uuid"
)

// testMover is a mover whose move waits for release, counting its attempts.
func testMover(release chan struct{}, fail *atomic.Bool, now *time.Time) (*mover, *atomic.Int32) {
	var attempts atomic.Int32
	m := &mover{
		move: func(context.Context) error {
			attempts.Add(1)
			<-release
			if fail.Load() {
				return errors.New("the graph is down")
			}
			return nil
		},
		isDone: func() (bool, error) { return false, nil },
		now:    func() time.Time { return *now },
		wait:   20 * time.Millisecond,
	}
	return m, &attempts
}

// While the move runs, a write waits a little and then hears that the
// calendar is moving; every writer waits on the one attempt; once it has
// finished, everyone may write, and nothing moves again.
func TestAWriteWaitsForTheMoveThenSaysItIsMoving(t *testing.T) {
	release, fail, now := make(chan struct{}), &atomic.Bool{}, time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	m, attempts := testMover(release, fail, &now)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.ready(context.Background()); !errors.Is(err, ErrMoving) {
				t.Errorf("while moving: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := attempts.Load(); n != 1 {
		t.Errorf("%d attempts at once, want 1", n)
	}
	close(release)
	// Waiting as long as it takes now: the read answers when the attempt
	// under way finishes.
	m.wait = time.Hour
	if err := m.ready(context.Background()); err != nil {
		t.Fatalf("after the move: %v", err)
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("moved %d times", n)
	}
}

// A move that failed is tried again, but not before a minute has passed,
// and writes in between hear that the calendar is moving.
func TestAFailedMoveIsTriedAgainAfterAMinute(t *testing.T) {
	release, fail, now := make(chan struct{}), &atomic.Bool{}, time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	close(release)
	fail.Store(true)
	m, attempts := testMover(release, fail, &now)
	m.wait = time.Second
	if err := m.ready(context.Background()); !errors.Is(err, ErrMoving) {
		t.Fatalf("a failed move: %v", err)
	}
	if err := m.ready(context.Background()); !errors.Is(err, ErrMoving) || attempts.Load() != 1 {
		t.Fatalf("at once again: %v after %d attempts", err, attempts.Load())
	}
	fail.Store(false)
	now = now.Add(moveRetry)
	if err := m.ready(context.Background()); err != nil || attempts.Load() != 2 {
		t.Fatalf("a minute later: %v after %d attempts", err, attempts.Load())
	}
}

// A move marked done on this install isn't run again.
func TestAMoveMarkedDoneIsNotRunAgain(t *testing.T) {
	release, fail, now := make(chan struct{}), &atomic.Bool{}, time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	m, attempts := testMover(release, fail, &now)
	m.isDone = func() (bool, error) { return true, nil }
	if err := m.ready(context.Background()); err != nil || attempts.Load() != 0 {
		t.Errorf("%v after %d attempts", err, attempts.Load())
	}
}

// After the move is recorded, a restart serves at once: no move, no check
// again, no 503, and reads from the one store from the first. The record is
// read once, then kept.
func TestARestartAfterTheMoveServesAtOnce(t *testing.T) {
	release, fail, now := make(chan struct{}), &atomic.Bool{}, time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	m, attempts := testMover(release, fail, &now)
	var reads atomic.Int32
	m.isDone = func() (bool, error) { reads.Add(1); return true, nil }
	prev := moves
	t.Cleanup(func() { moves = prev })
	moves = m
	t.Setenv("CALENDAR_STORE", "postgres")
	if !helpers.CalendarReadsFromStore() {
		t.Error("a read after the restart goes to the old stores")
	}
	for i := 0; i < 100; i++ {
		if err := storeReady(context.Background()); err != nil {
			t.Fatalf("a write after the restart: %v", err)
		}
		_ = helpers.CalendarReadsFromStore()
		now = now.Add(time.Minute)
	}
	if n := attempts.Load(); n != 0 {
		t.Errorf("moved %d times after the restart", n)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("the record was read %d times, want once", n)
	}
}

// A record that can't be read (the database restarting) moves nothing and
// shows nothing stale: a write says why it failed, and reads stay with the
// one store, where a moved install's events are.
func TestARecordThatCantBeReadMovesNothing(t *testing.T) {
	release, fail, now := make(chan struct{}), &atomic.Bool{}, time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	m, attempts := testMover(release, fail, &now)
	down := errors.New("the database is restarting")
	m.isDone = func() (bool, error) { return false, down }
	prev := moves
	t.Cleanup(func() { moves = prev })
	moves = m
	t.Setenv("CALENDAR_STORE", "postgres")
	if err := storeReady(context.Background()); !errors.Is(err, down) {
		t.Errorf("a write: %v", err)
	}
	if !helpers.CalendarReadsFromStore() {
		t.Error("a read went to the old stores")
	}
	if n := attempts.Load(); n != 0 {
		t.Errorf("moved %d times on a record it couldn't read", n)
	}
}

// While the move runs, every reader answers from the two old stores, as
// before the switch, and only a write hears that the calendar is being moved;
// when it is recorded, every reader switches together.
func TestReadsDuringTheMoveComeFromTheOldStores(t *testing.T) {
	release, fail, now := make(chan struct{}), &atomic.Bool{}, time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	m, attempts := testMover(release, fail, &now)
	prev, prevGraph, prevGoogle := moves, graphEventsOf, googleBusyOf
	t.Cleanup(func() { moves, graphEventsOf, googleBusyOf = prev, prevGraph, prevGoogle })
	moves = m
	t.Setenv("CALENDAR_STORE", "postgres")
	start := now.Add(time.Hour)
	end := start.Add(time.Hour)
	graphEventsOf = func(context.Context, string, *time.Time, *time.Time) ([]*dgraphStruct.DgraphEvent, error) {
		return []*dgraphStruct.DgraphEvent{{Uuid: uuid.NewString(), Title: "Board", StartTime: &start, EndTime: &end}}, nil
	}
	googleBusyOf = func(context.Context, uuid.UUID, time.Time, time.Time) ([]integrationBusiness.BusySpan, error) {
		return nil, nil
	}

	b, err := ReadBusy(context.Background(), uuid.NewString(), now, now.Add(24*time.Hour), BusyOptions{})
	if err != nil || len(b.Intervals) != 1 || !b.Intervals[0].Start.Equal(start) {
		t.Fatalf("busy during the move: %+v %v", b, err)
	}
	if helpers.CalendarReadsFromStore() {
		t.Error("a read during the move goes to the one store")
	}
	if got := notificationPreferenceModel.FocusStoreNow().Events("fe"); got != "calendar_events fe" {
		t.Errorf("the push gate during the move reads %q", got)
	}
	who := &model.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: uuid.NewString()}}
	_, err = CreateEvent(context.Background(), who, adapter.CreateOrUpdateEventInput{Title: "Sync", StartTime: start.Format(time.RFC3339), EndTime: end.Format(time.RFC3339)})
	if !errors.Is(err, ErrMoving) {
		t.Errorf("a write during the move: %v", err)
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("%d moves under way", n)
	}

	close(release)
	m.wait = time.Hour
	if err := m.ready(context.Background()); err != nil {
		t.Fatalf("after the move: %v", err)
	}
	if !helpers.CalendarReadsFromStore() {
		t.Error("after the move, reads stay with the old stores")
	}
	if got := notificationPreferenceModel.FocusStoreNow().Events("fe"); got != "events fe" {
		t.Errorf("the push gate after the move reads %q", got)
	}
}

// With the store off, the calendar's API says so; with it on, it waits for
// the move.
func TestTheStoreSaysWhenItIsOff(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "graph")
	if err := storeReady(context.Background()); !errors.Is(err, ErrStoreOff) {
		t.Errorf("off: %v", err)
	}
	prev := moves
	t.Cleanup(func() { moves = prev })
	moves = &mover{isDone: func() (bool, error) { return true, nil }, now: time.Now, wait: time.Millisecond}
	t.Setenv("CALENDAR_STORE", "Postgres")
	if err := storeReady(context.Background()); err != nil {
		t.Errorf("on, moved: %v", err)
	}
}

// The move counts an event its store has as it is: new, the move's own and
// the same, the move's own and different (brought up to date), or changed in
// the store since (kept).
func TestTheMoveKnowsWhichEventsAreItsOwn(t *testing.T) {
	start := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	guest := uuid.New()
	mk := func() *models.Event {
		e := blank("UTC")
		e.ID, e.CreatedBy, e.Source, e.Title, e.StartAt, e.EndAt = uuid.New(), uuid.New(), sourceMoved, "Board", &start, &end
		return e
	}
	want := movedEvent{event: mk(), guests: []models.Attendee{{UserID: &guest}}}
	m := &moveRun{report: &MoveReport{Zones: map[string]int{}}, zones: map[uuid.UUID]zoneSource{}}
	same := copyEvent(want.event)
	if got := m.classify(want, same, []models.Attendee{{UserID: &guest}}); got != "" || m.report.Unchanged != 1 {
		t.Errorf("the same: %q %+v", got, m.report)
	}
	other := copyEvent(want.event)
	other.Title = "Board (moved)"
	if got := m.classify(want, other, []models.Attendee{{UserID: &guest}}); got != "refresh" || m.report.Refreshed != 1 {
		t.Errorf("different: %q %+v", got, m.report)
	}
	if got := m.classify(want, same, nil); got != "refresh" {
		t.Errorf("a guest missing: %q", got)
	}
	edited := copyEvent(want.event)
	edited.Sequence = 3
	if got := m.classify(want, edited, nil); got != "" || m.report.Kept != 1 {
		t.Errorf("changed since: %q %+v", got, m.report)
	}
	if got := m.classify(want, nil, nil); got != "" || m.report.Moved != 1 {
		t.Errorf("new: %q %+v", got, m.report)
	}
}

// The report says where each organiser's zone came from: the first hint
// this server's clock can read.
func TestTheMoveSaysWhereAZoneCameFrom(t *testing.T) {
	for _, c := range []struct {
		hints      models.ZoneHints
		zone, from string
	}{
		{models.ZoneHints{QuietHours: "Asia/Kolkata", BookingPage: "Europe/London"}, "Asia/Kolkata", "quiet_hours"},
		{models.ZoneHints{QuietHours: "Mars/Olympus", BookingPage: "Europe/London"}, "Europe/London", "booking_page"},
		{models.ZoneHints{TaskRepeat: "Pacific/Chatham"}, "Pacific/Chatham", "task_repeat"},
		{models.ZoneHints{}, "UTC", "utc"},
	} {
		if z := zoneFrom(c.hints); z != c.zone || zoneSourceOf(c.hints, z) != c.from {
			t.Errorf("%+v: %s from %s", c.hints, z, zoneSourceOf(c.hints, z))
		}
	}
}
