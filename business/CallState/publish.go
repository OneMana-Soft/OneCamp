package business

// The call as its conversation's people see it from outside (MQTT call_state):
// whether it is on, who is in it, whether it is locked, recorded or captioned.

import (
	"context"
	"time"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
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
func Publish(ctx context.Context, room string) {
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
	var switches liveKitBusiness.CallView
	if s.Live {
		if switches, err = liveKitBusiness.ReadCallView(ctx, room, false); err != nil {
			return nil, err
		}
	}
	return statePayload(s, switches), nil
}

// statePayload is a call as its conversation hears of it. Pure.
func statePayload(s *Session, switches liveKitBusiness.CallView) *mqttStruct.MqttCallState {
	in := s.InCall()
	out := &mqttStruct.MqttCallState{
		Session: s.ID, ResourceKind: s.Resource.Kind, ResourceID: s.Resource.ID, Live: s.Live,
		StartedAt: s.StartedAt, EndedAt: s.EndedAt, Count: len(in), Locked: s.Locked != nil,
		People: make([]mqttStruct.MqttCallPerson, 0, min(len(in), statePeople)),
	}
	for _, p := range in {
		if len(out.People) == statePeople {
			break
		}
		out.People = append(out.People, mqttStruct.MqttCallPerson{Identity: p.Identity, UserUUID: p.UserUUID, Name: p.Name, Role: p.Role, Guest: p.Guest})
	}
	if s.Live {
		out.Recording = mqttStruct.MqttCallSwitch{On: switches.Recording.On, ByName: switches.Recording.By}
		if t := switches.Transcription; t.On {
			out.Transcription = mqttStruct.MqttCallTranscript{On: true, ByName: t.By, Where: t.Where}
		}
	}
	return out
}
