package business

// A Slack channel's own history, brought as OneCamp's quiet lines (chat B28):
// "Maya joined.", "Jonas left.", "Maya added Priya.", "Maya renamed the
// channel to #launch (was #q4-launch).", "Jonas set the topic: …", archived
// and restored. Written through business/Conversation WriteChannelEventLine,
// the one writer the live changes use, at the Slack event's time and as its
// actor, so an imported channel reads as one made here: joins in a public
// channel of more than 20 people stay hidden as there, and nothing notifies
// or counts unread. Only when the operator keeps system messages (the plan's
// "Skip system messages" off); other system events import as before.

import (
	"context"
	"fmt"
	"strings"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// isChannelLine is whether a Slack subtype is a channel event a line says.
func isChannelLine(subtype string) bool {
	switch subtype {
	case "channel_join", "group_join", "channel_leave", "group_leave",
		"channel_topic", "group_topic", "channel_name", "group_name",
		"channel_archive", "group_archive", "channel_unarchive", "group_unarchive":
		return true
	}
	return false
}

// channelLineEvent is the change m says, with its Slack people: its actor,
// and the one it happened to ("" for a change to the channel itself); ok
// false for a subtype that isn't one. A Slack event happens to one person at
// most. Pure.
func channelLineEvent(m *SlackMessage) (kind resourceEventBusiness.Kind, actor, subject string, data map[string]string, ok bool) {
	switch m.Subtype {
	case "channel_join", "group_join":
		if m.Inviter != "" && m.Inviter != m.User {
			return resourceEventBusiness.MemberAdded, m.Inviter, m.User, nil, true
		}
		return resourceEventBusiness.MemberAdded, m.User, m.User, nil, true
	case "channel_leave", "group_leave":
		return resourceEventBusiness.MemberRemoved, m.User, m.User, nil, true
	case "channel_topic", "group_topic":
		return resourceEventBusiness.ChannelTopicChanged, m.User, "", map[string]string{"topic": strings.TrimSpace(m.Topic)}, true
	case "channel_name", "group_name":
		if strings.TrimSpace(m.Name) == "" {
			return "", "", "", nil, false
		}
		return resourceEventBusiness.ChannelRenamed, m.User, "", map[string]string{"name": strings.TrimSpace(m.Name), "was": strings.TrimSpace(m.OldName)}, true
	case "channel_archive", "group_archive":
		return resourceEventBusiness.ChannelArchived, m.User, "", nil, true
	case "channel_unarchive", "group_unarchive":
		return resourceEventBusiness.ChannelRestored, m.User, "", nil, true
	}
	return "", "", "", nil, false
}

// writeImportedLine is the line writer; a seam for tests.
var writeImportedLine = conversationBusiness.WriteChannelEventLine

// importChannelLine writes m's line into the channel and records it, so a
// retry or a re-export skips it as it skips a message. A change no line says
// (a join in a large public channel) is recorded as seen, with nothing written.
func importChannelLine(ctx context.Context, mc *messageContext, m *SlackMessage, importingUser *userModels.UserInfo) error {
	kind, actorID, subjectID, data, ok := channelLineEvent(m)
	if !ok {
		return mc.maps.placeholder(ctx, importModels.EntityMessage, m.Ts,
			uuid.New(), nil, mustMarshal(map[string]interface{}{"line": m.Subtype, "said": false}))
	}
	actor, err := mc.resolveUser(ctx, actorID, importingUser)
	if err != nil || actor == nil {
		return fmt.Errorf("resolve %s: %w", actorID, err)
	}
	var subjects []string
	if subjectID != "" {
		who, err := mc.resolveUser(ctx, subjectID, importingUser)
		if err != nil || who == nil {
			return fmt.Errorf("resolve %s: %w", subjectID, err)
		}
		subjects = []string{who.UserDgraphInfo.Uuid}
	}
	line, shown, err := writeImportedLine(ctx, resourceEventBusiness.Event{
		Kind: kind, ResourceKind: conversationBusiness.KindChannel, ResourceUUID: mc.channelUUID.String(),
		Actor: resourceEventBusiness.Actor{UserUUID: actor.UserDgraphInfo.Uuid}, At: slackTsToTime(m.Ts),
		Subjects: subjects, Data: data,
	})
	if err != nil {
		return err
	}
	meta := mustMarshal(map[string]interface{}{"channel_slack_id": mc.channelSlackId, "line": string(kind), "said": shown})
	if !shown {
		return mc.maps.placeholder(ctx, importModels.EntityMessage, m.Ts, uuid.New(), nil, meta)
	}
	id, err := uuid.Parse(line)
	if err != nil {
		return fmt.Errorf("parse the line's uuid: %w", err)
	}
	// Mapped for this import and for the workspace in one write, as a
	// message is (batch.go), so a retry and the next re-import skip it.
	return mc.maps.recordCreated(ctx, importModels.EntityMessage, m.Ts, id, nil, meta)
}
