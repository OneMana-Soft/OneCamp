package business

import (
	"context"
	"errors"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// Where a call's recording, transcript, notes and recap land (meet-02): a
// conversation's call in its conversation, with no read; an event's call on
// its event; a breakout room where its call's do. Any other meeting has no
// event to land on, and a name no call uses has no home at all.
func TestACallsArtefactsLandWhereItsHomeIs(t *testing.T) {
	channel := "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33"
	dm := "4f0c2a1e-3b7d-4c55-9a51-0d8b1f6a2c33 5a1d3b2f-4c8e-4d66-8b62-1e9c2a7b3d44"
	group := "0123456789abcdef0123456789abcdef"
	eventMeeting := "6f1c0e2a-1111-4222-8333-944455556666"
	instant := "7a2d1f3b-2222-4333-8444-a55566667777"
	gone := "8b3e2a4c-3333-4444-8555-b66677778888"
	event := uuid.MustParse("9c4f3b5d-4444-4555-8666-c77788889999")

	defer func(was func(context.Context, string) (*model.Meeting, error)) { meetingOf = was }(meetingOf)
	var read []string
	meetingOf = func(_ context.Context, id string) (*model.Meeting, error) {
		read = append(read, id)
		switch id {
		case eventMeeting:
			return &model.Meeting{ID: id, Kind: model.KindEvent, Event: event}, nil
		case instant:
			return &model.Meeting{ID: id, Kind: "instant"}, nil
		}
		return nil, nil
	}

	for room, want := range map[string]Home{
		channel:                               {Ref: Ref{Kind: "channel", ID: channel}},
		dm:                                    {Ref: Ref{Kind: "dm", ID: dm}},
		group:                                 {Ref: Ref{Kind: "group", ID: group}},
		BreakoutRoom(channel, 2):              {Ref: Ref{Kind: "channel", ID: channel}},
		"meet-" + eventMeeting:                {Ref: Ref{Kind: KindMeeting, ID: eventMeeting}, Event: event},
		BreakoutRoom("meet-"+eventMeeting, 1): {Ref: Ref{Kind: KindMeeting, ID: eventMeeting}, Event: event},
		"meet-" + instant:                     {Ref: Ref{Kind: KindMeeting, ID: instant}},
		"meet-" + gone:                        {Ref: Ref{Kind: KindMeeting, ID: gone}},
	} {
		got, err := HomeOf(context.Background(), room)
		if err != nil || got != want {
			t.Errorf("%q's home is %+v, %v; want %+v", room, got, err, want)
		}
	}
	if len(read) != 4 {
		t.Errorf("read %d meetings (%v), want one for each of the 4 meeting rooms and none for a conversation's", len(read), read)
	}

	for _, room := range []string{"", "general", "meet-", "not a uuid-ish-name"} {
		if got, err := HomeOf(context.Background(), room); !errors.Is(err, ErrNotACallRoom) {
			t.Errorf("%q's home is %+v, %v; want ErrNotACallRoom", room, got, err)
		}
	}

	down := errors.New("postgres is down")
	meetingOf = func(context.Context, string) (*model.Meeting, error) { return nil, down }
	if got, err := HomeOf(context.Background(), "meet-"+eventMeeting); !errors.Is(err, down) {
		t.Errorf("a meeting that can't be read has home %+v, %v; want its error", got, err)
	}
}
