package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// Items is the record of a batch that applies its items one by one (the
// public API's fields and views batches: checked whole, then applied item by
// item, main's ruling of 3 Nov). Such a batch can stop part way, when the
// store fails under one item. Its key makes the retry safe: every item done
// is recorded under the caller's key with its answer, at once, so the same
// batch sent again with the same key skips those and answers them as before.
// One send holds the key at a time (a second meanwhile is refused as still on
// its way), and the same key with another batch is refused, as Middleware
// refuses a reused Idempotency-Key. Without Redis the batch isn't run: a
// record it can't keep is a retry it can't make safe (no record, no action).

var (
	// ErrItemsUnavailable: the record can't be kept just now; nothing ran.
	ErrItemsUnavailable = errors.New("idempotency: a batch's record can't be kept just now")
	// ErrItemsReused: the key came with another batch in the last 24 hours.
	ErrItemsReused = errors.New("idempotency: this key was sent with another batch")
	// ErrItemsInProgress: a send with this key is still being applied.
	ErrItemsInProgress = errors.New("idempotency: a batch with this key is still being applied")
)

// claimFor bounds a send's hold on its key: past it, the key of a send that
// died without letting go (a crash) is free for the retry.
const claimFor = 10 * time.Minute

// itemsKept is a batch's record: what makes two sends the same batch (its
// arguments' hash), and the answer of each item done, by its place.
type itemsKept struct {
	BodyHash string                  `json:"body_hash"`
	Done     map[int]json.RawMessage `json:"done"`
}

// itemsStore is where records live: Redis, or a test's own (the seam).
type itemsStore interface {
	claim(ctx context.Context, args []string) (bool, error)
	read(ctx context.Context, args []string) (string, bool, error)
	write(ctx context.Context, args []string, value string) error
	release(ctx context.Context, args []string)
}

type redisItems struct{}

func (redisItems) claim(ctx context.Context, args []string) (bool, error) {
	if !redisStore.IsAvailable() {
		return false, ErrItemsUnavailable
	}
	return redisStore.SetStringIfAbsent(ctx, registry.ApiBatchClaim, args, "1", claimFor)
}

func (redisItems) read(ctx context.Context, args []string) (string, bool, error) {
	return redisStore.GetString(ctx, registry.ApiBatchItems, args)
}

func (redisItems) write(ctx context.Context, args []string, value string) error {
	return redisStore.SetStringWithTTL(ctx, registry.ApiBatchItems, args, value, keptFor)
}

func (redisItems) release(ctx context.Context, args []string) {
	_ = redisStore.Delete(ctx, registry.ApiBatchClaim, args)
}

var records itemsStore = redisItems{}

// Items is one send's hold on a batch's key, and what the batch has done
// under it.
type Items struct {
	args []string
	kept itemsKept
}

// OpenItems claims key for caller's batch on route, whose arguments hash to
// hash (Fingerprint), and reads what the batch did under it before. The
// caller lets the key go with Release when the send ends.
func OpenItems(ctx context.Context, caller, route, key, hash string) (*Items, error) {
	args := []string{caller, route, key}
	claimed, err := records.claim(ctx, args)
	if err != nil {
		return nil, ErrItemsUnavailable
	}
	if !claimed {
		return nil, ErrItemsInProgress
	}
	it := &Items{args: args, kept: itemsKept{BodyHash: hash, Done: map[int]json.RawMessage{}}}
	raw, found, err := records.read(ctx, args)
	var before itemsKept
	switch {
	case err != nil, found && json.Unmarshal([]byte(raw), &before) != nil:
		it.Release(ctx)
		return nil, ErrItemsUnavailable
	case found && before.BodyHash != hash:
		it.Release(ctx)
		return nil, ErrItemsReused
	case found && before.Done != nil:
		it.kept.Done = before.Done
	}
	return it, nil
}

// Done is the answer item i gave when the batch did it, if it did.
func (it *Items) Done(i int) (json.RawMessage, bool) {
	a, ok := it.kept.Done[i]
	return a, ok
}

// Mark records item i done, with its answer, before the batch goes on. An
// error is a record that didn't take it: the batch stops there.
func (it *Items) Mark(ctx context.Context, i int, answer json.RawMessage) error {
	it.kept.Done[i] = answer
	b, err := json.Marshal(it.kept)
	if err != nil {
		return err
	}
	return records.write(context.WithoutCancel(ctx), it.args, string(b))
}

// Release lets the key go for the next send; the record keeps its 24 hours.
func (it *Items) Release(ctx context.Context) {
	records.release(context.WithoutCancel(ctx), it.args)
}

// Fingerprint is what makes two sends of a batch the same: the hash of its
// arguments as JSON, whose keys are sorted, so REST's and MCP's hash alike.
func Fingerprint(args map[string]any) string {
	b, _ := json.Marshal(args)
	return fingerprint("application/json", b)
}
