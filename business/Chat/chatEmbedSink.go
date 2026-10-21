package business

// The AI edition's sink for a DM or group message (chat A2): its embedding, as
// today's writers make it (a reply with what it replies to, so a terse answer
// carries its meaning). This file is the AI edition's alone: without-ai
// doesn't hold it, so its sink isn't registered there and the outbox passes
// over the name (chatEffectSinks, in durable.go, lists it).

import (
	"context"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func chatEmbedSink(ctx context.Context, ev outboxBusiness.Event) error {
	c, err := liveChat(ctx, ev)
	if err != nil || c == nil {
		return err
	}
	created := asCreated(chatKind(ev), c)
	a := created.author
	ai.EmbedChatContent(withChatReplyContext(created.replyTo, helpers.HTMLToPlainText(created.html)), created.id, a.Uuid, a.DisplayName(), created.grp, a.Uuid, created.dmOther(), created.participantUUIDs())
	return nil
}

func init() {
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: MessageTopic, Name: sinkChatEmbed, Order: 30, Run: chatEmbedSink})
}
