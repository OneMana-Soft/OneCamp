package business

// What an outgoing webhook is sent of an event.
//
// An outgoing webhook is set up for the whole workspace, a channel or a
// project (the admin's "Events from"). An event is offered to the workspace's
// webhooks and to those set up for its own channel or project
// (domain/Webhook), and each is sent (deliveryTo):
//   - a channel's or a project's own webhook: the event whole, a private
//     channel's too, since whoever set it up for that place chose to send it;
//   - a workspace webhook: a public channel's or a project's event whole, a
//     private channel's as its ids only (who, which channel, which message,
//     never what was said); an event of neither whole only when its kind is
//     on wholeAtOrg, and as ids otherwise. A table row's event named a
//     private doc's table, and any kind added later would have gone whole
//     too, so a workspace event is private until someone decides otherwise;
//   - no webhook, ever: a DM, a group chat or a reply in one, or an agent's
//     message outside a channel. These went to every workspace webhook with
//     their text: an admin's webhook for chat.created was sent every DM
//     between any two people, and nobody in them was told (BE-BUG-13).
//
// And the events of a channel or a task used to reach no webhook at all: they
// were offered only to webhooks set up for that channel or project, which
// nothing could set up (SA-BUG-10).

import (
	"context"
	"errors"
	"strings"

	webhookDomain "github.com/akashc777/OneCamp/domain/Webhook"
	"github.com/google/uuid"
)

// What an outgoing webhook can be set up for.
const (
	ScopeOrg     = "org"
	ScopeChannel = "channel"
	ScopeProject = "project"
)

// eventPlace is where an event happened, in the terms webhooks are set up in.
type eventPlace struct {
	kind string     // ScopeChannel, ScopeProject or ScopeOrg
	id   *uuid.UUID // the channel or project; nil when its id doesn't read
	// conversation is a DM's or a group chat's event: no webhook is sent it.
	conversation bool
	// whole is a workspace event (kind ScopeOrg) whose kind may go whole to
	// a workspace webhook (wholeAtOrg).
	whole bool
}

// wholeAtOrg is the workspace events a workspace webhook is sent whole: their
// data says nothing private. Every other event of no channel and no project
// goes as its ids. A kind ending ".*" covers every kind under it.
//   - user.joined, user.left: who, and the channel they're in, if any;
//   - github.*: what the repository's own webhook said;
//   - doc.*: built for outside (business/Follow webhooks.go): ids, and a
//     title only when the doc isn't private.
var wholeAtOrg = []string{"user.joined", "user.left", "github.*", "doc.*"}

func sentWholeAtOrg(eventType string) bool {
	for _, k := range wholeAtOrg {
		if k == eventType || (strings.HasSuffix(k, ".*") && strings.HasPrefix(eventType, strings.TrimSuffix(k, "*"))) {
			return true
		}
	}
	return false
}

func placeOf(eventType string, data map[string]interface{}) eventPlace {
	if privateConversationEvent(eventType, data) {
		return eventPlace{conversation: true}
	}
	for _, at := range []struct{ key, kind string }{{"channel_id", ScopeChannel}, {"project_id", ScopeProject}} {
		raw, _ := data[at.key].(string)
		if strings.TrimSpace(raw) == "" {
			continue
		}
		p := eventPlace{kind: at.kind}
		if id, err := uuid.Parse(raw); err == nil {
			p.id = &id
		}
		return p
	}
	return eventPlace{kind: ScopeOrg, whole: sentWholeAtOrg(eventType)}
}

// privateConversationEvent says whether an event is about a DM or a group
// chat, whose words are only for the people in it.
func privateConversationEvent(eventType string, data map[string]interface{}) bool {
	if strings.HasPrefix(eventType, "chat.") {
		return true
	}
	// An agent's own message (business/AIAgent EventTypeAgentMessage) names
	// its channel when it is in one; anywhere else it is in a conversation.
	if eventType == "agent.message" {
		ch, _ := data["channel_id"].(string)
		return strings.TrimSpace(ch) == ""
	}
	return false
}

type delivery int

const (
	sendNothing delivery = iota
	sendIDs
	sendAll
)

// deliveryTo is what a webhook set up for scope (and entity) is sent of an
// event at place; public says whether a channel event's channel is a public
// one. Pure.
func deliveryTo(scope string, entity *uuid.UUID, place eventPlace, public bool) delivery {
	if place.conversation {
		return sendNothing
	}
	switch scope {
	case "", ScopeOrg:
		switch {
		case place.kind == ScopeChannel && !public:
			return sendIDs
		case place.kind == ScopeOrg && !place.whole:
			return sendIDs
		}
		return sendAll
	case ScopeChannel, ScopeProject:
		if place.kind == scope && place.id != nil && entity != nil && *place.id == *entity {
			return sendAll
		}
	}
	return sendNothing
}

// idsOnly is data cut to the keys that name something.
func idsOnly(data map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(data))
	for k, v := range data {
		if namesSomething(k) {
			out[k] = v
		}
	}
	return out
}

// namesSomething is a key that names a thing (an id, a list of ids, where it
// came from) rather than saying anything. Kept to these on purpose: a field
// added to an event later stays private until someone decides otherwise.
func namesSomething(key string) bool {
	switch key {
	case "source", "surface_kind":
		return true
	}
	return strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_ids") || strings.HasSuffix(key, "_uuid")
}

// ScopeError is a scope a webhook can't be set up for; its message is
// the one the admin sees.
type ScopeError struct{ msg string }

func (e ScopeError) Error() string { return e.msg }

// webhookScope checks what an outgoing webhook is set up for and says it as
// stored. An incoming webhook isn't sent events, so it is the workspace's.
func webhookScope(ctx context.Context, webhookType, scope string, entity *uuid.UUID) (string, *uuid.UUID, error) {
	if webhookType != "outgoing" {
		return ScopeOrg, nil, nil
	}
	switch scope {
	case "", ScopeOrg:
		return ScopeOrg, nil, nil
	case ScopeChannel, ScopeProject:
	default:
		return "", nil, ScopeError{"Send it events from the whole workspace, one channel or one project."}
	}
	if entity == nil || *entity == uuid.Nil {
		return "", nil, ScopeError{"Choose the " + scope + " to send events from."}
	}
	open := webhookDomain.ChannelIsOpen
	if scope == ScopeProject {
		open = webhookDomain.ProjectIsOpen
	}
	ok, err := open(ctx, *entity)
	if err != nil {
		return "", nil, errors.New("couldn't check the " + scope + "; try again")
	}
	if !ok {
		return "", nil, ScopeError{"That " + scope + " doesn't exist, or is archived."}
	}
	return scope, entity, nil
}
