package helpers

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A reconciler over a fake store whose fingerprint the test controls.
type fakeStore struct {
	mu       sync.Mutex
	fp       string
	fpErr    error
	applyErr error
	applied  atomic.Int32
}

func newFakeStore(fp string) *fakeStore { return &fakeStore{fp: fp} }

func (s *fakeStore) set(fp string)           { s.mu.Lock(); s.fp = fp; s.mu.Unlock() }
func (s *fakeStore) failFingerprint(e error) { s.mu.Lock(); s.fpErr = e; s.mu.Unlock() }
func (s *fakeStore) failApply(e error)       { s.mu.Lock(); s.applyErr = e; s.mu.Unlock() }

func (s *fakeStore) reconciler(interval time.Duration) ConfigReconciler {
	return ConfigReconciler{
		Name:     "test",
		Interval: interval,
		Fingerprint: func(ctx context.Context) (string, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.fp, s.fpErr
		},
		Apply: func(ctx context.Context) error {
			s.mu.Lock()
			e := s.applyErr
			s.mu.Unlock()
			if e != nil {
				return e
			}
			s.applied.Add(1)
			return nil
		},
	}
}

func TestFirstObservationIsABaselineAndNeverRebuilds(t *testing.T) {
	// The cache was just built from these rows. Rebuilding it again would
	// only reset the state a rebuild carries (the circuit breaker), for nothing.
	s := newFakeStore("v1")
	st, applied, err := reconcileStep(context.Background(), s.reconciler(time.Second), reconcileState{})
	if err != nil {
		t.Fatal(err)
	}
	if applied || s.applied.Load() != 0 {
		t.Fatal("the baseline observation must not rebuild the cache")
	}
	if !st.baselined || st.fingerprint != "v1" {
		t.Fatalf("baseline not recorded: %+v", st)
	}
}

func TestAnUnchangedStoreLeavesTheCacheAlone(t *testing.T) {
	s := newFakeStore("v1")
	st := reconcileState{fingerprint: "v1", baselined: true}
	next, applied, err := reconcileStep(context.Background(), s.reconciler(time.Second), st)
	if err != nil {
		t.Fatal(err)
	}
	if applied || s.applied.Load() != 0 || next != st {
		t.Fatal("nothing changed in the store, so nothing should have been rebuilt")
	}
}

func TestAChangedStoreRebuildsTheCacheOnce(t *testing.T) {
	// The bug this exists for: another replica saved a new provider, and this
	// process must pick it up without being told.
	s := newFakeStore("v2")
	st := reconcileState{fingerprint: "v1", baselined: true}
	next, applied, err := reconcileStep(context.Background(), s.reconciler(time.Second), st)
	if err != nil {
		t.Fatal(err)
	}
	if !applied || s.applied.Load() != 1 {
		t.Fatal("a changed fingerprint must rebuild the cache")
	}
	if next.fingerprint != "v2" {
		t.Fatalf("the new fingerprint must be remembered, got %q", next.fingerprint)
	}
	// And only once: the same store on the next pass is "unchanged".
	_, applied, _ = reconcileStep(context.Background(), s.reconciler(time.Second), next)
	if applied || s.applied.Load() != 1 {
		t.Fatal("the same rows must not be rebuilt a second time")
	}
}

func TestAFailedRebuildIsRetriedOnTheNextPass(t *testing.T) {
	// Recording the new fingerprint after a failed Apply would make the failure
	// permanent: the store would look unchanged forever while the cache stayed
	// stale. The old fingerprint has to survive the failure.
	s := newFakeStore("v2")
	s.failApply(errors.New("db hiccup"))
	st := reconcileState{fingerprint: "v1", baselined: true}
	next, applied, err := reconcileStep(context.Background(), s.reconciler(time.Second), st)
	if err == nil || applied {
		t.Fatal("a failed apply must be reported and not counted as applied")
	}
	if next != st {
		t.Fatalf("a failed apply must keep the old state so the change is seen again, got %+v", next)
	}
	// The hiccup clears; the next pass rebuilds.
	s.failApply(nil)
	next, applied, err = reconcileStep(context.Background(), s.reconciler(time.Second), next)
	if err != nil || !applied || next.fingerprint != "v2" {
		t.Fatalf("after the fault clears the change must be applied: applied=%v err=%v state=%+v", applied, err, next)
	}
}

func TestAFingerprintErrorChangesNothing(t *testing.T) {
	s := newFakeStore("v2")
	s.failFingerprint(errors.New("db away"))
	st := reconcileState{fingerprint: "v1", baselined: true}
	next, applied, err := reconcileStep(context.Background(), s.reconciler(time.Second), st)
	if err == nil || applied || next != st {
		t.Fatal("an unreadable store must be reported and leave the state untouched")
	}
}

func TestStartRefusesAReconcilerThatCouldNeverWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newFakeStore("v1")
	for name, r := range map[string]ConfigReconciler{
		"no name":        {Interval: time.Second, Fingerprint: s.reconciler(time.Second).Fingerprint, Apply: s.reconciler(time.Second).Apply},
		"zero interval":  {Name: "x", Fingerprint: s.reconciler(time.Second).Fingerprint, Apply: s.reconciler(time.Second).Apply},
		"no fingerprint": {Name: "x", Interval: time.Second, Apply: s.reconciler(time.Second).Apply},
		"no apply":       {Name: "x", Interval: time.Second, Fingerprint: s.reconciler(time.Second).Fingerprint},
	} {
		if err := StartConfigReconciler(ctx, r); err == nil {
			t.Errorf("%s: a reconciler that cannot work must be refused at start, not left running silently", name)
		}
	}
}

func TestTheLoopConvergesOnAChangeAndStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newFakeStore("v1")
	if err := StartConfigReconciler(ctx, s.reconciler(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// Baseline taken; a few ticks with nothing changed.
	time.Sleep(50 * time.Millisecond)
	if s.applied.Load() != 0 {
		t.Fatal("an unchanged store must never trigger a rebuild")
	}
	// Another replica saves.
	s.set("v2")
	deadline := time.Now().Add(2 * time.Second)
	for s.applied.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.applied.Load() != 1 {
		t.Fatalf("the loop must rebuild exactly once after a change, got %d", s.applied.Load())
	}
	// Stop; further changes must not be applied.
	cancel()
	time.Sleep(30 * time.Millisecond)
	s.set("v3")
	time.Sleep(50 * time.Millisecond)
	if s.applied.Load() != 1 {
		t.Fatal("a stopped loop must not keep rebuilding")
	}
}
