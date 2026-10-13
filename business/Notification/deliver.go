package notification

// One notification, every way a person hears of it.
//
// A push reaches the devices that registered for web or mobile push. The
// OneCamp desktop app's webview has no web push, so it registers nothing, and
// someone who used only the desktop app heard of nothing: no DM, no mention,
// no reply (TN-BUG-12). The app does carry a native bridge (the Tauri
// notification plugin, granted to the workspace's origin), which turns a
// page's `new Notification()` into a native one; it only needs telling.
//
// Deliver tells both from one decision. The site works out who hears of the
// thing by person (the conversation's members, their level for it, who was
// mentioned, never the writer); Deliver applies the push gate by person too
// (pause, quiet hours, focus time: domain/UserFCMToken ReachableNow), then
// sends each one who passes a push to each of their devices and one desktop
// notice (MESSAGE_DESKTOP_NOTICE) on their own activity topic, which only they
// may read (business/MqttAccess). Only the desktop app acts on the notice; a
// browser tab ignores it, having had the push.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	fcmDomain "github.com/akashc777/OneCamp/domain/UserFCMToken"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// Deliver tells each of recipients (user ids) of one thing: a push to their
// devices and a desktop notice, to each one the push gate lets through now.
// pushData is the push's data (firebaseInit's keys); noticeID names the thing
// and is the same for every copy of it, so the app raises it once: the email
// dispatcher's dedup parts where there are some, else
// "<type>:<type_id>:<message or thread id>". Run it after the write has
// committed, on the goroutine that tells people.
func Deliver(ctx context.Context, recipients []string, pushData map[string]string, noticeID string) {
	if err := deliver(ctx, recipients, pushData, noticeID, fcmDomain.ReachableNow); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Notification Deliver %s: %+v", noticeID, err)
	}
}

// DeliverThroughPause is Deliver for the one notice a pause doesn't hold:
// Notify anyway (urgent.go), limited to one a day per sender. It says when
// the read or the push failed, which the sender is told.
func DeliverThroughPause(ctx context.Context, recipients []string, pushData map[string]string, noticeID string) error {
	return deliver(ctx, recipients, pushData, noticeID, fcmDomain.ReachableThroughPause)
}

// fcmBatch is how many devices one push call may name (Firebase's limit).
const fcmBatch = 500

// deliver sends the notices and the push, and says what failed: the read of
// who may be told (then nobody is), or the push.
func deliver(ctx context.Context, recipients []string, pushData map[string]string, noticeID string,
	reach func(context.Context, []string) ([]string, []string, error)) error {
	ctx = context.WithoutCancel(ctx)
	users, tokens, err := reach(ctx, recipients)
	if err != nil {
		return fmt.Errorf("could not read who may be told, telling nobody: %w", err)
	}
	if len(users) == 0 {
		return nil
	}

	// The notices first: they cost a publish each, where the push waits on
	// Firebase. Built before the push, which writes into pushData.
	notice := desktopNotice(pushData, noticeID, time.Now())
	for _, u := range users {
		mqttBusiness.PublishMessageToUser(u, mqttStruct.MESSAGE_DESKTOP_NOTICE, &notice)
	}

	for batch := range slices.Chunk(tokens, fcmBatch) {
		if err := firebaseInit.FirebaseApp.MultiCastPush(ctx, pushData, batch); err != nil {
			return fmt.Errorf("push failed: %w", err)
		}
	}
	return nil
}

// noticeMax is the most a notice's whole message may weigh. A browser takes
// far more, but a notice is a notification: about 1 KB of text, a title, a
// few ids. Kept to the push's own ceiling so one paste can't make it the
// largest message on the topic.
const noticeMax = 4000

// desktopNotice is the notice for pushData: the push's own data, so the app
// routes it exactly as a tap on the push, with the body cut to
// helpers.PushBodyMax and the whole message held under noticeMax. Pure.
func desktopNotice(pushData map[string]string, noticeID string, now time.Time) mqttStruct.MqttDesktopNotice {
	data := make(map[string]string, len(pushData))
	for k, v := range pushData {
		data[k] = v
	}
	if body, ok := data[firebaseInit.FIREBASE_PUSH_DATA_BODY]; ok {
		data[firebaseInit.FIREBASE_PUSH_DATA_BODY] = helpers.CutPushBody(body, helpers.PushBodyMax)
	}
	n := mqttStruct.MqttDesktopNotice{NoticeID: noticeID, SentAt: mqttStruct.DesktopNoticeTime(now), Data: data}

	// Escaping can make a body that fits as text weigh more as JSON: give up
	// what the message is over, from the body, then the icon (a signed link,
	// and the desktop app shows its own), then the title.
	for _, key := range []string{firebaseInit.FIREBASE_PUSH_DATA_BODY, firebaseInit.FIREBASE_PUSH_DATA_ICON, firebaseInit.FIREBASE_PUSH_DATA_TITLE} {
		over := noticeSize(n) - noticeMax
		if over <= 0 {
			break
		}
		if v, ok := data[key]; ok {
			data[key] = helpers.CutPushBody(v, len(v)-over-len("…")*2)
		}
	}
	return n
}

// noticeSize is how much n weighs as the message on the topic.
func noticeSize(n mqttStruct.MqttDesktopNotice) int {
	b, err := json.Marshal(mqttStruct.Message{Type: mqttStruct.MESSAGE_DESKTOP_NOTICE, Data: &n})
	if err != nil {
		return 0
	}
	return len(b)
}
