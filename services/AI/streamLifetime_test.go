package ai

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A stop registry the test controls: who asked, and when the answer arrives.
type fakeStops struct {
	mu  sync.Mutex
	who string
}

func (f *fakeStops) set(who string) { f.mu.Lock(); f.who = who; f.mu.Unlock() }
func (f *fakeStops) take(ctx context.Context, session string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.who
	f.who = "" // consumed, as the real one is
	return w
}
func (f *fakeStops) clear(ctx context.Context, session string) { f.set("") }
func (f *fakeStops) pending() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.who
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	// Generous: the lifetime falls back to polling every stopPollSlow once its
	// fast window has passed, and under a loaded full-suite run a 3 s wait
	// failed while the code was right. It returns as soon as cond holds, so a
	// passing run is no slower.
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAClientThatIsStillListeningChangesNothing(t *testing.T) {
	client, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newStreamLifetime(client, "s1", "alice", time.Minute, &fakeStops{})
	defer l.Close()
	time.Sleep(50 * time.Millisecond)
	if l.ClientGone() || l.Stopped() || l.Gen.Err() != nil {
		t.Fatal("a listening client must leave the run untouched")
	}
}

func TestAClientThatGoesAwayDoesNotEndTheAnswer(t *testing.T) {
	// The whole point: a closed tab is not a request to stop.
	client, cancel := context.WithCancel(context.Background())
	l := newStreamLifetime(client, "s1", "alice", time.Minute, &fakeStops{})
	defer l.Close()
	cancel()
	waitFor(t, "the client to be seen as gone", l.ClientGone)
	time.Sleep(400 * time.Millisecond) // past the first fast polls
	if l.Gen.Err() != nil {
		t.Fatal("generation must continue after the client leaves")
	}
	if l.Stopped() {
		t.Fatal("nobody asked for a stop")
	}
}

func TestTheOwnersStopEndsTheAnswerEvenWhenItArrivesLate(t *testing.T) {
	// The stop request rides its own connection and can land after the
	// disconnect it was sent with. It must still count.
	client, cancel := context.WithCancel(context.Background())
	stops := &fakeStops{}
	l := newStreamLifetime(client, "s1", "alice", time.Minute, stops)
	defer l.Close()
	cancel()
	waitFor(t, "the client to be seen as gone", l.ClientGone)
	time.Sleep(600 * time.Millisecond)
	stops.set("alice")
	waitFor(t, "the owner's stop to take effect", l.Stopped)
	if l.Gen.Err() == nil {
		t.Fatal("a deliberate stop must cancel generation")
	}
}

func TestAStrangersStopIsIgnoredAndConsumed(t *testing.T) {
	// A session id is not a secret. Somebody else's stop must neither end the
	// answer nor lie in wait for the next one.
	client, cancel := context.WithCancel(context.Background())
	stops := &fakeStops{}
	stops.set("mallory")
	l := newStreamLifetime(client, "s1", "alice", time.Minute, stops)
	defer l.Close()
	cancel()
	waitFor(t, "the client to be seen as gone", l.ClientGone)
	time.Sleep(400 * time.Millisecond)
	if l.Stopped() || l.Gen.Err() != nil {
		t.Fatal("a stranger's stop must not end the owner's answer")
	}
	if stops.pending() != "" {
		t.Fatal("the stranger's request must be consumed, not left for the next answer")
	}
}

func TestTheTimeoutStillBoundsADetachedAnswer(t *testing.T) {
	// Detaching from the client must not detach from the clock: an answer
	// nobody is reading is the one most in need of a ceiling.
	client, cancel := context.WithCancel(context.Background())
	l := newStreamLifetime(client, "s1", "alice", 100*time.Millisecond, &fakeStops{})
	defer l.Close()
	cancel()
	waitFor(t, "the timeout", func() bool { return l.Gen.Err() != nil })
	if l.Stopped() {
		t.Fatal("a timeout is not a stop by the owner")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	l := newStreamLifetime(context.Background(), "s1", "alice", time.Minute, &fakeStops{})
	l.Close()
	l.Close()
	if l.Gen.Err() == nil {
		t.Fatal("Close must end generation")
	}
}

func TestAStopLeftOverFromThePreviousAnswerDoesNotEndTheNext(t *testing.T) {
	// The person pressed stop, but the answer had already finished, so nothing
	// consumed the request. It must not lie in wait for the next answer and
	// end it the moment that client steps away.
	stops := &fakeStops{}
	stops.set("alice")
	client, cancel := context.WithCancel(context.Background())
	l := newStreamLifetime(client, "s1", "alice", time.Minute, stops)
	defer l.Close()
	if stops.pending() != "" {
		t.Fatal("a new answer must start with no pending stop")
	}
	cancel()
	waitFor(t, "the client to be seen as gone", l.ClientGone)
	time.Sleep(400 * time.Millisecond)
	if l.Stopped() || l.Gen.Err() != nil {
		t.Fatal("the stale stop ended the new answer")
	}
}
