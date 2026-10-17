package business

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memOnce keeps sends in memory, as Redis would, and how long each is kept.
type memOnce struct {
	mu  sync.Mutex
	m   map[string]string
	ttl map[string]time.Duration
}

func (s *memOnce) claim(_ context.Context, key string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.m[key]; taken {
		return false, nil
	}
	s.m[key], s.ttl[key] = sendOnceGoing, ttl
	return true, nil
}

func (s *memOnce) get(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok, nil
}

func (s *memOnce) put(_ context.Context, key, value string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key], s.ttl[key] = value, ttl
	return nil
}

// keptFor is how long key is kept.
func (s *memOnce) keptFor(key string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttl[key]
}

func (s *memOnce) release(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

type sent struct {
	UUID string `json:"uuid"`
}

func withStore(t *testing.T) *memOnce {
	t.Helper()
	store := &memOnce{m: map[string]string{}, ttl: map[string]time.Duration{}}
	prev := theOnceStore
	theOnceStore = func() onceStore { return store }
	t.Cleanup(func() { theOnceStore = prev })
	return store
}

// A send whose answer was lost is tried again with the same client id: the
// server had it, so the repeat gets the first one's answer, not a second copy.
func TestASendTriedAgainIsTheSameSend(t *testing.T) {
	withStore(t)
	var writes atomic.Int32
	send := func() (*sent, error) {
		n := writes.Add(1)
		return &sent{UUID: "post-" + string(rune('0'+n))}, nil
	}
	first, err := Once(context.Background(), "maya", "ch-1", "local-mf3k2x1a-1b", send)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Once(context.Background(), "maya", "ch-1", "local-mf3k2x1a-1b", send)
	if err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 1 || again.UUID != first.UUID {
		t.Fatalf("%d writes, answers %q and %q: the repeat made a second copy", writes.Load(), first.UUID, again.UUID)
	}

	// Someone else's id, or another message, is a send of its own.
	if _, err := Once(context.Background(), "jonas", "ch-1", "local-mf3k2x1a-1b", send); err != nil || writes.Load() != 2 {
		t.Fatalf("another person's send: %d writes, %v", writes.Load(), err)
	}
	if _, err := Once(context.Background(), "maya", "ch-1", "local-mf3k2x1a-1c", send); err != nil || writes.Load() != 3 {
		t.Fatalf("another message: %d writes, %v", writes.Load(), err)
	}
	// Two of Maya's tabs can give the same id to messages in two
	// conversations: they're two sends.
	if _, err := Once(context.Background(), "maya", "dm-jonas", "local-mf3k2x1a-1b", send); err != nil || writes.Load() != 4 {
		t.Fatalf("the same id in another conversation: %d writes, %v", writes.Load(), err)
	}
}

func TestASendThatFailedIsTriedAfresh(t *testing.T) {
	withStore(t)
	fail := true
	var writes int
	send := func() (*sent, error) {
		writes++
		if fail {
			return nil, errors.New("the graph refused it")
		}
		return &sent{UUID: "post-1"}, nil
	}
	if _, err := Once(context.Background(), "maya", "ch-1", "local-1", send); err == nil {
		t.Fatal("a failed send answered as sent")
	}
	fail = false
	got, err := Once(context.Background(), "maya", "ch-1", "local-1", send)
	if err != nil || got.UUID != "post-1" || writes != 2 {
		t.Fatalf("the retry of a failed send: %+v %v after %d writes", got, err, writes)
	}
}

func TestARepeatWaitsForTheFirstTry(t *testing.T) {
	withStore(t)
	release := make(chan struct{})
	var writes atomic.Int32
	send := func() (*sent, error) {
		writes.Add(1)
		<-release
		return &sent{UUID: "post-1"}, nil
	}
	done := make(chan *sent, 1)
	go func() {
		got, _ := Once(context.Background(), "maya", "ch-1", "local-1", send)
		done <- got
	}()
	time.Sleep(50 * time.Millisecond) // the first try has the key
	repeat := make(chan *sent, 1)
	go func() {
		got, _ := Once(context.Background(), "maya", "ch-1", "local-1", send)
		repeat <- got
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)
	if a, b := <-done, <-repeat; a == nil || b == nil || a.UUID != b.UUID || writes.Load() != 1 {
		t.Fatalf("first %+v, repeat %+v, %d writes", a, b, writes.Load())
	}
}

func TestARepeatOfASendStillGoingIsTurnedAway(t *testing.T) {
	store := withStore(t)
	prev := sendOnceWait
	sendOnceWait = 300 * time.Millisecond
	t.Cleanup(func() { sendOnceWait = prev })
	_ = store.put(context.Background(), "send:once:maya:ch-1:local-1", sendOnceGoing, sendOnceLease)
	_, err := Once(context.Background(), "maya", "ch-1", "local-1", func() (*sent, error) {
		t.Fatal("a repeat sent while the first was still going")
		return nil, nil
	})
	if rj, ok := AsRejection(err); !ok || rj.Status != http.StatusConflict {
		t.Fatalf("a repeat while the first try is still going answered %v", err)
	}
}

// No client id, or one that isn't an app's, is a plain send.
func TestASendWithoutAClientIDJustSends(t *testing.T) {
	withStore(t)
	var writes int
	send := func() (*sent, error) { writes++; return &sent{}, nil }
	for _, id := range []string{"", "has spaces", "{}", "a/b"} {
		if _, err := Once(context.Background(), "maya", "ch-1", id, send); err != nil {
			t.Fatal(err)
		}
		if _, err := Once(context.Background(), "maya", "ch-1", id, send); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 8 {
		t.Fatalf("%d writes, want 8", writes)
	}
}

// A send that died on the way (a panic, the server stopping) kept its claim
// for the day an answer is kept: every Try again waited, was turned away as
// "still being sent", and the message could never go. One that panicked now
// lets go at once, so Try again sends it.
func TestASendThatPanickedIsTriedAgain(t *testing.T) {
	withStore(t)
	prev := sendOnceWait
	sendOnceWait = 300 * time.Millisecond
	t.Cleanup(func() { sendOnceWait = prev })
	func() {
		defer func() { _ = recover() }()
		_, _ = Once(context.Background(), "maya", "ch-1", "local-1", func() (*sent, error) { panic("the graph client panicked") })
	}()
	got, err := Once(context.Background(), "maya", "ch-1", "local-1", func() (*sent, error) { return &sent{UUID: "post-1"}, nil })
	if err != nil || got == nil || got.UUID != "post-1" {
		t.Fatalf("Try again after a send that panicked: %+v %v", got, err)
	}
}

// failingGet is a store whose reads fail.
type failingGet struct{ *memOnce }

func (failingGet) get(context.Context, string) (string, bool, error) {
	return "", false, errors.New("redis: connection refused")
}

// A repeat that can't read the first try's state asks the app to try again
// shortly, as a store that's down is: it isn't a message the app got wrong.
func TestARepeatThatCantReadTheStoreIsToldToTryAgain(t *testing.T) {
	store := failingGet{&memOnce{m: map[string]string{"send:once:maya:ch-1:local-1": sendOnceGoing}, ttl: map[string]time.Duration{}}}
	prev := theOnceStore
	theOnceStore = func() onceStore { return store }
	t.Cleanup(func() { theOnceStore = prev })
	_, err := Once(context.Background(), "maya", "ch-1", "local-1", func() (*sent, error) {
		t.Fatal("a repeat sent while it couldn't tell whether the first went")
		return nil, nil
	})
	if rj, ok := AsRejection(err); !ok || rj.Status != http.StatusServiceUnavailable {
		t.Fatalf("a repeat that couldn't read the store answered %v", err)
	}
}

// A send under way holds its key for a moment, and its answer is kept for
// the day: a send whose server stopped mid-way holds back its Try again for a
// minute, not for the day.
func TestASendHoldsItsKeyForAMomentAndItsAnswerForADay(t *testing.T) {
	store := withStore(t)
	const key = "send:once:maya:ch-1:local-2"
	var held time.Duration
	if _, err := Once(context.Background(), "maya", "ch-1", "local-2", func() (*sent, error) {
		held = store.keptFor(key)
		return &sent{UUID: "post-2"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if held != sendOnceLease || held > time.Minute {
		t.Errorf("a send under way holds its key for %v, want a minute at most", held)
	}
	if kept := store.keptFor(key); kept != sendOnceFor {
		t.Errorf("an answer is kept for %v, want %v", kept, sendOnceFor)
	}
}
