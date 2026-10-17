package helpers

import "sync"

// InOrder runs the work handed to it for one key one piece at a time, in the
// order it was handed over, while work for other keys runs alongside. Each
// piece runs on a goroutine of its own started with GoSafeNamed: recovered,
// counted as work in flight (SettleBackground waits for it), and never waited
// for by the caller.
//
// It is for background writes where the last one asked for must be the last
// one done. A channel archived and then restored started its search mark and
// its unmark on a goroutine each, and a restore whose unmark finished first
// left the live channel's whole history marked Archived. KeyedLock serialises
// too, but a goroutine takes it when it is scheduled rather than when it was
// started, so two started back to back may take it in either order; here a
// piece's place is taken when it is handed over.
//
// The zero value is ready to use. A key's entry is dropped once its last piece
// is done, so a long-lived process that sees many keys keeps none of them.
type InOrder struct {
	mu   sync.Mutex
	last map[string]chan struct{} // closed when the key's latest piece is done
}

// Go hands fn over, to run once every piece handed over before it for key is
// done. name labels it in the log if it panics; a piece that panics is logged,
// and the pieces after it still run.
func (o *InOrder) Go(key, name string, fn func()) {
	done := make(chan struct{})
	o.mu.Lock()
	if o.last == nil {
		o.last = make(map[string]chan struct{})
	}
	before := o.last[key]
	o.last[key] = done
	o.mu.Unlock()

	GoSafeNamed(name, func() {
		// Deferred, so a piece that panics still lets the next one go.
		defer func() {
			o.mu.Lock()
			if o.last[key] == done {
				delete(o.last, key)
			}
			o.mu.Unlock()
			close(done)
		}()
		if before != nil {
			<-before
		}
		fn()
	})
}
