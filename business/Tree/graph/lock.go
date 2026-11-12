package graph

// One change to a tree's shape at a time, across every API instance: a move,
// a delete or a restore of a subtree takes the tree's lock, reads what it
// needs, writes, and lets go. Two moves that would close a loop together
// (A under B while B goes under A) each look fine alone; one after the other,
// the second sees the first. The lock is in Redis so it holds across
// instances; without Redis (a single instance, or Redis down) it's held in
// this process, and the stamp every move writes (graph.go) still keeps two
// instances from closing a loop.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

const (
	// lockTTL bounds a lock whose holder died: no tree change takes this long.
	lockTTL = 15 * time.Second
	// lockWait is how long a change waits for the one before it.
	lockWait = 8 * time.Second
)

// ErrBusy is a tree whose lock wasn't free within lockWait.
var ErrBusy = errors.New("tree: another change to these pages is still going; try again")

var local sync.Map // kind -> *sync.Mutex

// Lock takes the tree's lock for kind and returns what lets it go, which
// may be called more than once (a caller that lets go early and defers it).
func Lock(ctx context.Context, kind string) (func(), error) {
	unlock, err := lock(ctx, kind)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(unlock) }, nil
}

func lock(ctx context.Context, kind string) (func(), error) {
	mu, _ := local.LoadOrStore(kind, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	// The process lock first: changes from this instance queue here, and only
	// one of them at a time waits on Redis.
	if !lockWithin(ctx, m, lockWait) {
		return nil, ErrBusy
	}
	if !redisStore.IsAvailable() {
		return m.Unlock, nil
	}
	args := []string{kind}
	token := uuid.NewString()
	deadline := time.Now().Add(lockWait)
	for {
		ok, err := redisStore.SetStringIfAbsent(ctx, registry.TreeLock, args, token, lockTTL)
		if err != nil {
			// Redis can't be asked: this instance's lock is what there is.
			return m.Unlock, nil
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			m.Unlock()
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
			m.Unlock()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return func() {
		// Only while this holder still has it: one that outlived lockTTL
		// can't free the next one's lock.
		_, _ = redisStore.DeleteIfHeld(context.Background(), registry.TreeLock, args, token)
		m.Unlock()
	}, nil
}

func lockWithin(ctx context.Context, m *sync.Mutex, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if m.TryLock() {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}
