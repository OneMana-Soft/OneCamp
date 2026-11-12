package business

// Where a call keeps what it makes (meet-02), as this package can tell it: its
// channel, its DM or group, or the calendar event whose call it is. Call
// State's HomeOf is the one answer; this package can't import it, so it reads
// it through callOwnRoom and callEvent (OnCallOwnRoom, OnCallEvent). An
// unrecorded call's transcript and every recording are filed by the same
// switch, here.

import (
	"context"

	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// callHome is where the call in a room keeps what it makes: one of a
// channel's uuid, a DM's or group's grouping id, or an event's uuid.
type callHome struct {
	channel string
	chat    string
	event   uuid.UUID
}

// homeOfCall answers where the call in a room keeps what it makes: a breakout
// room's call's home (callOwnRoom), a conversation's call's conversation, or
// the event whose call a meeting is (callEvent). ErrTranscriptNotKept for a
// call that keeps nothing: an instant meeting, a personal room.
func homeOfCall(ctx context.Context, room string) (callHome, error) {
	// A breakout room's own name ("<channel>~b1") is no channel's uuid, and
	// taken for one it once made a Channel node of that name to file the
	// transcript under, which nothing showed.
	channelUUID, chatGroupID := helpers.ClassifyRoom(callOwnRoom(room))
	if channelUUID != "" || chatGroupID != "" {
		return callHome{channel: channelUUID, chat: chatGroupID}, nil
	}
	id, err := callEvent(ctx, room)
	if err != nil {
		return callHome{}, err
	}
	event, err := uuid.Parse(id)
	if err != nil || event == uuid.Nil {
		return callHome{}, ErrTranscriptNotKept
	}
	return callHome{event: event}, nil
}

// fileUnderHome files a recording node (its uid) under its call's home, where
// the home's readers find it. Idempotent.
func fileUnderHome(ctx context.Context, uid string, home callHome) error {
	var err error
	switch {
	case home.event != uuid.Nil:
		err = recordingDomain.LinkCallSessionToEvent(ctx, uid, home.event)
	case home.channel != "":
		_, err = channelDomain.CreateOrUpdateDgraphChannel(ctx, &dgraphStruct.DgraphChannel{
			Uid:  "uid(ch)",
			Uuid: home.channel,
			Recordings: []*dgraphStruct.DgraphRecording{{
				Uid:     uid,
				Channel: &dgraphStruct.DgraphChannel{Uid: "uid(ch)"},
			}},
		})
	case home.chat != "":
		_, _, err = chatDomain.CreateDM(ctx, &dgraphStruct.DgraphDm{
			Uid: "uid(dm)",
			Recordings: []*dgraphStruct.DgraphRecording{{
				Uid: uid,
				Dm:  &dgraphStruct.DgraphDm{Uid: "uid(dm)"},
			}},
		}, home.chat)
	default:
		err = ErrTranscriptNotKept
	}
	return err
}

// FileUnderGroup files a recording node (its uid) under the DM or group whose
// grouping id is group, where that conversation's own calls file theirs
// (fileUnderHome). Idempotent. ErrTranscriptNotKept for no group.
func FileUnderGroup(ctx context.Context, uid, group string) error {
	return fileUnderHome(ctx, uid, callHome{chat: group})
}
