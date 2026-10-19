package business

// A channel post's side effects as the outbox's sinks (chat A2): what today's
// CreatePost fires as goroutines, run from the post's event instead, at least
// once each, after a crash, on any process (business/Outbox).
//
// Each sink reads the post as it is when it runs (the event carries its id,
// never its words): one deleted since is skipped and its event marked done,
// and one edited since sends its words now. The sinks say what today's path
// says (golden_test.go holds them to it):
//
//   - realtime: the post to its channel's open apps (MQTT);
//   - search:   its entry and its files', written as an index, so a retry is
//     the same entry;
//   - notify:   the Activity item, push and email today's sendNewPostNotification
//     sends, each person told once (outbox FirstTime);
//   - events:   post.created to the listeners and webhooks (DispatchEvent).
//
// The AI edition's embedding is its own sink, in postEmbedSink.go.

import (
	"context"
	"errors"
	"fmt"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	domain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// MessageTopic is the outbox topic every message's events are on.
const MessageTopic = "message"

// The kinds of message event (business/ResourceEvent's names).
const MessageCreated = "message.created"

// The sinks' names: stable, since the outbox remembers which had an event by
// them.
const (
	sinkRealtime = "post.realtime"
	sinkSearch   = "post.search"
	sinkEmbed    = "post.embed"
	sinkNotify   = "post.notify"
	sinkEvents   = "post.events"
)

// The seams the sinks go out through, for tests.
var (
	readPost    = domain.ReadPostForSinks
	publishPost = mqttBusiness.PublishPost
	indexPost   = domain.IndexPostWithAttachments
	// createSearchEntry is today's path's search write (a create).
	createSearchEntry = domain.CreatePostWithAttachmentsInOpenSearch
	notifyPost        = notifyNewPost
	dispatchPost      = webhookBusiness.DispatchEvent
	firstTime         = outboxBusiness.FirstTime
)

// postRead keeps the post one event's sinks read, for the length of a drain's
// run of them: the four sinks of a post's other event, one read. A retry,
// later, reads it again.
var postRead = helpers.NewTTLCache[*dgraphStruct.DgraphPost](30 * time.Second)

// livePost is the post an event is about, as it is now, or nil when it's gone
// or deleted: its side effects are skipped, and the event is done.
func livePost(ctx context.Context, ev outboxBusiness.Event) (*dgraphStruct.DgraphPost, error) {
	if ev.Entities["kind"] != "channel" {
		return nil, nil
	}
	if p, ok := postRead.Get(ev.EventID); ok {
		return p, nil
	}
	p, err := readPost(ctx, ev.Entities["message"])
	if errors.Is(err, domain.ErrPostGone) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if helpers.IsSoftDeleted(p.DeletedAt) {
		p = nil
	}
	postRead.Set(ev.EventID, p)
	return p, nil
}

// replyOf is the post p quotes, when it is still there to quote.
func replyOf(p *dgraphStruct.DgraphPost) *dgraphStruct.DgraphPost {
	if p.ReplyToPost == nil || p.ReplyToPost.Uuid == "" || helpers.IsSoftDeleted(p.ReplyToPost.DeletedAt) {
		return nil
	}
	return p.ReplyToPost
}

func authorOf(p *dgraphStruct.DgraphPost) *dgraphStruct.DgraphUser {
	if p.PostBy == nil {
		return &dgraphStruct.DgraphUser{}
	}
	return p.PostBy
}

func channelOf(p *dgraphStruct.DgraphPost) *dgraphStruct.DgraphChannel {
	if p.Channel == nil {
		return &dgraphStruct.DgraphChannel{}
	}
	return p.Channel
}

// realtimePayload is the live message open apps get, as today's CreatePost
// sends it. Pure.
func realtimePayload(p *dgraphStruct.DgraphPost) *mqttStruct.MqttPost {
	a := authorOf(p)
	return &mqttStruct.MqttPost{
		Type:             mqttStruct.TYPE_CREATE,
		PostAttachments:  p.MediaObj,
		PostHtmlText:     p.Text,
		PostCreatedAt:    p.CreatedAt,
		PostByUserUuid:   a.Uuid,
		PostByProfileKey: a.ProfileKey,
		PostByUserName:   a.DisplayName(),
		PostByIsBot:      a.IsBot,
		PostChannelUuid:  channelOf(p).Uuid,
		PostUuid:         p.Uuid,
		PostReplyTo:      replyOf(p),
		PostKind:         p.Kind,
		PostSystem:       p.System,
	}
}

func realtimeSink(ctx context.Context, ev outboxBusiness.Event) error {
	p, err := livePost(ctx, ev)
	if err != nil || p == nil {
		return err
	}
	publishPost(realtimePayload(p), channelOf(p).Uuid)
	return nil
}

// searchEntry is the post's search entry, as today's CreatePost writes it. Pure.
func searchEntry(p *dgraphStruct.DgraphPost) *openSearchStruct.OpenSearchPost {
	a, ch := authorOf(p), channelOf(p)
	var at int64
	if p.CreatedAt != nil {
		at = p.CreatedAt.Unix()
	}
	return &openSearchStruct.OpenSearchPost{
		Uuid:               p.Uuid,
		PostBody:           helpers.HTMLToPlainText(p.Text),
		PostChannelUuid:    ch.Uuid,
		PostCreatedAt:      at,
		PostByUserUuid:     a.Uuid,
		PostByProfile:      a.ProfileKey,
		PostByUserFullName: a.DisplayName(),
		PostChannelName:    ch.Name,
	}
}

func searchSink(ctx context.Context, ev outboxBusiness.Event) error {
	p, err := livePost(ctx, ev)
	if err != nil || p == nil {
		return err
	}
	return indexPost(ctx, searchEntry(p), p.MediaObj)
}

func notifySink(ctx context.Context, ev outboxBusiness.Event) error {
	p, err := livePost(ctx, ev)
	if err != nil || p == nil {
		return err
	}
	a, ch := authorOf(p), channelOf(p)
	var mentions []*dgraphStruct.DgraphUser
	if p.Mentions != nil {
		mentions = p.Mentions.Mentions
	}
	replyTo := ""
	if r := replyOf(p); r != nil && r.PostBy != nil {
		replyTo = r.PostBy.Uuid
	}
	title := fmt.Sprintf("#%s - %s", ch.Name, a.DisplayName())
	notifyPost(ctx, title, helpers.HTMLToPlainText(p.Text), ch.Uuid, mentions, ch, p.Uuid, a, replyTo, func(how, recipient string) bool {
		return firstTime(ctx, ev, sinkNotify+":"+how, recipient)
	})
	return nil
}

func eventsSink(ctx context.Context, ev outboxBusiness.Event) error {
	p, err := livePost(ctx, ev)
	if err != nil || p == nil {
		return err
	}
	ch := channelOf(p)
	dispatchPost(context.WithoutCancel(ctx), "post.created", postCreatedEvent(p.Uuid, ch.Uuid, ch.Name, p.Text, authorOf(p)))
	return nil
}

func init() {
	for _, s := range []outboxBusiness.Sink{
		{Topic: MessageTopic, Name: sinkRealtime, Order: 10, Run: realtimeSink},
		{Topic: MessageTopic, Name: sinkSearch, Order: 20, Run: searchSink},
		{Topic: MessageTopic, Name: sinkNotify, Order: 40, Run: notifySink},
		{Topic: MessageTopic, Name: sinkEvents, Order: 50, Run: eventsSink},
	} {
		outboxBusiness.Sinks.Register(s)
	}
}
