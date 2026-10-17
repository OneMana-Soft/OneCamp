package business

// A send tried again is the same send.
//
// A message is on screen the moment Send is pressed, and goes out with the id
// the app gave it (client_msg_id). When the answer to it was lost (a timeout,
// a proxy's 502 turned into "Network Error") the app said "Not sent" and
// offered Try again, which posted it again: and the server, which had the
// first one, made a second copy that everyone saw twice.
//
// So a send is done once per person, conversation and client id, for a day.
// A repeat of one that went through gets its answer; a repeat of one still
// going waits for it; a repeat of one that failed is tried afresh.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/redis/go-redis/v9"
)

const (
	// sendOnceFor is how long a send's answer is kept for a repeat.
	sendOnceFor = 24 * time.Hour
	// sendOnceLease is how long a send under way holds its key: longer than
	// any send takes, and short, so one that died on the way (the server
	// stopped mid-send) holds back its Try again for a minute, not the day
	// an answer is kept.
	sendOnceLease = time.Minute
	// sendOnceGoing marks a send still under way.
	sendOnceGoing = "going"
)

// sendOnceWait is how long a repeat waits for the first try to finish.
var sendOnceWait = 10 * time.Second

// clientMsgID is the shape of an id an app gives a message (the web app's
// look like local-mf3k2x1a-1b).
var clientMsgID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// onceStore keeps each send's state: claimed (going), its answer, or nothing,
// each for as long as it's given.
type onceStore interface {
	// claim takes the key for a send about to start, for ttl; false when
	// it's taken.
	claim(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// get is what the key holds, and whether it holds anything.
	get(ctx context.Context, key string) (string, bool, error)
	put(ctx context.Context, key, value string, ttl time.Duration) error
	release(ctx context.Context, key string) error
}

// redisOnce keeps it in Redis.
type redisOnce struct{ c *redis.Client }

func (r redisOnce) claim(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return r.c.SetNX(ctx, key, sendOnceGoing, ttl).Result()
}

func (r redisOnce) get(ctx context.Context, key string) (string, bool, error) {
	v, err := r.c.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (r redisOnce) put(ctx context.Context, key, value string, ttl time.Duration) error {
	return r.c.Set(ctx, key, value, ttl).Err()
}

func (r redisOnce) release(ctx context.Context, key string) error {
	return r.c.Del(ctx, key).Err()
}

// theOnceStore is where sends are kept, or nil when there's nowhere.
var theOnceStore = func() onceStore {
	if redisInit.RedisClient == nil {
		return nil
	}
	return redisOnce{redisInit.RedisClient}
}

// Once runs send once for a person, a conversation (where: a channel's id, a
// DM's other person, a group's id) and the id their app gave the message.
// Ids from two of the person's tabs could meet; in two conversations they're
// two sends. With no id, or nowhere to keep it, it just sends: a store that
// can't be reached never stops a message.
func Once[R any](ctx context.Context, userUUID, where, clientID string, send func() (R, error)) (R, error) {
	store := theOnceStore()
	if store == nil || userUUID == "" || len(where) > 128 || !clientMsgID.MatchString(clientID) {
		return send()
	}
	return once(ctx, store, registry.SendOnce.Build(userUUID, where, clientID), send)
}

func once[R any](ctx context.Context, store onceStore, key string, send func() (R, error)) (R, error) {
	var zero R
	claimed, err := store.claim(ctx, key, sendOnceLease)
	if err != nil {
		return send()
	}
	if claimed {
		finished := false
		defer func() {
			if !finished {
				// The send panicked: a repeat tries afresh.
				_ = store.release(context.WithoutCancel(ctx), key)
			}
		}()
		answer, err := send()
		finished = true
		if err != nil {
			// Failed: a repeat tries afresh.
			_ = store.release(context.WithoutCancel(ctx), key)
			return zero, err
		}
		raw, err := json.Marshal(answer)
		if err == nil {
			err = store.put(context.WithoutCancel(ctx), key, string(raw), sendOnceFor)
		}
		if err != nil {
			// Sent, but a repeat can't be given its answer: once the lease
			// ends it sends again.
			helpers.LogErrorWithContext(ctx, "business/Send once couldn't keep the answer for %s: %v", key, err)
		}
		return answer, nil
	}
	// A repeat: the first try's answer, once it has one.
	deadline := time.Now().Add(sendOnceWait)
	for {
		v, ok, err := store.get(ctx, key)
		switch {
		case err != nil:
			// Whether the first try went is unknown: the app tries again
			// shortly, as for a store that's down.
			return zero, reject(http.StatusServiceUnavailable, "Couldn't send the message. Try again in a moment.", err)
		case !ok:
			// The first try failed and let go: this one is the send.
			return once(ctx, store, key, send)
		case v != sendOnceGoing:
			var answer R
			if err := json.Unmarshal([]byte(v), &answer); err != nil {
				return zero, err
			}
			return answer, nil
		}
		if time.Now().After(deadline) {
			return zero, reject(http.StatusConflict, "This message is still being sent", nil)
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
