package business

// The call as its conversation's people see it from outside (MQTT call_state):
// whether it is on, who is in it, whether it is locked, recorded or captioned.

import (
	"context"
	"slices"
	"time"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// statePeople is how many of the people in a call the payload names: enough
// for the faces a header or a banner shows, never a channel's whole list.
const statePeople = 12

// publishes holds a room's publishes to one a second: a channel of fifty
// joining at the top of the hour is two messages to each member, not a
// hundred.
var publishes = helpers.NewThrottle("calls.state.publish", time.Second)

func init() {
	helpers.RegisterBackground("call state publishes", publishes.Settle)
	// A recording or a transcription switch changes what the call says about
	// itself; business/LiveKit, which can't import this package, says so here.
	liveKitBusiness.OnCallChange(Publish)
}

// Publish tells a room's conversation how its call is now, within a second.
// A breakout room's change is its parent call's: the conversation hears of
// the call, whichever of its rooms its people are in.
func Publish(ctx context.Context, room string) {
	if parent, _, ok := ParentOfBreakout(room); ok {
		room = parent
	}
	publishes.Do(room, func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		state, err := stateOf(ctx, room)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CallState could not read the call in %s to publish it: %+v", room, err)
			return
		}
		if state != nil {
			mqttBusiness.PublishCallState(state)
		}
	})
}

// stateOf is the payload for a room's latest call, or nil when it has none.
func stateOf(ctx context.Context, room string) (*mqttStruct.MqttCallState, error) {
	row, err := model.LatestSessionOf(ctx, room)
	if err != nil || row == nil {
		return nil, err
	}
	s, err := view(ctx, row)
	if err != nil {
		return nil, err
	}
	if s.Live && s.GoesOnInRooms(time.Now()) {
		if err := countInRooms(ctx, row.ID, s); err != nil {
			return nil, err
		}
	}
	var switches liveKitBusiness.CallView
	if s.Live {
		if switches, err = liveKitBusiness.ReadCallView(ctx, room, false); err != nil {
			return nil, err
		}
	}
	return statePayload(s, switches), nil
}

// countInRooms counts as in a call everyone in one of its open breakout
// rooms: from outside, a call is everyone in it, whichever room they are in.
func countInRooms(ctx context.Context, parent uuid.UUID, s *Session) error {
	there, err := model.InRooms(ctx, parent)
	if err != nil {
		return err
	}
	for i := range s.People {
		if _, ok := there[s.People[i].Identity]; ok {
			s.People[i].In = true
		}
	}
	return nil
}

// statePayload is a call as its conversation hears of it. Pure.
func statePayload(s *Session, switches liveKitBusiness.CallView) *mqttStruct.MqttCallState {
	in := s.InCall()
	out := &mqttStruct.MqttCallState{
		Session: s.ID, ResourceKind: s.Resource.Kind, ResourceID: s.Resource.ID, Live: s.Live,
		StartedAt: s.StartedAt, EndedAt: s.EndedAt, Count: len(in), Locked: s.Locked != nil,
		People: make([]mqttStruct.MqttCallPerson, 0, min(len(in), statePeople)),
		Post:   s.PostUUID, ChatHeld: s.Live && slices.Contains(s.Held, model.HoldChat),
	}
	for _, p := range in {
		if len(out.People) == statePeople {
			break
		}
		out.People = append(out.People, mqttStruct.MqttCallPerson{Identity: p.Identity, UserUUID: p.UserUUID, Name: p.Name, Role: p.Role, Guest: p.Guest})
	}
	// A meeting has no conversation to hear of it: every member in its call
	// does, so whoever runs it learns that someone knocked.
	if s.Resource.Kind == KindMeeting {
		for _, p := range in {
			if !p.Guest && p.UserUUID != "" {
				out.Members = append(out.Members, p.UserUUID)
			}
		}
	}
	if s.Live {
		out.Recording = mqttStruct.MqttCallSwitch{On: switches.Recording.On, ByName: switches.Recording.By}
		if t := switches.Transcription; t.On {
			out.Transcription = mqttStruct.MqttCallTranscript{On: true, ByName: t.By, Where: t.Where}
		}
	}
	return out
}
