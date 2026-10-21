package business

// A channel's history says what happened to it (chat B28, SD-34 to 36): who
// joined, left, was added or removed, and who renamed it, set its topic,
// changed its privacy, archived or restored it. Each change is a Resource
// Event its controller emits; this sink writes it into the channel as a system
// line (systemLine.go), which notifies nobody and is never counted unread.
//
// Joins and leaves show in private channels and channels of up to 20 people;
// a larger public channel shows them only where the workspace turns on
// chat.lines.joins_large. Every other change always shows. A line is the
// event's: written at its time, as its actor. The bus delivers at most once,
// so a server stopping between a change and its line loses the line, never
// the change.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// The channel lines' kinds, as the web app draws them. Never renamed once shipped.
const (
	LineChannelJoined   = "channel.joined"   // Actor joined
	LineChannelLeft     = "channel.left"     // Actor left
	LineChannelAdded    = "channel.added"    // Actor added Targets
	LineChannelRemoved  = "channel.removed"  // Actor removed Targets
	LineChannelRenamed  = "channel.renamed"  // Actor renamed it Name, From the old name
	LineChannelTopic    = "channel.topic"    // Actor set the topic to Name ("" removed it)
	LineChannelPrivate  = "channel.private"  // Actor made it private
	LineChannelPublic   = "channel.public"   // Actor made it public
	LineChannelArchived = "channel.archived" // Actor archived it
	LineChannelRestored = "channel.restored" // Actor restored it
)

// joinsShownUpTo is the largest public channel whose joins and leaves show
// without the workspace's say.
const joinsShownUpTo = 20

// channelFacts is what a line needs of its channel.
type channelFacts struct {
	UID     string
	Members int
	Private bool
}

// channelLine is the line event says, its words, and whether it shows, with
// people (by uuid) to name and the channel's facts; joinsLarge is the
// workspace's chat.lines.joins_large. Pure.
func channelLine(e resourceEventBusiness.Event, people map[string]*dgraphStruct.DgraphUser, ch channelFacts, joinsLarge bool) (Line, string, bool) {
	actor := e.Actor.UserUUID
	who := names(pick(people, actor))
	l := Line{Actor: actor}
	membership := false
	var text string
	self := len(e.Subjects) == 1 && strings.EqualFold(e.Subjects[0], actor)
	switch e.Kind {
	case resourceEventBusiness.MemberAdded:
		membership = true
		if self {
			l.Kind, text = LineChannelJoined, who+" joined."
		} else {
			l.Kind, l.Targets = LineChannelAdded, e.Subjects
			text = who + " added " + names(pick(people, e.Subjects...)) + "."
		}
	case resourceEventBusiness.MemberRemoved:
		membership = true
		if self {
			l.Kind, text = LineChannelLeft, who+" left."
		} else {
			l.Kind, l.Targets = LineChannelRemoved, e.Subjects
			text = who + " removed " + names(pick(people, e.Subjects...)) + "."
		}
	case resourceEventBusiness.ChannelRenamed:
		l.Kind, l.Name, l.From = LineChannelRenamed, e.Data["name"], e.Data["was"]
		text = who + " renamed the channel to #" + l.Name
		if l.From != "" {
			text += " (was #" + l.From + ")"
		}
		text += "."
	case resourceEventBusiness.ChannelTopicChanged:
		l.Kind, l.Name = LineChannelTopic, strings.TrimSpace(e.Data["topic"])
		if l.Name == "" {
			text = who + " removed the topic."
		} else {
			text = who + " set the topic: " + l.Name
		}
	case resourceEventBusiness.ChannelPrivacyChanged:
		if e.Data["private"] == "true" {
			l.Kind, text = LineChannelPrivate, who+" made this channel private. Only its members can find it and read it now."
		} else {
			l.Kind, text = LineChannelPublic, who+" made this channel public. Everyone in the workspace can now find it and read its history and files."
		}
	case resourceEventBusiness.ChannelArchived:
		l.Kind, text = LineChannelArchived, who+" archived the channel."
	case resourceEventBusiness.ChannelRestored:
		l.Kind, text = LineChannelRestored, who+" restored the channel."
	default:
		return Line{}, "", false
	}
	if membership && !ch.Private && ch.Members > joinsShownUpTo && !joinsLarge {
		return l, text, false
	}
	return l, text, true
}

// pick is the people of ids in order, each known one once. Pure.
func pick(people map[string]*dgraphStruct.DgraphUser, ids ...string) []*dgraphStruct.DgraphUser {
	var out []*dgraphStruct.DgraphUser
	for _, id := range ids {
		if p := people[id]; p != nil {
			out = append(out, p)
		}
	}
	return out
}

// Seams for tests: the channel's facts, the people to name, the workspace's
// switch, and the writer.
var (
	readChannelFacts = func(ctx context.Context, channelUUID string) (channelFacts, error) {
		return channelFactsOf(ctx, channelUUID)
	}
	readLinePeople = func(ctx context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		return linePeople(ctx, ids)
	}
	joinsInLarge = func(ctx context.Context) bool {
		return policyBusiness.Policies.Bool(ctx, policyBusiness.ChatLinesJoinsInLarge)
	}
	writeLine = writeChannelLineAt
)

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "chat.lines", Wants: wantsChannelLine, Run: writeChannelEventLine})
}

// wantsChannelLine is whether e is a channel change a line says.
func wantsChannelLine(e resourceEventBusiness.Event) bool {
	if e.ResourceKind != KindChannel || e.Actor.UserUUID == "" {
		return false
	}
	switch e.Kind {
	case resourceEventBusiness.MemberAdded, resourceEventBusiness.MemberRemoved, resourceEventBusiness.ChannelRenamed,
		resourceEventBusiness.ChannelTopicChanged, resourceEventBusiness.ChannelPrivacyChanged,
		resourceEventBusiness.ChannelArchived, resourceEventBusiness.ChannelRestored:
		return true
	}
	return false
}

// writeChannelEventLine is the chat.lines sink: e's line, its failures logged.
func writeChannelEventLine(ctx context.Context, e resourceEventBusiness.Event) {
	if _, _, err := WriteChannelEventLine(ctx, e); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Conversation chat.lines: %s in %s: %+v", e.Kind, e.ResourceUUID, err)
	}
}

// WriteChannelEventLine writes the line a channel's change e says into the
// channel, at e's time and as its actor, and answers the line's uuid; shown is
// false for a change no line says (an unknown kind, an actor who isn't there,
// a join in a large public channel). The bus's sink and the Slack import (a
// Slack export's join, topic and rename events) both write through it.
func WriteChannelEventLine(ctx context.Context, e resourceEventBusiness.Event) (line string, shown bool, err error) {
	ch, err := readChannelFacts(ctx, e.ResourceUUID)
	if err != nil {
		return "", false, fmt.Errorf("reading channel: %w", err)
	}
	if ch.UID == "" {
		return "", false, nil
	}
	ids := append([]string{e.Actor.UserUUID}, e.Subjects...)
	found, err := readLinePeople(ctx, ids)
	if err != nil {
		return "", false, fmt.Errorf("reading people: %w", err)
	}
	people := map[string]*dgraphStruct.DgraphUser{}
	for _, p := range found {
		if p != nil {
			people[p.Uuid] = p
		}
	}
	actor := people[e.Actor.UserUUID]
	if actor == nil {
		return "", false, nil
	}
	l, text, show := channelLine(e, people, ch, joinsInLarge(ctx))
	if !show {
		return "", false, nil
	}
	id, err := writeLine(ctx, e.ResourceUUID, ch.UID, actor, l, text, e.At)
	if err != nil {
		return "", false, fmt.Errorf("writing %s: %w", l.Kind, err)
	}
	return id, true, nil
}

// channelFactsOf is a channel's node, size and privacy; no UID for a channel
// that isn't there.
func channelFactsOf(ctx context.Context, channelUUID string) (channelFacts, error) {
	id, err := uuid.Parse(channelUUID)
	if err != nil {
		return channelFacts{}, nil
	}
	ch, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(ctx, id, "")
	if errors.Is(err, channelBusiness.ErrNotFound) || (err == nil && ch == nil) {
		return channelFacts{}, nil
	}
	if err != nil {
		return channelFacts{}, err
	}
	return channelFacts{UID: ch.Uid, Members: int(ch.MemberCount), Private: ch.IsPrivate != nil && *ch.IsPrivate}, nil
}

// linePeople is the live people of ids, to name.
func linePeople(ctx context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
	return userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, ids)
}
