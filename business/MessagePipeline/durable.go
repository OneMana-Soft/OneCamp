package business

// The durable pipeline (chat A2): a surface switched on writes its messages
// with their events in one transaction and has their side effects run from the
// outbox (business/Outbox; business/Post durable.go and postSinks.go for
// channel posts). A surface not switched on writes as today's routes do.
//
// THE SWITCH. MESSAGE_PIPELINE names the surfaces that go the durable way,
// comma separated: "channel" today; "dm" and "group" follow, then replies,
// edits and reactions, then the other writers. Off by default in the first
// release, on for the demo once each surface's golden test matches today's,
// and on by default a release later. The system check says which are on.

import (
	"context"
	"os"
	"sort"
	"strings"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// SurfacesEnv is the switch's environment variable.
const SurfacesEnv = "MESSAGE_PIPELINE"

// The surfaces the switch knows.
const SurfaceChannel = "channel"

// surfacesOf is the surfaces a switch's value turns on. Pure.
func surfacesOf(v string) map[string]bool {
	on := map[string]bool{}
	for _, s := range strings.Split(v, ",") {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			on[s] = true
		}
	}
	return on
}

// Seams for tests: the switch, the durable channel write, a post's answer.
var (
	surfaceOn    = func(s string) bool { return surfacesOf(os.Getenv(SurfacesEnv))[s] }
	writeChannel = writeChannelDurably
	postWritten  = postBusiness.PostWritten
)

// durable is the pipeline: the durable write for each surface switched on,
// today's for the rest.
type durable struct{ today Pipeline }

func (d durable) Write(ctx context.Context, who *userModels.UserInfo, w Write) (Written, error) {
	if w.Op == OpSend && w.To.Kind == conversationBusiness.KindChannel && surfaceOn(SurfaceChannel) {
		return writeChannel(ctx, who, w)
	}
	return d.today.Write(ctx, who, w)
}

// writeChannelDurably is a channel post the durable way: today's rules and its
// once per client id, then CommitDurably.
func writeChannelDurably(ctx context.Context, who *userModels.UserInfo, w Write) (Written, error) {
	if who == nil {
		return Written{}, conversationBusiness.ErrNotFound
	}
	me := who.UserDgraphInfo.Uuid
	post, err := sendBusiness.PrepareChannelPost(ctx, who, channelInput(w))
	if err != nil {
		return Written{}, err
	}
	out, err := sendBusiness.Once(ctx, me, onceKey(w, me), w.Draft.ClientMsgID, func() (*postAdapter.OutputCreatePostForPost, error) {
		return post.CommitDurably(ctx, w.Draft.ClientMsgID)
	})
	if err != nil || out == nil {
		return Written{}, err
	}
	return Written{ID: out.Uuid, At: out.PostCreated}, nil
}

// resolveMessage is the message topic's answer for an event its writer never
// released: whether the message was written, by its kind.
func resolveMessage(ctx context.Context, ev outboxBusiness.Event) (bool, error) {
	switch ev.Entities["kind"] {
	case conversationBusiness.KindChannel:
		return postWritten(ctx, ev.Entities["message"])
	}
	// A kind with no answer of its own: its change was committed with the event.
	return true, nil
}

// switchNote is what the system check says of the switch. Pure.
func switchNote(v string) string {
	on := surfacesOf(v)
	if len(on) == 0 {
		return "Every message is written as before; " + SurfacesEnv + " switches surfaces to the outbox."
	}
	names := make([]string, 0, len(on))
	for s := range on {
		names = append(names, s)
	}
	sort.Strings(names)
	return "These surfaces write through the outbox: " + strings.Join(names, ", ") + "."
}

func init() {
	outboxBusiness.Sinks.Resolve(postBusiness.MessageTopic, resolveMessage)
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "message-pipeline",
		Kind: helpers.CheckKindBehaviour,
		Describe: "Says which kinds of message are written through the outbox (" + SurfacesEnv + "); " +
			"the outbox's own check says whether their side effects are keeping up.",
		Probe: func(context.Context) error { return helpers.SystemCheckNote(switchNote(os.Getenv(SurfacesEnv))) },
	})
}
