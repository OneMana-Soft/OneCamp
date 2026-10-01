package ai

// Whose lifetime a streamed answer has.
//
// THE FAILURE THIS ENDS. The model call ran on the request's context, so the
// moment the client went away the call was cancelled, the handler returned on
// the error path, and nothing was saved. A closed tab, a phone that locked, a
// train tunnel, a navigation to another screen: each one threw away an answer
// the model had already half written and been paid for, and left the
// conversation with a question that was never answered. The person came back,
// found nothing, and asked again. Google's AX calls the property this lacks
// "connection recovery": the run belongs to the server, and a client that
// reconnects gets what it missed.
//
// So the model runs on a context the client cannot cancel. What the client CAN
// do is ask for the answer to stop, which is a different thing from going away,
// and the two are told apart here: a stop request lands in Redis under the
// session, and a handler whose client has gone checks for it. Present, and from
// the session's owner: generation stops and what was written so far is kept.
// Absent: generation finishes, is saved, and is there when the person returns.
//
// The stop request travels on its own connection, so it can arrive a moment
// after the disconnect it was sent with. The check therefore keeps polling for
// a few seconds after the client goes, then more slowly for as long as the
// answer takes, so a late stop still stops.

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

const (
	// stopPollFast is how often the stop flag is checked in the first moments
	// after the client goes, when a deliberate stop is most likely in flight.
	stopPollFast = 250 * time.Millisecond
	// stopPollFastFor is how long the fast polling lasts.
	stopPollFastFor = 3 * time.Second
	// stopPollSlow is the cadence afterwards, for the rest of the answer.
	stopPollSlow = 2 * time.Second
)

// RequestStreamStop records that requester wants the answer for sessionID to
// stop. It is honoured only by a stream that requester owns.
func RequestStreamStop(ctx context.Context, sessionID, requester string) error {
	if sessionID == "" || requester == "" || !redisStore.IsAvailable() {
		return nil
	}
	return redisStore.SetString(ctx, registry.AIStreamStop, []string{sessionID}, requester)
}

// stopRequests is where stop requests live. An interface so the lifetime can
// be tested against a fake; Redis in production, because a stop must reach the
// replica that holds the stream, which is not necessarily the one that took
// the request.
type stopRequests interface {
	// take returns who asked for this session's answer to stop, and consumes
	// the request so it cannot be honoured twice.
	take(ctx context.Context, sessionID string) string
	// clear discards any pending request. Called when a new answer starts: a
	// stop meant for the previous answer must not end this one.
	clear(ctx context.Context, sessionID string)
}

type redisStopRequests struct{}

func (redisStopRequests) take(ctx context.Context, sessionID string) string {
	if !redisStore.IsAvailable() {
		return ""
	}
	who, _, _ := redisStore.GetDelString(ctx, registry.AIStreamStop, []string{sessionID})
	return who
}

func (redisStopRequests) clear(ctx context.Context, sessionID string) {
	if redisStore.IsAvailable() {
		_ = redisStore.Delete(ctx, registry.AIStreamStop, []string{sessionID})
	}
}

// StreamLive says whether an answer for this session is being written right
// now. A client restoring the conversation uses it to know there is something
// to wait for.
func StreamLive(ctx context.Context, sessionID string) bool {
	if sessionID == "" || !redisStore.IsAvailable() {
		return false
	}
	_, ok, _ := redisStore.GetString(ctx, registry.AIStreamLive, []string{sessionID})
	return ok
}

func markStreamLive(ctx context.Context, sessionID string) {
	if sessionID != "" && redisStore.IsAvailable() {
		_ = redisStore.SetString(ctx, registry.AIStreamLive, []string{sessionID}, "1")
	}
}

func clearStreamLive(ctx context.Context, sessionID string) {
	if sessionID != "" && redisStore.IsAvailable() {
		_ = redisStore.Delete(ctx, registry.AIStreamLive, []string{sessionID})
	}
}

// StreamLifetime separates how long the model may run from how long the
// client is listening.
type StreamLifetime struct {
	// Gen is the context for the model call and everything that builds the
	// answer. The client going away does not cancel it; a deliberate stop from
	// the owner, or the stream timeout, does.
	Gen context.Context

	cancel  context.CancelFunc
	client  context.Context
	session string
	owner   string
	gone    atomic.Bool
	stopped atomic.Bool

	// stops is where the watcher asks who requested a stop. A field so tests
	// can answer without Redis.
	stops stopRequests
}

// NewStreamLifetime starts watching the client. Gen carries client's values
// (the model limits, the notice sink) but not its cancellation, under its own
// timeout. Call Close when the answer has been saved.
func NewStreamLifetime(client context.Context, sessionID, owner string, timeout time.Duration) *StreamLifetime {
	return newStreamLifetime(client, sessionID, owner, timeout, redisStopRequests{})
}

func newStreamLifetime(client context.Context, sessionID, owner string, timeout time.Duration, stops stopRequests) *StreamLifetime {
	gen, cancel := context.WithTimeout(context.WithoutCancel(client), timeout)
	l := &StreamLifetime{Gen: gen, cancel: cancel, client: client, session: sessionID, owner: owner, stops: stops}
	// A stop sent for the previous answer that nobody consumed (the answer had
	// already finished) would otherwise end this one the moment its client
	// went away. Every answer starts with a clean slate.
	stops.clear(gen, sessionID)
	markStreamLive(gen, sessionID)
	go l.watch()
	return l
}

// watch waits for the client to go, then decides between "finish and save"
// and "stop now" by asking for a stop request, quickly at first and then at a
// walking pace for as long as the answer takes.
func (l *StreamLifetime) watch() {
	select {
	case <-l.client.Done():
	case <-l.Gen.Done():
		return
	}
	l.gone.Store(true)

	started := time.Now()
	for {
		if who := l.stops.take(l.Gen, l.session); who != "" {
			if who == l.owner {
				l.stopped.Store(true)
				l.cancel()
			}
			// A stranger's stop is dropped, and consumed so it cannot be tried
			// again against the next answer either.
			return
		}
		wait := stopPollSlow
		if time.Since(started) < stopPollFastFor {
			wait = stopPollFast
		}
		select {
		case <-l.Gen.Done():
			return
		case <-time.After(wait):
		}
	}
}

// ClientGone says whether anyone is still reading the stream. Writes to a
// gone client are wasted work, not errors, so callers skip them.
func (l *StreamLifetime) ClientGone() bool { return l.gone.Load() }

// Stopped says whether the owner asked for the answer to stop. What was
// written before that is theirs to keep; the loop that sees it saves the
// partial answer rather than discarding it as a failure.
func (l *StreamLifetime) Stopped() bool { return l.stopped.Load() }

// Close ends generation and says the session is no longer live. Idempotent.
func (l *StreamLifetime) Close() {
	l.cancel()
	clearStreamLive(context.WithoutCancel(l.Gen), l.session)
}
