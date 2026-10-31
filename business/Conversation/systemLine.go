package business

// System lines: a message the server writes into a conversation about the
// conversation itself ("Maya added Priya"), never a person's words. The web app
// draws it as a line; it is never notified, counted unread, searched or
// embedded (it has no row in posts or chats, the ledgers counts and retention
// read), and nobody edits or deletes it. Its words are kept too, for an app
// that can't draw a line and for the conversation list's preview.
//
// A9 writes the group lines below; chat B28 adds joins, leaves, renames and
// the rest on the same writer (PROGRAM.md §4.2 #36).

import (
	"context"
	"encoding/json"
	"html"
	"strings"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/google/uuid"
)

// KindSystem is a system line's post_kind and chat_kind.
const KindSystem = "system"

// KindHuddle is a call's post's post_kind and chat_kind (calls CALLS-3b,
// business/Huddle): a message with a thread, pinnable, never edited or
// shared, telling nobody (helpers.KindHuddle).
const KindHuddle = helpers.KindHuddle

// The kinds of line, as the web app draws them. Never renamed once shipped.
const (
	// In a group opened for a new set of people: Actor started it with
	// Targets, continuing From (the conversation they were in).
	LineGroupStarted = "group.started"
	// In the conversation people were added from: Actor added Targets, and
	// the conversation continues in To.
	LineGroupContinued = "group.continued"
	// Actor named the group Name ("" took its name away).
	LineGroupRenamed = "group.renamed"
	// Count earlier messages and replies from From were shared in.
	LineHistoryShared = "history.shared"
	// In a group: Actor made it the private channel To (named Name).
	LineGroupBecameChannel = "group.channel"
	// In a channel: Actor made it from the conversation From, with Targets.
	LineChannelFromGroup = "channel.from_group"
	// In any conversation: Actor pinned the message To (chat B25).
	LineMessagePinned = "message.pinned"
	// In a group: Actor left it.
	LineGroupLeft = "group.left"
)

// Line is a system line's facts: who did what, to whom, between which
// conversations. People are named by their uuids; the web app names them.
type Line struct {
	Kind    string   `json:"kind"`
	Actor   string   `json:"actor,omitempty"`
	Targets []string `json:"targets,omitempty"`
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
	Name    string   `json:"name,omitempty"`
	Count   int      `json:"count,omitempty"`
}

// lineHTML is a line's words as a message body. Pure.
func lineHTML(text string) string {
	return "<p>" + html.EscapeString(strings.TrimSpace(text)) + "</p>"
}

// names is people's names as a sentence lists them: "Priya", "Priya and
// Jonas", "Priya, Jonas and Sam". Pure.
func names(people []*dgraphStruct.DgraphUser) string {
	var ns []string
	for _, p := range people {
		if n := strings.TrimSpace(p.DisplayName()); n != "" {
			ns = append(ns, n)
		}
	}
	switch len(ns) {
	case 0:
		return "someone"
	case 1:
		return ns[0]
	}
	return strings.Join(ns[:len(ns)-1], ", ") + " and " + ns[len(ns)-1]
}

// Where lines are written and announced: seams for tests.
var (
	writeChatNode  = chatDomain.CreateDgraphChat
	writePostNode  = postDomain.CreateOrUpdateDgraphPost
	publishChat    = mqttBusiness.PublishChat
	publishPost    = mqttBusiness.PublishPost
	systemLineTime = time.Now
)

// writeGroupLine writes l into the DM or group groupingID, as actor, with
// text as its words, and tells the conversation's open apps. people, when
// given, are the group's people, written with the line: a group's first
// message makes it.
func writeGroupLine(ctx context.Context, groupingID string, actor *dgraphStruct.DgraphUser, l Line, text string, people []*dgraphStruct.DgraphUser) (string, error) {
	facts, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	now := systemLineTime()
	zero := time.Time{}
	body := lineHTML(text)
	dm := dgraphStruct.DgraphDm{
		Uid:        "uid(dm)",
		GroupingId: groupingID,
		DType:      []string{"Dm"},
		Chats: []*dgraphStruct.DgraphChat{{
			Uid:       "uid(ch)",
			Uuid:      id,
			DType:     []string{"Chat"},
			CreatedAt: &now,
			DeletedAt: &zero,
			From:      &dgraphStruct.DgraphUser{Uid: actor.Uid},
			DM:        &dgraphStruct.DgraphDm{Uid: "uid(dm)"},
			Body:      body,
			Kind:      KindSystem,
			System:    string(facts),
		}},
	}
	for _, p := range people {
		dm.Participants = append(dm.Participants, &dgraphStruct.DgraphUser{
			Uid: p.Uid,
			DMs: []*dgraphStruct.DgraphDm{{Uid: "uid(dm)"}},
		})
	}
	if _, _, err := writeChatNode(ctx, &dm, id); err != nil {
		return "", err
	}
	// An import writes history: nobody is watching it arrive, so nothing is
	// published (helpers.IsBulkImport), as the import's messages aren't.
	if helpers.IsBulkImport(ctx) {
		return id, nil
	}
	helpers.GoSafeNamed("conversation.line", func() {
		publishChat(&mqttStruct.MqttChat{
			Type: mqttStruct.TYPE_CREATE, ChatUuid: id, ChatHtmlText: body, ChatCreatedAt: &now,
			ChatByUserUuid: actor.Uuid, ChatByUserName: actor.DisplayName(), ChatByProfileKey: actor.ProfileKey,
			ChatGrpId: groupingID, ChatKind: KindSystem, ChatSystem: string(facts),
		}, groupingID)
	})
	return id, nil
}

// writeChannelLine is writeGroupLine for a channel (channelUID is its graph
// node).
func writeChannelLine(ctx context.Context, channelUUID, channelUID string, actor *dgraphStruct.DgraphUser, l Line, text string) (string, error) {
	return writeChannelLineAt(ctx, channelUUID, channelUID, actor, l, text, systemLineTime())
}

// writeChannelLineAt is writeChannelLine at the time of what it says (a zero
// time is now).
func writeChannelLineAt(ctx context.Context, channelUUID, channelUID string, actor *dgraphStruct.DgraphUser, l Line, text string, at time.Time) (string, error) {
	facts, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	now := at
	if now.IsZero() {
		now = systemLineTime()
	}
	zero := time.Time{}
	body := lineHTML(text)
	post := dgraphStruct.DgraphPost{
		Uid:    "uid(po)",
		Uuid:   id,
		DType:  []string{"Post"},
		Text:   body,
		PostBy: &dgraphStruct.DgraphUser{Uid: actor.Uid},
		Channel: &dgraphStruct.DgraphChannel{
			Uid:   channelUID,
			Posts: []*dgraphStruct.DgraphPost{{Uid: "uid(po)"}},
		},
		CreatedAt: &now,
		DeletedAt: &zero,
		Kind:      KindSystem,
		System:    string(facts),
	}
	if _, err := writePostNode(ctx, &post); err != nil {
		return "", err
	}
	// An import writes history: nobody is watching it arrive, so nothing is
	// published (helpers.IsBulkImport), as the import's messages aren't.
	if helpers.IsBulkImport(ctx) {
		return id, nil
	}
	helpers.GoSafeNamed("conversation.line", func() {
		publishPost(&mqttStruct.MqttPost{
			Type: mqttStruct.TYPE_CREATE, PostUuid: id, PostHtmlText: body, PostCreatedAt: &now,
			PostByUserUuid: actor.Uuid, PostByUserName: actor.DisplayName(), PostByProfileKey: actor.ProfileKey,
			PostChannelUuid: channelUUID, PostKind: KindSystem, PostSystem: string(facts),
		}, channelUUID)
	})
	return id, nil
}

// WriteLine writes l into the conversation ref as actor, with text as its
// words, for a package beside this one (chat B25's pins; B28's joins and
// renames come the same way). It answers the line's id; ErrNotFound when
// there is no such conversation.
func WriteLine(ctx context.Context, ref Ref, actor *dgraphStruct.DgraphUser, l Line, text string) (string, error) {
	switch ref.Kind {
	case KindChannel:
		ch, err := channelOf(ctx, ref.ID, actor.Uid)
		if err != nil {
			return "", err
		}
		if ch == nil {
			return "", ErrNotFound
		}
		return writeChannelLine(ctx, ref.ID, ch.Uid, actor, l, text)
	case KindDM, KindGroup:
		return writeGroupLine(ctx, ref.ID, actor, l, text, nil)
	}
	return "", ErrNotFound
}
