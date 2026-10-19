package business

// The AI edition's sink for a channel post (chat A2): its embedding, as
// today's CreatePost makes it (a reply with what it replies to, so a terse
// answer carries its meaning). This file is the AI edition's alone: without-ai
// doesn't hold it, so its sink isn't registered there and the outbox passes
// over the name (effectSinks, in durable.go, lists it).

import (
	"context"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func embedSink(ctx context.Context, ev outboxBusiness.Event) error {
	p, err := livePost(ctx, ev)
	if err != nil || p == nil {
		return err
	}
	a, ch := authorOf(p), channelOf(p)
	ai.EmbedPostContent(withReplyContext(replyOf(p), helpers.HTMLToPlainText(p.Text)), p.Uuid, ch.Uuid, ch.Name, a.Uuid, a.DisplayName())
	return nil
}

func init() {
	outboxBusiness.Sinks.Register(outboxBusiness.Sink{Topic: MessageTopic, Name: sinkEmbed, Order: 30, Run: embedSink})
}
