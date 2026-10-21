package helpers

import (
	"context"
	"fmt"
	"runtime/debug"
)

// goSafeWork is every goroutine the GoSafe family started that hasn't
// finished, so a shutdown or a test can wait for them (background.go).
var goSafeWork Inflight

func init() { RegisterBackground("goroutines started with GoSafe", goSafeWork.Settle) }

// GoSafe runs fn in a new goroutine with a recover guard. Any panic
// inside fn is caught, logged with a stack trace, and swallowed so the
// process continues. Use this for fire-and-forget background work
// (webhook side-effects, notification dispatch, sync triggers) where
// the caller can't (or shouldn't) wait for the result.
//
// Naming: pass a stable label as the first argument to make recovered
// panics traceable in production logs.
func GoSafe(fn func()) { goSafe("helpers.GoSafe", true, fn) }

// GoSafeNamed is GoSafe with a label so the recovered log line tells
// the operator which call site panicked. Prefer this in any new code.
func GoSafeNamed(name string, fn func()) { goSafe("helpers.GoSafeNamed["+name+"]", true, fn) }

// GoSafeBounded is GoSafeNamed with at most cap(slots) running at once: it
// takes a place in slots BEFORE the goroutine starts, so a caller with a full
// pool waits, and gives it back when fn is done. Starting the goroutine first
// and then waiting for a place parks one goroutine per item, unbounded, under
// a burst (F4 fixed that once in the webhook sender; business/Hooks had it
// back). slots is the caller's own pool, shared by its call sites.
func GoSafeBounded(slots chan struct{}, name string, fn func()) {
	slots <- struct{}{}
	goSafe("helpers.GoSafeBounded["+name+"]", true, func() {
		defer func() { <-slots }()
		fn()
	})
}

// TryGoSafeBounded is GoSafeBounded for work that has somewhere else to go:
// it starts fn only if slots has a free place at once, and says whether it
// did. A caller on a request's path hands the rest to its queue instead of
// waiting (business/Hooks leaves a full pool's deliveries to the drain).
func TryGoSafeBounded(slots chan struct{}, name string, fn func()) bool {
	select {
	case slots <- struct{}{}:
	default:
		return false
	}
	goSafe("helpers.TryGoSafeBounded["+name+"]", true, func() {
		defer func() { <-slots }()
		fn()
	})
	return true
}

// GoSafeLoop is GoSafeNamed for a goroutine that lasts as long as its context
// rather than one piece of work: a sweep on a ticker, a repair that retries for
// hours, a rebuild that runs for six. It is recovered the same way, and is not
// counted as work in flight: SettleBackground waits for work handed off, and a
// loop that ends only with its context would hold every wait to its bound.
func GoSafeLoop(name string, fn func()) { goSafe("helpers.GoSafeLoop["+name+"]", false, fn) }

// goSafe runs fn on a goroutine of its own behind the one recover guard the
// family shares, logging a panic under label. counted is whether it is work
// in flight, which SettleBackground waits for.
func goSafe(label string, counted bool, fn func()) {
	if counted {
		goSafeWork.Add(1)
	}
	go func() {
		if counted {
			defer goSafeWork.Add(-1)
		}
		defer logRecovered(label)
		fn()
	}()
}

// logRecovered, deferred, swallows a panic and logs it with its stack.
func logRecovered(label string) {
	if r := recover(); r != nil {
		LogErrorWithContext(context.Background(), "%s recovered panic: %v\n%s", label, r, debug.Stack())
	}
}

// GoSafeSend runs fn in a goroutine and delivers its result on ch, converting a
// panic into the ZERO VALUE rather than a process exit.
//
// WHY THIS SHAPE
// --------------
// Several read paths fan out to independent sources and join them with a
// deadline — the home screen's briefing and attention queues are the hot
// examples, and both document that any source which errors or times out is
// simply omitted. That contract was not actually held: an unrecovered panic in
// ANY goroutine terminates the whole Go process, so a malformed response from an
// external calendar or mail API would take the server down for every user on a
// home-page load. A recover in the caller cannot help, because recover only
// catches panics on its own goroutine.
//
// The zero value is delivered deliberately instead of sending nothing. Those
// callers loop once per source over a select, so a silent source would leave
// them waiting for the whole remaining budget on every panic. Sending the zero
// value keeps the join prompt, and for the slice results these paths use it
// appends nothing — which is exactly the "that source contributed nothing"
// degradation the functions already promise.
//
// ch should be buffered (cap >= 1) so a caller that has already given up on the
// deadline does not leak this goroutine on the send.
func GoSafeSend[T any](name string, ch chan<- T, fn func() T) {
	goSafeWork.Add(1)
	go func() {
		defer goSafeWork.Add(-1)
		var out T
		defer func() {
			r := recover()
			// Send BEFORE logging. Ordering matters: if the log call were to
			// panic (it dereferenced a nil global logger until that was guarded
			// at the source) the caller would never receive its value and would
			// stall for the rest of its deadline on top of the crash. Sending
			// first means the join is unaffected by anything the log path does.
			ch <- out
			if r != nil {
				LogErrorWithContext(context.Background(),
					"helpers.GoSafeSend[%s] recovered panic: %v\n%s",
					name, r, debug.Stack())
			}
		}()
		out = fn()
	}()
}

// RecoverToErr turns a panic in a channel-producing goroutine into an error on
// errCh instead of a process exit.
//
// ORDERING CONTRACT — read before using
// -------------------------------------
// It must be deferred AFTER the channel closes, so that LIFO ordering runs it
// FIRST and the send happens while errCh is still open:
//
//	go func() {
//	    defer close(out)
//	    defer close(errCh)
//	    defer helpers.RecoverToErr("jira.IterTasks", errCh) // last = runs first
//	    ...
//	}()
//
// Registered before the closes it would run last, after errCh is closed, and the
// send would panic on a closed channel — turning the guard into a second crash.
// The order above is the only correct one.
//
// WHY
// ---
// The import providers stream from third-party APIs (Jira, Notion, Asana, …) and
// decode responses whose shape they do not control, which is the classic source
// of an index-out-of-range or a failed type assertion. An unrecovered panic in
// any goroutine terminates the whole Go process, and the orchestrator's recover
// cannot help because it is on a different goroutine. So one malformed upstream
// response would take the workspace down for every user.
//
// Reporting the panic as an error rather than only logging it matters: the
// consumer selects on errCh, so the import job fails visibly with a message and
// the chunk records it, instead of quietly completing with zero items — which
// would look like "the source had no data" and lose work silently.
//
// The send is non-blocking so a producer whose error slot is already occupied
// cannot deadlock on the way out.
func RecoverToErr(name string, errCh chan<- error) {
	if r := recover(); r != nil {
		LogErrorWithContext(context.Background(),
			"helpers.RecoverToErr[%s] recovered panic: %v\n%s", name, r, debug.Stack())
		select {
		case errCh <- fmt.Errorf("%s panicked: %v", name, r):
		default:
		}
	}
}
