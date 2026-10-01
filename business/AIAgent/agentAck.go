package business

// The "On it…" acknowledgement, on a delay.
//
// A durable run has to announce itself: silence after someone @mentions an agent
// reads as being ignored. But announcing INSTANTLY is its own problem — a
// question the agent answers in two seconds produced an "On it, I'll post back
// when I'm done" comment that flickered into the answer a moment later, which
// looks like a bug and clutters the thread. That flicker is the main reason the
// durable path felt worse than a plain synchronous reply for quick work.
//
// So the ack is armed on a short timer and cancelled by the first real message.
// Fast work posts exactly one comment (its answer); only genuinely slow work
// says "On it". Cancel WAITS for an ack that is already mid-post, so a late ack
// can never overwrite the real result — the invariant that makes this safe to
// put in front of every status post.

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultAgentAckDelay is how long a run may take before it owes the thread an
// acknowledgement. Chosen to sit just past a typical fast tool-less answer, and
// well under the point where a person starts wondering if anything happened.
const defaultAgentAckDelay = 4 * time.Second

// agentAckDelay is the slow-ack delay. AI_AGENT_ACK_DELAY_SECONDS tunes it; 0
// posts the acknowledgement immediately (the previous behaviour), which is also
// the escape hatch if an operator prefers an instant receipt.
func agentAckDelay() time.Duration {
	if v := strings.TrimSpace(os.Getenv("AI_AGENT_ACK_DELAY_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return defaultAgentAckDelay
}

// slowAck runs a one-shot action after a delay unless it is cancelled first.
// Generic: it knows nothing about agents or posting, so any "say something only
// if this takes a while" case can use it.
//
// The nil value is usable (Cancel is a no-op), so a caller can declare one
// unconditionally and arm it only when it applies.
type slowAck struct {
	mu      sync.Mutex
	timer   *time.Timer
	stopped bool
	done    chan struct{} // closed once a fired action has finished
}

// startSlowAck arms action to run after delay. A delay of 0 or less runs it
// immediately (still cancel-safe: Cancel then simply waits for it to finish).
func startSlowAck(delay time.Duration, action func()) *slowAck {
	if action == nil {
		return nil
	}
	a := &slowAck{done: make(chan struct{})}
	if delay < 0 {
		delay = 0
	}
	a.timer = time.AfterFunc(delay, func() {
		defer close(a.done)
		action()
	})
	return a
}

// Cancel prevents a pending action from running. If it has already started,
// Cancel BLOCKS until it finishes, so the caller's own output always lands after
// it — never the other way round. Idempotent and safe on a nil receiver.
func (a *slowAck) Cancel() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	a.stopped = true
	// Stop reports false when the timer already expired, which (since nothing
	// else stops it) means the action is running or about to run.
	stoppedInTime := a.timer.Stop()
	a.mu.Unlock()
	if !stoppedInTime {
		<-a.done
	}
}
