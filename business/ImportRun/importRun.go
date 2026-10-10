// Package business starts import runs, one at a time per job.
//
// A run's status is not proof that its run is over. An orchestrator writes
// "failed" (or an admin's Cancel writes "cancelled") and then keeps going for
// a while: it closes the staged export, clears provider caches, removes its
// cancel registration and, after a cancel, lets its workers finish the chunk
// they hold. A Run or Retry that read "failed" in that window started a second
// orchestrator beside the first, and the first's last cleanup then removed the
// second's cancel registration (Cancel could no longer stop it) and cleared
// the provider state it was using.
//
// So a job's run is leased here for as long as its goroutine lives, and a new
// run starts only when no lease is held. A lease never outlives its holder: a
// run gives it back when its goroutine ends, a panic included (the job is
// failed if it was left running); a lease whose run never started is given
// back by Start, or by a Release deferred where it was taken. And it is
// in-process, as the cancel registrations it protects are: a restart ends
// every run along with its lease.
package business

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// ErrRunAlive is Start's answer while the job's previous run is still ending,
// whatever its status says by now.
var ErrRunAlive = errors.New("the import's last run is still stopping")

var (
	mu   sync.Mutex
	live = map[uuid.UUID]*Lease{}

	// held, when set, keeps every lease taken while it is set until it is
	// closed (HoldRunsForTest).
	held chan struct{}
)

// Start moves the job to running from one of from and calls run in its own
// goroutine, only while no earlier run of the job is alive; the job's lease is
// released when run returns. It reports whether it started the job: false with
// no error when the job is in none of from, ErrRunAlive while a run is still
// ending, and the model's error (ErrConflictActiveJob among them) otherwise.
func Start(ctx context.Context, jobId uuid.UUID, from []string, run func()) (bool, error) {
	l, err := Take(jobId)
	if err != nil {
		return false, err
	}
	return l.Start(ctx, from, run)
}

// Lease is a job's run lease, taken before its run starts, for work that has
// to happen with no run alive (retrying failed chunks resets them to pending,
// which a worker still running would claim).
type Lease struct {
	jobId uuid.UUID
	hold  chan struct{}

	state    sync.Mutex
	launched bool // a run was started under it, and gives it back
	once     sync.Once
}

// Take takes the job's lease, or answers ErrRunAlive while a run holds it. The
// lease is given back by the run Start begins under it, when that run ends,
// and otherwise by Release, which is meant to be deferred right here.
func Take(jobId uuid.UUID) (*Lease, error) {
	mu.Lock()
	defer mu.Unlock()
	if _, busy := live[jobId]; busy {
		return nil, ErrRunAlive
	}
	l := &Lease{jobId: jobId, hold: held}
	live[jobId] = l
	return l, nil
}

// Start is the package's Start under a lease already taken. When the job does
// not start, or Start panics, the lease is given back here; a lease that has
// already started a run, or been released, starts nothing (ErrRunAlive).
func (l *Lease) Start(ctx context.Context, from []string, run func()) (bool, error) {
	defer l.Release()
	if !l.holds() {
		return false, ErrRunAlive
	}
	started, err := importModels.StartRunning(ctx, l.jobId, from)
	if err != nil || !started {
		return false, err
	}
	l.state.Lock()
	l.launched = true
	l.state.Unlock()
	go l.run(run)
	return true, nil
}

// run is a run's goroutine: run, then the lease back, whatever happens. A
// panic that run leaves unrecovered ends the run here rather than the server,
// and a job it left running is failed, so it can be run again.
func (l *Lease) run(run func()) {
	defer l.end()
	defer func() {
		if l.hold != nil {
			<-l.hold
		}
	}()
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		ctx := context.Background()
		helpers.LogErrorWithContext(ctx, "ImportRun: run of job %s panicked: %v", l.jobId, r)
		stage, msg := "failed", fmt.Sprintf("panic: %v", r)
		if _, err := importModels.UpdateStatusFrom(ctx, l.jobId,
			[]string{importModels.StatusRunning, importModels.StatusPaused},
			importModels.StatusFailed, &stage, &msg); err != nil {
			helpers.LogErrorWithContext(ctx, "ImportRun: could not fail job %s after its panic: %+v", l.jobId, err)
		}
	}()
	run()
}

// holds reports whether the lease is still held and has started no run.
func (l *Lease) holds() bool {
	mu.Lock()
	defer mu.Unlock()
	l.state.Lock()
	defer l.state.Unlock()
	return live[l.jobId] == l && !l.launched
}

// Release gives the lease back, unless a run was started under it (that run
// gives it back when it ends). Safe to defer right after Take, and to call
// more than once.
func (l *Lease) Release() {
	l.state.Lock()
	launched := l.launched
	l.state.Unlock()
	if !launched {
		l.end()
	}
}

// end gives the lease back, once, and only this lease: a later lease of the
// same job is never released by an earlier one.
func (l *Lease) end() {
	l.once.Do(func() {
		mu.Lock()
		if live[l.jobId] == l {
			delete(live, l.jobId)
		}
		mu.Unlock()
	})
}

// HoldRunsForTest keeps the lease of every run started from now on until the
// returned function is called, as if each run were still cleaning up after
// writing its last status.
func HoldRunsForTest() (done func()) {
	ch := make(chan struct{})
	mu.Lock()
	held = ch
	mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			if held == ch {
				held = nil
			}
			mu.Unlock()
			close(ch)
		})
	}
}
