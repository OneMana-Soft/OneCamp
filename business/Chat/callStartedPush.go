package business

// "Call from Maya" when a DM or group call begins: its push, desktop notice
// and email (sendChatCallNotification), and the call dot for the screens that
// still read MqttChatCall. Sent when the first person really joins the call
// (call.started, from business/CallState), never when a token is minted
// (CL2): someone who opened the green room and walked away rang nobody.
//
// Chat's huddles (P10) replace the deliveries with the notification router;
// the trigger stays here.

import (
	"context"
	"fmt"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
)

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{
		Name:  "chat.call.started",
		Wants: chatCallStarted,
		Run:   ringChatCall,
	})
}

// chatCallStarted is a DM's or a group's call beginning. Pure.
func chatCallStarted(e resourceEventBusiness.Event) bool {
	return e.Kind == resourceEventBusiness.CallStarted && (e.ResourceKind == "dm" || e.ResourceKind == "group")
}

// chatCallPush is what a DM's or a group's call start says. Pure.
func chatCallPush(callerName string, group bool) (title, body string) {
	if group {
		return fmt.Sprintf("%s started group call", callerName), "started call"
	}
	return fmt.Sprintf("Call from %s", callerName), "calling you..."
}

func ringChatCall(ctx context.Context, e resourceEventBusiness.Event) {
	grpID := e.ResourceUUID
	mqttBusiness.PublishChatCall(&mqttStruct.MqttChatCall{CallActive: mqttStruct.MESSAGE_CALL_ACTIVE, GrpId: grpID}, grpID)
	if e.Actor.UserUUID == "" {
		return
	}
	caller, err := userBusiness.GetDgraphUserInfoByUUID(ctx, e.Actor.UserUUID)
	if err != nil || caller == nil {
		helpers.LogErrorWithContext(ctx, "business/Chat could not read who started the call in %s: %+v", grpID, err)
		return
	}
	title, body := chatCallPush(caller.DisplayName(), e.ResourceKind == "group")
	sendChatCallNotification(title, body, caller.Uuid, grpID, caller.DisplayName(), caller.ProfileKey)
}
