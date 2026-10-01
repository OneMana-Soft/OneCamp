package helpers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// These tests exist because the failure they guard is a process exit, which is
// the one bug class a normal test run cannot survive to report. An unrecovered
// panic in any goroutine terminates the program, so a panicking source inside a
// fan-out took the whole server down — on a home-page load, for every user.
//
// If GoSafeSend ever loses its recover, these tests do not fail politely: the
// test binary itself dies. That is the correct signal here, and it is why the
// panic case is worth an explicit test rather than being assumed.

func TestGoSafeSendDeliversResult(t *testing.T) {
	ch := make(chan []int, 1)
	GoSafeSend("ok", ch, func() []int { return []int{1, 2, 3} })
	select {
	case got := <-ch:
		if len(got) != 3 {
			t.Fatalf("expected 3 items, got %d", len(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the result")
	}
}

func TestGoSafeSendConvertsPanicToZeroValue(t *testing.T) {
	ch := make(chan []int, 1)
	GoSafeSend("panicking", ch, func() []int {
		panic("simulated malformed upstream response")
	})
	// The contract is that the caller still receives exactly one value per
	// source, so its join stays prompt instead of waiting out the budget.
	select {
	case got := <-ch:
		if got != nil {
			t.Fatalf("expected the zero value after a panic, got %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("panicking source never delivered; callers would stall on every panic")
	}
}

// A panic must not stop the sibling sources from contributing. This is the
// property the briefing and attention paths actually rely on: one bad source
// degrades to nothing while the rest of the surface still renders.
func TestGoSafeSendPanicDoesNotAffectSiblings(t *testing.T) {
	a := make(chan []string, 1)
	b := make(chan []string, 1)
	GoSafeSend("bad", a, func() []string { panic("boom") })
	GoSafeSend("good", b, func() []string { return []string{"kept"} })

	var gotA, gotB []string
	for i := 0; i < 2; i++ {
		select {
		case gotA = <-a:
		case gotB = <-b:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out joining sources")
		}
	}
	if gotA != nil {
		t.Fatalf("panicking source should contribute nothing, got %v", gotA)
	}
	if len(gotB) != 1 || gotB[0] != "kept" {
		t.Fatalf("healthy sibling should still deliver, got %v", gotB)
	}
}

func TestGoSafeNamedSurvivesPanic(t *testing.T) {
	done := make(chan struct{})
	GoSafeNamed("panicking-worker", func() {
		defer close(done)
		panic("simulated worker panic")
	})
	select {
	case <-done:
		// Reaching here at all proves the panic was contained.
	case <-time.After(2 * time.Second):
		t.Fatal("worker never ran")
	}
}

// A log call must never be the thing that kills the process.
//
// resolveLogger is tested directly rather than by setting Logger = nil and
// calling LogWithContext. That version raced: the GoSafeSend tests above spawn
// goroutines that log AFTER sending, so they can still be mid-log when the test
// reassigns the global, and -race rightly failed it. Testing the resolver keeps
// the assertion on the actual property with no shared state.
//
// The exposure is real rather than theoretical: helpers.Logger stays nil until
// loggerInit runs, and LogWithContext is called from inside panic-recovery
// handlers. A nil dereference there panics within the recover, where nothing can
// catch it, turning a contained panic into a crash. Verified by making the guard
// unreachable, which produced exactly that nil-pointer panic.
func TestResolveLoggerNeverReturnsNil(t *testing.T) {
	if got := resolveLogger(nil); got == nil {
		t.Fatal("resolveLogger(nil) returned nil; a log call from a recover handler would crash the process")
	}
	// Logging through the resolved fallback must itself be safe.
	resolveLogger(nil).ErrorContext(context.Background(), "fallback logger is usable")
}

func TestResolveLoggerPassesThroughConfiguredLogger(t *testing.T) {
	want := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := resolveLogger(want); got != want {
		t.Fatal("resolveLogger must not replace an already-configured logger")
	}
}

// RecoverToErr is used by the import providers, whose goroutines stream from
// third-party APIs and decode responses whose shape they do not control. The
// tests below pin the two things that make it useful: the panic is contained,
// and it is REPORTED rather than swallowed — a producer that closed silently
// would look like "the source had no data" and lose the import's work quietly.
func TestRecoverToErrReportsPanicAsError(t *testing.T) {
	out := make(chan int, 4)
	errCh := make(chan error, 1)

	// Exactly the shape the providers use, and the only correct defer order:
	// RecoverToErr is registered LAST so LIFO runs it FIRST, while errCh is
	// still open.
	go func() {
		defer close(out)
		defer close(errCh)
		defer RecoverToErr("test.producer", errCh)
		out <- 1
		panic("simulated malformed upstream payload")
	}()

	// The consumer's range must still terminate, which the closes guarantee.
	var got []int
	for v := range out {
		got = append(got, v)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("expected the pre-panic item to survive, got %v", got)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected the panic to be reported as an error")
		}
		// Naming the site is the point: a bare "panic" in a log gives an
		// operator nothing to act on.
		if !strings.Contains(err.Error(), "test.producer") {
			t.Fatalf("error should name the producer, got %q", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("panic was contained but never reported; the import would look empty")
	}
}

// A producer whose error slot is already taken must not block on the way out,
// or the panic path would deadlock instead of crashing — which is worse, because
// it hangs the import rather than failing it.
func TestRecoverToErrDoesNotBlockOnAFullErrorChannel(t *testing.T) {
	errCh := make(chan error, 1)
	errCh <- errors.New("first failure already recorded")

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer RecoverToErr("test.saturated", errCh)
		panic("second failure")
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RecoverToErr blocked on a full error channel")
	}
}

func TestRecoverToErrIsInertWithoutAPanic(t *testing.T) {
	errCh := make(chan error, 1)
	func() {
		defer RecoverToErr("test.clean", errCh)
	}()
	select {
	case err := <-errCh:
		t.Fatalf("no panic occurred, but an error was reported: %v", err)
	default:
	}
}
