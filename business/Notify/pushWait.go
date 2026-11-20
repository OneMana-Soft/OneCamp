package business

// The phone waits while you're in OneCamp (A4 H4, slack TN-87): Plan keeps a
// chat push back while its person is in OneCamp (wait, plan.go), Deliver
// writes it as a 'waiting' row due at the end of their notify.push_delay and
// keeps its payload here, in Redis, as notify:wait:<event>:<person> (Postgres
// stays ids only), and the Sweeper's notify.push.wait sends it when due,
// unless they read the message meanwhile (business/ReadState, their marker
// where it was written), it was deleted (Retract dropped it), its payload is
// gone, or a hold began; then it is dropped and never sent.
//
// The payload holds only what the push shows (notificationBusiness
// PushPreview: its title, a short preview and its ids), where it was and
// when: never anything else, and it is never logged. It is forgotten when its
// push is sent or dropped, and its TTL (the wait and a grace) ends it if
// nothing else does.
//
// Permission model: the push goes only to the row's own person's devices,
// through the same push half and gate as every push (business/Notification),
// exactly as it would have gone at once; nothing here is served over HTTP.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	deliveryModels "github.com/akashc777/OneCamp/models/postgres/NotificationDelivery"
	"github.com/redis/go-redis/v9"
)

// waitingPush is a waiting push's payload.
type waitingPush struct {
	// Push is what the push shows (PushPreview).
	Push map[string]string `json:"push"`
	// Place is where it was written (kind and id), for the read check.
	Place Place `json:"place"`
	// At is when the router told of it: a read at or after it read it.
	At time.Time `json:"at"`
}

const (
	// waitJob is the Sweeper's job, and how often it passes.
	waitJob   = "notify.push.wait"
	waitEvery = 15 * time.Second
	// claimBatch is the most waiting pushes one pass takes.
	claimBatch = 200
	// payloadGrace is how long a payload outlives its wait, for a pass that
	// comes late.
	payloadGrace = 10 * time.Minute
)

// errNoPayloadStore is Redis not configured: nothing can wait, so it goes now.
var errNoPayloadStore = errors.New("notify: no store for a waiting push")

// waitKey is the payload's key: the event and its person.
func waitKey(eventID, userID string) string {
	return "notify:wait:" + eventID + ":" + strings.ToLower(userID)
}

// keptPayload is one waiting push's payload as it is kept: its key, its
// value and how long it lasts.
type keptPayload struct {
	key   string
	value []byte
	ttl   time.Duration
}

// Seams for tests: the payload store, the rows and the holds.
var (
	// keepPayloads keeps an event's waiting payloads in one round trip.
	keepPayloads = func(ctx context.Context, ps []keptPayload) error {
		rdb := redisInit.RedisClient
		if rdb == nil {
			return errNoPayloadStore
		}
		_, err := rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, p := range ps {
				pipe.Set(ctx, p.key, p.value, p.ttl)
			}
			return nil
		})
		return err
	}
	// payloadsOf reads a pass's payloads in one round trip, nil for one gone.
	payloadsOf = func(ctx context.Context, keys []string) ([][]byte, error) {
		out := make([][]byte, len(keys))
		rdb := redisInit.RedisClient
		if rdb == nil || len(keys) == 0 {
			return out, nil
		}
		got, err := rdb.MGet(ctx, keys...).Result()
		if err != nil {
			return nil, err
		}
		for i, v := range got {
			if s, ok := v.(string); ok {
				out[i] = []byte(s)
			}
		}
		return out, nil
	}
	forgetPayloads = func(ctx context.Context, keys ...string) {
		rdb := redisInit.RedisClient
		if rdb == nil {
			return
		}
		if err := rdb.Del(context.WithoutCancel(ctx), keys...).Err(); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Notify: could not forget %d waiting pushes (their TTL ends them): %v", len(keys), err)
		}
	}
	claimDue     = deliveryModels.ClaimDue
	stillWaiting = deliveryModels.Waiting
	// heldOf is the router's own read of holds (Plan's Held): one read a pass.
	heldOf = liveInputs{}.Held
)

// readSince is when someone last read where (their read marker; nil for
// never), and whether they may read it at all. business/ReadState gives it
// (ReadWith): it can't be imported here. Until it is, nothing is read and
// every waiting push is sent.
var readSince func(ctx context.Context, userID string, where Place) (since *time.Time, mayRead bool, err error)

// ReadWith gives the Sweeper the read check, from business/ReadState's init.
func ReadWith(read func(ctx context.Context, userID string, where Place) (*time.Time, bool, error)) {
	readSince = read
}

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: waitJob, Every: waitEvery, Run: sendWaitingPushes})
}

// keepWaiting keeps the payload of each of ev's newly waiting pushes until
// its wait ends, and a grace after, in one write. When they can't be kept
// (Redis blinks, or isn't there), they go now through the one path, their
// rows marked sent: a wait never loses a push. Answers ds as told.
func (r router) keepWaiting(ctx context.Context, ev Event, ds []Decision) []Decision {
	var waiting []int
	for i, d := range ds {
		if len(d.Waiting) > 0 {
			waiting = append(waiting, i)
		}
	}
	if len(waiting) == 0 {
		return ds
	}
	at := r.now().UTC()
	value, err := json.Marshal(waitingPush{Push: notificationBusiness.PushPreview(ev.Push), Place: Place{Kind: ev.Place.Kind, ID: ev.Place.ID}, At: at})
	if err == nil {
		ps := make([]keptPayload, len(waiting))
		for j, i := range waiting {
			ttl := payloadGrace
			if till := ds[i].WaitTill; till != nil {
				ttl += till.Sub(at)
			}
			ps[j] = keptPayload{key: waitKey(ev.ID, ds[i].UserID), value: value, ttl: ttl}
		}
		err = keepPayloads(ctx, ps)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Notify Deliver %s: could not keep %d waiting pushes, pushing them now: %v", ev.ID, len(waiting), err)
		return pushWaitingNow(ctx, ev.ID, ds, waiting)
	}
	forgetRetracted(ctx, ev, ds, waiting)
	return ds
}

// pushWaitingNow sends the waiting pushes of ds (at waiting) now instead, and
// marks their rows sent in one write. Answers ds.
func pushWaitingNow(ctx context.Context, eventID string, ds []Decision, waiting []int) []Decision {
	var rows []deliveryModels.Row
	for _, i := range waiting {
		for _, to := range ds[i].Waiting {
			rows = append(rows, deliveryModels.Row{EventID: eventID, UserID: ds[i].UserID, Delivery: string(to)})
		}
		ds[i] = pushingNow(ds[i : i+1])[0]
	}
	if _, err := settleRows(ctx, rows, "waiting", "sent"); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Notify Deliver %s: could not mark %d pushes sent now: %+v", eventID, len(rows), err)
	}
	return ds
}

// forgetRetracted forgets the payload of each push a Retract dropped between
// its 'waiting' row and its payload (the message deleted at that moment):
// that Retract found nothing to forget. A Retract after this read forgets
// them itself. An event about no message is never retracted.
func forgetRetracted(ctx context.Context, ev Event, ds []Decision, waiting []int) {
	if ev.MessageID == "" {
		return
	}
	still, err := stillWaiting(ctx, ev.ID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Notify Deliver %s: could not read whether its pushes still wait (a TTL ends any left): %+v", ev.ID, err)
		return
	}
	var gone []string
	for _, i := range waiting {
		if !slices.Contains(still, key(ds[i].UserID)) {
			gone = append(gone, waitKey(ev.ID, ds[i].UserID))
		}
	}
	if len(gone) > 0 {
		forgetPayloads(ctx, gone...)
	}
}

// sendWaitingPushes is one pass of notify.push.wait: it claims the waiting
// pushes now due, each at most once whoever else passes at the same moment
// (ClaimDue: a crash after the claim loses that push, never sends it twice),
// reads their payloads and their people's holds in one read each, drops each
// one cancelled, and sends the rest, each event's to its people in one call
// (each person's own devices alone). Every payload it took is forgotten.
func sendWaitingPushes(ctx context.Context) error {
	rows, err := claimDue(ctx, claimBatch)
	if err != nil || len(rows) == 0 {
		return err
	}
	keys := make([]string, len(rows))
	people := make([]string, 0, len(rows))
	for i, row := range rows {
		keys[i] = waitKey(row.EventID, row.UserID)
		if !slices.Contains(people, row.UserID) {
			people = append(people, row.UserID)
		}
	}
	raws, err := payloadsOf(ctx, keys)
	if err != nil {
		// Redis blinked: they wait for the next pass, which their TTL outlasts.
		helpers.LogErrorWithContext(ctx, "business/Notify %s: could not read %d waiting pushes, trying again: %v", waitJob, len(rows), err)
		if _, err := settleRows(ctx, rows, "sent", "waiting"); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Notify %s: %+v", waitJob, err)
		}
		return nil
	}
	now := time.Now()
	held, err := heldOf(ctx, people, now)
	if err != nil {
		// As a failed read of who waits pushes now, a failed read of holds
		// sends: the push gate below still holds a pause.
		helpers.LogErrorWithContext(ctx, "business/Notify %s: could not read holds, sending: %+v", waitJob, err)
		held = nil
	}
	var dropped []deliveryModels.Row
	var events []string
	sends := map[string]*eventPush{}
	for i, row := range rows {
		var p waitingPush
		if raws[i] == nil || json.Unmarshal(raws[i], &p) != nil || held[key(row.UserID)].Reason != "" || readFirst(ctx, row.UserID, p) {
			dropped = append(dropped, row)
			continue
		}
		s := sends[row.EventID]
		if s == nil {
			s = &eventPush{push: p.Push}
			sends[row.EventID] = s
			events = append(events, row.EventID)
		}
		s.to = append(s.to, row.UserID)
	}
	if _, err := settleRows(ctx, dropped, "sent", "dropped"); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Notify %s: %+v", waitJob, err)
	}
	for _, id := range events {
		if err := deliverHalf(ctx, notificationBusiness.PushHalf, sends[id].to, sends[id].push, id); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Notify %s %s: %+v", waitJob, id, err)
		}
	}
	forgetPayloads(ctx, keys...)
	return nil
}

// eventPush is one event's waiting push as a pass sends it: what it shows
// (every person's payload of an event is the same) and to whom.
type eventPush struct {
	push map[string]string
	to   []string
}

// readFirst is whether a waiting push's person read the message meanwhile
// (their marker where it was is at or after it) or may no longer read it. A
// read that fails sends it.
func readFirst(ctx context.Context, userID string, p waitingPush) bool {
	if readSince != nil {
		since, mayRead, err := readSince(ctx, userID, p.Place)
		switch {
		case err != nil:
			helpers.LogErrorWithContext(ctx, "business/Notify %s: could not read whether it was read, sending: %+v", waitJob, err)
		case !mayRead:
			return true
		case since != nil && !since.Before(p.At):
			return true
		}
	}
	return false
}
