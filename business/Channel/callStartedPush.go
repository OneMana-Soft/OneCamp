package business

// "#launch - Maya started call" when a channel's call begins: its pushes
// (sendChannelCallNotification) and the call dot for the screens that still
// read MqttChannelCall. Sent when the first person really joins the call
// (call.started, from business/CallState), never when a token is minted
// (CL2).
//
// Chat's huddles (P10) make channel calls quiet (D-H2) through the
// notification router; the trigger stays here.

import (
	"context"
	"fmt"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/google/uuid"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
)

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{
		Name:  "channel.call.started",
		Wants: channelCallStarted,
		Run:   announceChannelCall,
	})
}

// channelCallStarted is a channel's call beginning. Pure.
func channelCallStarted(e resourceEventBusiness.Event) bool {
	return e.Kind == resourceEventBusiness.CallStarted && e.ResourceKind == "channel"
}

func announceChannelCall(ctx context.Context, e resourceEventBusiness.Event) {
	channelUUID := e.ResourceUUID
	mqttBusiness.PublishChannelCall(&mqttStruct.MqttChannelCall{CallActive: mqttStruct.MESSAGE_CALL_ACTIVE, ChannelUUID: channelUUID}, channelUUID)
	if e.Actor.UserUUID == "" {
		return
	}
	id, err := uuid.Parse(channelUUID)
	if err != nil {
		return
	}
	ch, err := channelDomain.GetChannelInfoByUUID(ctx, id)
	if err != nil || ch == nil {
		helpers.LogErrorWithContext(ctx, "business/Channel could not read the channel whose call began (%s): %+v", channelUUID, err)
		return
	}
	caller, err := userBusiness.GetDgraphUserInfoByUUID(ctx, e.Actor.UserUUID)
	if err != nil || caller == nil {
		helpers.LogErrorWithContext(ctx, "business/Channel could not read who started the call in %s: %+v", channelUUID, err)
		return
	}
	sendChannelCallNotification(fmt.Sprintf("#%s - %s", ch.Name, caller.DisplayName()), "started call", channelUUID, ch.Name, caller)
}
