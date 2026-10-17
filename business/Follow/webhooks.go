package business

// Doc webhooks: doc.created, doc.updated, doc.deleted, doc.shared and
// doc.comment.created, to the workspace's outgoing webhooks that subscribe to
// them (business/Webhook DispatchEvent, which also runs in-process listeners).
//
// doc.updated is at most one a minute per doc: the first save of a doc opens a
// minute, every save in that minute joins it, and when it closes one
// doc.updated goes out, so a webhook reading the doc then reads every save of
// the minute. The open minutes are a Redis sorted set, shared by every
// instance, flushed by a claimed sweep (one instance flushes each pass);
// without Redis they're kept in this process.
//
// A payload carries ids, never a body, and a private doc's title stays out of
// it: a webhook's endpoint is outside the workspace. And a webhook is sent a
// doc's event only when whoever set it up can open the doc (or could, before
// it went to the trash), by the Access Resolver: a private doc's events reach
// no webhook of someone it isn't shared with (docs programme C4).

import (
	"context"
	"strconv"
	"sync"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// The doc events webhooks receive, as their own kinds; doc.updated is gathered
// first (pendingUpdates).
var webhookKinds = map[resourceEventBusiness.Kind]bool{
	resourceEventBusiness.DocCreated:        true,
	resourceEventBusiness.DocUpdated:        true,
	resourceEventBusiness.DocDeleted:        true,
	resourceEventBusiness.DocShared:         true,
	resourceEventBusiness.DocCommentCreated: true,
}

// updateWindow is how long saves of one doc are gathered into one doc.updated.
const updateWindow = time.Minute

// updatesKey holds each doc with an open minute, scored by when it closes
// (Unix milliseconds): the registry's DocUpdatedWebhooksDue.
var updatesKey = registry.DocUpdatedWebhooksDue.Build()

// Seams for tests: where webhooks go, the clock, and a webhook owner's graph
// node.
var (
	dispatchWebhook = func(ctx context.Context, kind string, data map[string]interface{}, mayKnow func(context.Context, uuid.UUID) bool) {
		webhookBusiness.DispatchEventFor(ctx, kind, data, mayKnow)
	}
	clock      = time.Now
	updates    = &pendingUpdates{due: map[string]time.Time{}}
	activeUser = userDomain.GetActiveDgraphUserInfoByUUID
)

// ownerCanOpen is, for a doc's event, whether a webhook's owner may know of
// it: they can open the doc, or could before it went to the trash.
func ownerCanOpen(docUUID string) func(context.Context, uuid.UUID) bool {
	return func(ctx context.Context, owner uuid.UUID) bool {
		u, err := activeUser(ctx, owner.String())
		if err != nil || u == nil || u.Uid == "" {
			return false
		}
		d, err := resolve(ctx, accessBusiness.Subject{UserUID: u.Uid, UserUUID: owner.String()}, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID})
		if err != nil {
			return false
		}
		return d.Level >= accessBusiness.LevelView || d.Deleted && d.Was >= accessBusiness.LevelView
	}
}

func isWebhook(e resourceEventBusiness.Event) bool {
	return webhookKinds[e.Kind] && e.ResourceKind == "doc"
}

// sendWebhook is the sink.
func sendWebhook(ctx context.Context, e resourceEventBusiness.Event) {
	if e.Kind == resourceEventBusiness.DocUpdated {
		updates.add(ctx, e.ResourceUUID, clock())
		return
	}
	doc, err := docPeople(ctx, e.ResourceUUID)
	if err != nil {
		doc = nil
	}
	dispatchWebhook(ctx, string(e.Kind), webhookPayload(e, doc), ownerCanOpen(e.ResourceUUID))
}

// webhookPayload is what a doc webhook carries. doc is the doc as read now, nil
// if it couldn't be. Pure.
func webhookPayload(e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc) map[string]interface{} {
	private := doc == nil || (doc.IsPrivate != nil && *doc.IsPrivate)
	p := map[string]interface{}{
		"doc_uuid":    e.ResourceUUID,
		"doc_private": private,
		"at":          e.At.UTC().Format(time.RFC3339Nano),
	}
	if doc != nil && !private {
		p["doc_title"] = doc.Title
	}
	if e.Actor.UserUUID != "" {
		p["actor_uuid"] = e.Actor.UserUUID
	}
	if e.Actor.AgentUUID != "" {
		p["agent_uuid"] = e.Actor.AgentUUID
	}
	switch e.Kind {
	case resourceEventBusiness.DocCreated:
		if parent := e.Data["parent"]; parent != "" {
			p["parent_uuid"] = parent
		}
	case resourceEventBusiness.DocShared:
		p["user_uuids"] = append([]string{}, e.Subjects...)
		if level := e.Data["level"]; level != "" {
			p["level"] = level
		}
	case resourceEventBusiness.DocCommentCreated:
		p["comment_uuid"] = e.Data["comment_uuid"]
	}
	return p
}

// pendingUpdates are the docs whose minute is open.
type pendingUpdates struct {
	mu  sync.Mutex
	due map[string]time.Time // without Redis
}

// add opens a minute for the doc, unless one is open.
func (p *pendingUpdates) add(ctx context.Context, docUUID string, now time.Time) {
	closes := now.Add(updateWindow)
	if rdb := redisInit.RedisClient; rdb != nil {
		// The set's expiry is renewed with each add, in the same round trip:
		// the flush empties it as minutes close, so it only expires after an
		// hour in which no instance flushed.
		pipe := rdb.TxPipeline()
		pipe.ZAddNX(ctx, updatesKey, redis.Z{Score: float64(closes.UnixMilli()), Member: docUUID})
		pipe.Expire(ctx, updatesKey, registry.DocUpdatedWebhooksDue.TTL)
		_, err := pipe.Exec(ctx)
		if err == nil {
			return
		}
		helpers.LogErrorWithContext(ctx, "business/Follow webhooks: Redis couldn't keep doc %s's open minute, so this instance does: %+v", docUUID, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, open := p.due[docUUID]; !open {
		p.due[docUUID] = closes
	}
}

// takeClosed takes the docs whose minute has closed, each once across every
// instance (removing a member from the set is what claims it).
func (p *pendingUpdates) takeClosed(ctx context.Context, now time.Time) []string {
	var out []string
	if rdb := redisInit.RedisClient; rdb != nil {
		ids, err := rdb.ZRangeByScore(ctx, updatesKey, &redis.ZRangeBy{Min: "-inf", Max: strconv.FormatInt(now.UnixMilli(), 10)}).Result()
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Follow webhooks: the closed minutes couldn't be read: %+v", err)
		}
		for _, id := range ids {
			if n, err := rdb.ZRem(ctx, updatesKey, id).Result(); err == nil && n == 1 {
				out = append(out, id)
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, closes := range p.due {
		if !closes.After(now) {
			out = append(out, id)
			delete(p.due, id)
		}
	}
	return out
}

// flushUpdates sends one doc.updated for each doc whose minute closed, unless
// the doc is gone (its webhooks heard doc.deleted).
func flushUpdates(ctx context.Context) error {
	for _, id := range updates.takeClosed(ctx, clock()) {
		doc, err := docPeople(ctx, id)
		if err != nil || doc == nil || helpers.IsSoftDeleted(doc.DeletedAt) {
			continue
		}
		dispatchWebhook(ctx, string(resourceEventBusiness.DocUpdated), webhookPayload(resourceEventBusiness.Event{
			Kind: resourceEventBusiness.DocUpdated, ResourceKind: "doc", ResourceUUID: id, At: clock(),
		}, doc), ownerCanOpen(id))
	}
	return nil
}

// DocUpdatedWebhooks is the sweep that sends doc.updated as each doc's minute
// closes, registered with the server's sweeps. A test runs it in a registry of
// its own, so only this job runs, on the test's clock, rather than in the
// server's, which runs every registered job (sweepBusiness.Jobs.StartAll).
var DocUpdatedWebhooks = sweepBusiness.Job{Name: "webhooks.doc.updated", Every: 15 * time.Second, Run: flushUpdates}

func init() {
	sweepBusiness.Jobs.Register(DocUpdatedWebhooks)
}
