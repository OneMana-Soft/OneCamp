package business

// A DM's or group message's side effects as the outbox's sinks (chat A2): what
// today's CreateChat and CreateChatForGroup fire as goroutines
// (fanOutCreatedChat), run from the message's events instead, at least once
// each, after a crash, on any process (business/Outbox).
//
// Each sink reads the message as it is when it runs (the event carries its
// id, never its words): one deleted since is skipped and its event marked
// done, and one edited since sends its words now. The sinks say what today's
// fan-out says (golden_test.go holds them to it):
//
//   - access:   a new conversation's people are told it's theirs, so their
//     open apps subscribe (before the message reaches them: it runs first);
//   - realtime: the message to its conversation's open apps (MQTT);
//   - search:   its entry and its files', with the conversation's people,
//     written as an index, so a retry is the same entry;
//   - notify:   the Activity item, push and email notifyNewChat sends, each
//     person told once (outbox FirstTime);
//   - events:   chat.created to the listeners and webhooks (DispatchEvent).
//
// The AI edition's embedding is its own sink, in chatEmbedSink.go.

import (
	"context"
	"errors"
	"strings"
	"time"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	domain "github.com/akashc777/OneCamp/domain/Chat"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// MessageTopic is the outbox topic every message's events are on, a channel
// post's (business/Post) as a DM's or group message's; MessageCreated the
// kind of a new one.
const (
	MessageTopic   = "message"
	MessageCreated = "message.created"
)

// The sinks' names: stable, since the outbox remembers which had an event by
// them.
const (
	sinkChatAccess   = "chat.access"
	sinkChatRealtime = "chat.realtime"
	sinkChatSearch   = "chat.search"
	sinkChatEmbed    = "chat.embed"
	sinkChatNotify   = "chat.notify"
	sinkChatEvents   = "chat.events"
)

// The seams the sinks read and tell through, for tests (the fan-out's own are
// in chatBusiness.go).
var (
	readChat  = domain.ReadChatForSinks
	indexChat = domain.IndexChatWithAttachments
	firstTime = outboxBusiness.FirstTime
)

// chatRead keeps the message one event's sinks read, for the length of a
// drain's run of them: the four sinks of a message's other event, one read. A
// retry, later, reads it again.
var chatRead = helpers.NewTTLCache[*dgraphStruct.DgraphChat](30 * time.Second)

// chatKind is the conversation kind an event is about, "" for one that isn't
// a DM's or group message's. Pure.
func chatKind(ev outboxBusiness.Event) string {
	switch k := ev.Entities["kind"]; k {
	case mqttStruct.CONVERSATION_KIND_DM, mqttStruct.CONVERSATION_KIND_GROUP:
		return k
	}
	return ""
}

// liveChat is the message an event is about, as it is now, or nil when it's
// gone or deleted (or the event isn't a DM's or group message's): its side
// effects are skipped, and the event is done.
func liveChat(ctx context.Context, ev outboxBusiness.Event) (*dgraphStruct.DgraphChat, error) {
	if chatKind(ev) == "" {
		return nil, nil
	}
	if c, ok := chatRead.Get(ev.EventID); ok {
		return c, nil
	}
	c, err := readChat(ctx, ev.Entities["message"])
	if errors.Is(err, domain.ErrChatGone) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if helpers.IsSoftDeleted(c.DeletedAt) {
		c = nil
	}
	chatRead.Set(ev.EventID, c)
	return c, nil
}

// asCreated is a message as the graph holds it now, described as today's
// writers describe a new one to their fan-out. Pure.
func asCreated(kind string, c *dgraphStruct.DgraphChat) createdChat {
	out := createdChat{
		kind: kind, id: c.Uuid, html: c.Body, files: c.MediaObj,
		author: userOrEmpty(c.From), to: userOrEmpty(c.To), replyTo: chatReplyOf(c),
	}
	if c.CreatedAt != nil {
		out.at = *c.CreatedAt
	}
	if c.DM != nil {
		out.grp = c.DM.GroupingId
		if kind == mqttStruct.CONVERSATION_KIND_GROUP {
			out.people = c.DM.Participants
		}
	}
	if c.Mentions != nil {
		out.mentions = c.Mentions.Mentions
	}
	return out
}

func userOrEmpty(u *dgraphStruct.DgraphUser) *dgraphStruct.DgraphUser {
	if u == nil {
		return &dgraphStruct.DgraphUser{}
	}
	return u
}

// chatReplyOf is the message c quotes, when it is still there to quote. Pure.
func chatReplyOf(c *dgraphStruct.DgraphChat) *dgraphStruct.DgraphChat {
	if c.ReplyToChat == nil || c.ReplyToChat.Uuid == "" || helpers.IsSoftDeleted(c.ReplyToChat.DeletedAt) {
		return nil
	}
	return c.ReplyToChat
}

// openedOf is who an event says are told the conversation is theirs: the
// people of the conversation its message made, nil for an older one. Pure.
func openedOf(ev outboxBusiness.Event) []string {
	if ev.Entities["opened"] == "" {
		return nil
	}
	return strings.Split(ev.Entities["opened"], ",")
}

func chatAccessSink(_ context.Context, ev outboxBusiness.Event) error {
	opened := openedOf(ev)
	if chatKind(ev) == "" || len(opened) == 0 {
		return nil
	}
	// The conversation is there whatever became of its first message since:
	// the event is only released once that message was written.
	publishAccess(chatKind(ev), ev.Entities["conversation"], mqttStruct.CONVERSATION_ADDED, opened...)
	return nil
}

func chatRealtimeSink(ctx context.Context, ev outboxBusiness.Event) error {
	c, err := liveChat(ctx, ev)
	if err != nil || c == nil {
		return err
	}
	created := asCreated(chatKind(ev), c)
	publishChat(created.livePayload(), created.grp)
	return nil
}

func chatSearchSink(ctx context.Context, ev outboxBusiness.Event) error {
	c, err := liveChat(ctx, ev)
	if err != nil || c == nil {
		return err
	}
	created := asCreated(chatKind(ev), c)
	return indexChat(ctx, created.searchEntry(), created.files)
}

func chatNotifySink(ctx context.Context, ev outboxBusiness.Event) error {
	c, err := liveChat(ctx, ev)
	if err != nil || c == nil {
		return err
	}
	created := asCreated(chatKind(ev), c)
	a := created.author
	notifyChat(ctx, created.pushTitle(), helpers.HTMLToPlainText(created.html), a.Uuid, created.grp, created.mentions, created.id, a.DisplayName(), a.ProfileKey, replyParentChatAuthorUUID(created.replyTo), func(how, recipient string) bool {
		return firstTime(ctx, ev, sinkChatNotify+":"+how, recipient)
	})
	return nil
}

func chatEventsSink(ctx context.Context, ev outboxBusiness.Event) error {
	c, err := liveChat(ctx, ev)
	if err != nil || c == nil {
		return err
	}
	created := asCreated(chatKind(ev), c)
	dispatchChatEvent(context.WithoutCancel(ctx), "chat.created", created.createdEvent())
	return nil
}

func init() {
	for _, s := range []outboxBusiness.Sink{
		{Topic: MessageTopic, Name: sinkChatAccess, Order: 5, Run: chatAccessSink},
		{Topic: MessageTopic, Name: sinkChatRealtime, Order: 10, Run: chatRealtimeSink},
		{Topic: MessageTopic, Name: sinkChatSearch, Order: 20, Run: chatSearchSink},
		{Topic: MessageTopic, Name: sinkChatNotify, Order: 40, Run: chatNotifySink},
		{Topic: MessageTopic, Name: sinkChatEvents, Order: 50, Run: chatEventsSink},
	} {
		outboxBusiness.Sinks.Register(s)
	}
}
