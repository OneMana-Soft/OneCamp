package helpers

import "context"

// KindHuddle is a call's message's post_kind and chat_kind: the post a
// conversation's call writes when it starts (calls CALLS-3b, business/Huddle).
// Unlike a system line it is a message, with a thread, that can be pinned;
// nobody edits it, it can't be shared, and it tells nobody (the ring is chat
// P10's). business/Conversation names it beside KindSystem.
const KindHuddle = "huddle"

type messageKindKey struct{}

// WithMessageKind marks ctx as writing a message of kind (KindHuddle), so the
// durable write chooses that kind's outbox sinks.
func WithMessageKind(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, messageKindKey{}, kind)
}

// MessageKind is the kind of message ctx writes: "" for an ordinary one.
func MessageKind(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	k, _ := ctx.Value(messageKindKey{}).(string)
	return k
}
