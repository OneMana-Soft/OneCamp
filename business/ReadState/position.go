package business

// Where someone is in a conversation and Mark unread (chat A3, the core B27
// stands on), and the one live event every move tells: read.moved on the
// mover's own activity topic, so every tab and device they have open moves its
// line and its counts at once.

import (
	"context"
	"errors"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	readStateModels "github.com/akashc777/OneCamp/models/postgres/ReadState"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Seams for tests: the marker store and the publisher.
var (
	positionOf     = readStateModels.PositionOf
	markUnreadFrom = readStateModels.MarkUnreadFrom
	publish        = mqttBusiness.PublishMessageToUser
)

// markerOf is ref's marker table and key, once who may read ref.
func markerOf(ctx context.Context, who *userModels.UserInfo, ref conversationBusiness.Ref) (readStateModels.Table, string, error) {
	if who == nil {
		return readStateModels.Table{}, "", conversationBusiness.ErrNotFound
	}
	var t readStateModels.Table
	switch ref.Kind {
	case conversationBusiness.KindChannel:
		if _, err := uuid.Parse(ref.ID); err != nil {
			return t, "", conversationBusiness.ErrNotFound
		}
		t = readStateModels.Channel
	case conversationBusiness.KindDM, conversationBusiness.KindGroup:
		if ref.ID == "" {
			return t, "", conversationBusiness.ErrNotFound
		}
		t = readStateModels.Chat
	default:
		return t, "", conversationBusiness.ErrNotFound
	}
	if _, err := conversationBusiness.Resolve(ctx, who, ref); err != nil {
		return t, "", err
	}
	return t, ref.ID, nil
}

func (markers) Position(ctx context.Context, who *userModels.UserInfo, ref conversationBusiness.Ref) (Position, error) {
	t, key, err := markerOf(ctx, who, ref)
	if err != nil {
		return Position{}, err
	}
	p, err := positionOf(ctx, t, who.UserPostgresInfo.Id, key)
	if err != nil {
		return Position{}, err
	}
	out := Position{Conversation: ref, Unread: p.Unread, FirstUnread: p.FirstUnread}
	if !p.Since.IsZero() {
		since := p.Since
		out.Since = &since
	}
	return out, nil
}

func (m markers) MarkUnread(ctx context.Context, who *userModels.UserInfo, ref conversationBusiness.Ref, message string) error {
	t, key, err := markerOf(ctx, who, ref)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(message)
	if err != nil {
		return conversationBusiness.ErrNotFound
	}
	if err := markUnreadFrom(ctx, t, who.UserPostgresInfo.Id, key, id); err != nil {
		if errors.Is(err, readStateModels.ErrNoMessage) {
			return conversationBusiness.ErrNotFound
		}
		return err
	}
	Moved(ctx, who, ref)
	return nil
}

// MovedEvent is the read.moved event's body: where the mover now is in one
// conversation.
type MovedEvent struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Unread      int    `json:"unread"`
	FirstUnread string `json:"first_unread,omitempty"`
}

// Moved tells who's own activity topic where they now are in ref: called
// after every move of their marker (a read, a Mark unread), so their other
// tabs and devices follow. A position that can't be read tells nothing; the
// next refetch puts it right.
func Moved(ctx context.Context, who *userModels.UserInfo, ref conversationBusiness.Ref) {
	if who == nil || who.UserDgraphInfo.Uuid == "" {
		return
	}
	p, err := States.Position(ctx, who, ref)
	if err != nil {
		if !errors.Is(err, conversationBusiness.ErrNotFound) {
			helpers.LogErrorWithContext(ctx, "business/ReadState Moved %s %s: %+v", ref.Kind, ref.ID, err)
		}
		return
	}
	publish(who.UserDgraphInfo.Uuid, mqttStruct.MESSAGE_READ_MOVED, MovedEvent{Kind: ref.Kind, ID: ref.ID, Unread: p.Unread, FirstUnread: p.FirstUnread})
}
