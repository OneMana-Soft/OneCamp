package Calendar

// Whether the one store can be used now. It can when it is on (the default;
// CALENDAR_STORE=graph keeps the old stores) and the events of the two old
// stores have been moved into it
// (move.go). The move runs once per install, by itself, the first time
// anything asks after the switch, and a sweep asks every minute so it starts
// even when nobody opens the calendar; it is recorded (movedKey) only once it
// has been checked, and an install that has the record never moves or checks
// again, after any restart.
//
// Until the move has finished, reads come from the two old stores, as they
// did before the switch (helpers.CalendarReadsFromStore): nothing writes to
// them meanwhile, because every write waits for the move a little and then
// answers ErrMoving (503, "try again in a minute"). A calendar half moved is
// never read, and no write lands in the store being left. When the record is
// written, every reader switches to the one store together.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

var (
	// ErrStoreOff is a call to the calendar's own API on a server that
	// still keeps events in the two old stores (CALENDAR_STORE=graph).
	ErrStoreOff = errors.New("This part of the calendar isn't switched on on this server yet.")
	// ErrMoving is a call while the calendar's events are being moved to
	// its one store.
	ErrMoving = errors.New("The calendar is being moved to its new store. Try again in a minute.")
)

// movedKey records in the system config that the move into the one store has
// finished on this install, after which the old stores are never read again.
const movedKey = "calendar_store_moved"

// Timings of the move.
const (
	// moveWait is how long a read or a write waits for a move under way.
	moveWait = 20 * time.Second
	// moveRetry is how soon a move that failed is tried again.
	moveRetry = time.Minute
	// moveTimeLimit bounds one attempt.
	moveTimeLimit = 30 * time.Minute
)

// mover runs the move once in this process and tells everyone who waits.
type mover struct {
	mu      sync.Mutex
	done    atomic.Bool
	running chan struct{} // closed when the attempt under way ends; nil when none is
	tried   time.Time     // when the last attempt started
	// move moves and checks, and marks the move done; isDone reads the mark.
	move   func(ctx context.Context) error
	isDone func() (bool, error)
	now    func() time.Time
	wait   time.Duration
}

var moves = &mover{
	move:   moveAndMark,
	isDone: func() (bool, error) { return configModel.IsDone(movedKey) },
	now:    time.Now,
	wait:   moveWait,
}

// storeReady is whether the one store can be used now.
func storeReady(ctx context.Context) error {
	if !helpers.CalendarInPostgres() {
		return ErrStoreOff
	}
	return moves.ready(ctx)
}

func (m *mover) ready(ctx context.Context) error {
	if m.done.Load() {
		return nil
	}
	done, err := m.isDone()
	if err != nil {
		// A record that can't be read now is no reason to move again.
		return err
	}
	if done {
		m.done.Store(true)
		return nil
	}
	finished := m.start()
	timer := time.NewTimer(m.wait)
	defer timer.Stop()
	select {
	case <-finished:
		if m.done.Load() {
			return nil
		}
		return ErrMoving
	case <-timer.C:
		return ErrMoving
	case <-ctx.Done():
		return ctx.Err()
	}
}

// movedNow is whether this install's events are in the one store, without
// waiting: the move this process ran, else the record one left (read until it
// says so, then kept). When they aren't, the move is started. A record that
// can't be read starts nothing and says yes: an install spends minutes once
// moving and the rest of its life moved, so the one store is where its
// events almost always are, and the old stores, stale once moved, are never
// shown by mistake. The next read asks again.
func (m *mover) movedNow() bool {
	if m.done.Load() {
		return true
	}
	done, err := m.isDone()
	if err != nil {
		return true
	}
	if done {
		m.done.Store(true)
		return true
	}
	m.start()
	return false
}

// closed is a channel that is already closed: no attempt to wait for.
var closed = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// start starts an attempt unless one is under way or the last failed a moment
// ago, and answers what to wait on.
func (m *mover) start() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running != nil {
		return m.running
	}
	if m.done.Load() || (!m.tried.IsZero() && m.now().Sub(m.tried) < moveRetry) {
		return closed
	}
	finished := make(chan struct{})
	m.running, m.tried = finished, m.now()
	helpers.GoSafeNamed("business/Calendar/move", func() {
		defer func() {
			m.mu.Lock()
			m.running = nil
			m.mu.Unlock()
			close(finished)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), moveTimeLimit)
		defer cancel()
		if err := m.move(ctx); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: the move into the one store didn't finish, and is tried again in a minute: %v", err)
			return
		}
		m.done.Store(true)
	})
	return finished
}

func init() {
	// Every reader asks the one latch, read through moves when it is asked
	// (a test puts its own in).
	helpers.SetCalendarMoved(func() bool { return moves.movedNow() })
	// So the move starts on the switch even when nobody opens the calendar:
	// the push gate reads the store directly.
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "calendar.move", Every: time.Minute, Run: func(ctx context.Context) error {
		if !helpers.CalendarInPostgres() || moves.done.Load() {
			return nil
		}
		if err := moves.ready(ctx); err != nil && !errors.Is(err, ErrMoving) {
			return err
		}
		return nil
	}})
}
