package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// teamScopeIDs extracts the user's team scope-entity ids for command
// resolution. Team commands are visible only to members of that team.
func teamScopeIDs(user userModels.UserInfo) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(user.UserDgraphInfo.Teams))
	for _, t := range user.UserDgraphInfo.Teams {
		if id, err := uuid.Parse(t.Uuid); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// channelScopeIDs extracts the user's channel scope-entity ids. Channel
// commands are visible only to members of that channel.
func channelScopeIDs(user userModels.UserInfo) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(user.UserDgraphInfo.Channels))
	for _, c := range user.UserDgraphInfo.Channels {
		if id, err := uuid.Parse(c.Uuid); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// trimSpace is a tiny indirection so dispatcher.go reads cleanly.
func trimSpace(s string) string { return strings.TrimSpace(s) }

// deliverAsync pushes an app's answer to its command to the invoker over
// MQTT: an external app's callback, which arrives after the synchronous request
// has already returned. It says it is an app's (KindApp). The payload rides
// the user's existing activity topic, so it needs no new EMQX ACL rule.
func deliverAsync(_ context.Context, cc CommandContext, resp *commandAdapter.CommandResponse) {
	if resp != nil {
		resp.Kind = commandAdapter.KindApp
	}
	PublishEphemeralToUser(cc.User.UserDgraphInfo.Uuid, resp)
}

// PublishEphemeralToUser publishes an ephemeral command response to a single
// user's activity topic. Exported so the scheduler worker (a different
// package) can deliver reminders without importing dispatcher internals.
func PublishEphemeralToUser(userUUID string, resp *commandAdapter.CommandResponse) {
	if resp == nil || mqttInit.MqttClient == nil {
		return
	}
	msg := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_COMMAND_EPHEMERAL,
		Data: resp,
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf("business/Command/PublishEphemeralToUser marshal err: %+v", err)
		return
	}
	token := mqttInit.MqttClient.Publish(helpers.GetMqttTopicForUserActivity(userUUID), 1, false, payload)
	go func() {
		defer func() { _ = recover() }()
		_ = token.WaitTimeout(mqttInit.PublishWait)
		if token.Error() != nil {
			helpers.MessageLogs.ErrorLog.Printf("business/Command/PublishEphemeralToUser publish err: %+v", token.Error())
		}
	}()
}

// pushReminder tells the person a reminder is for that it came due: a push,
// so a backgrounded or closed app shows it through the service worker, and the
// desktop app's notice, whose webview has no push (business/Notification
// Deliver). The notice goes out with push switched off too. Best-effort: the
// scheduler job succeeds whatever happens here.
func pushReminder(ctx context.Context, userUUID string, text string) {
	pushData := map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:    firebaseInit.FIREBASE_PUSH_DATA_TYPE_REMINDER,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID: userUUID,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:   "Reminder",
		firebaseInit.FIREBASE_PUSH_DATA_BODY:    text,
	}
	notificationBusiness.Deliver(ctx, []string{userUUID}, pushData, fmt.Sprintf("reminder:%s:%d", userUUID, time.Now().UnixMilli()))
}
