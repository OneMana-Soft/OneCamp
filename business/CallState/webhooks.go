package business

// Calls' webhooks (zoom-15; design/calls-vs-zoom PROGRAM.md B14): call.started
// and call.ended, to the workspace's outgoing webhooks that subscribe to them
// (business/Webhook DispatchEvent), as Zoom's meeting.started and
// meeting.ended tell an account's apps.
//
// A payload is built for outside, as a doc's is (business/Follow webhooks.go):
// ids, times and counts, never a name, a title or anything said, so a
// workspace webhook may be sent it whole (business/Webhook wholeAtOrg).
//   - A call in a channel names its channel, which holds it to whoever may
//     read the channel, and a private one's reaches a workspace webhook as its
//     ids alone.
//   - A meeting's names the meeting, which holds it to whoever may read the
//     meeting's calls (MayRead: its host, or someone let into it), asked of
//     each webhook's owner (business/Webhook reach.go).
//   - A DM's or a group chat's call is sent to no webhook, as their messages
//     aren't (BE-BUG-13).
//
// A breakout room's call says nothing (emit): its call's events are its own.

import (
	"context"
	"errors"
	"strconv"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/google/uuid"
)

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "calls.webhooks", Wants: toWebhooks, Run: sendWebhook})
	webhookBusiness.ReadMeetingsWith(readsMeeting)
}

// toWebhooks is a call in a channel or a meeting beginning or ending. Pure.
func toWebhooks(e resourceEventBusiness.Event) bool {
	if e.Kind != resourceEventBusiness.CallStarted && e.Kind != resourceEventBusiness.CallEnded {
		return false
	}
	return e.ResourceKind == conversationBusiness.KindChannel || e.ResourceKind == KindMeeting
}

// dispatchWebhook is where a call's webhook goes; a seam for tests.
var dispatchWebhook = webhookBusiness.DispatchEvent

// sendWebhook is the sink.
func sendWebhook(ctx context.Context, e resourceEventBusiness.Event) {
	dispatchWebhook(ctx, string(e.Kind), webhookPayload(e))
}

// webhookPayload is what a call's webhook carries: the call, where it is, who
// began or ended it (none when nobody did: the last leave, the sweep), and
// its times; at its end, how long it went on, how many people joined it and
// why it ended. Pure.
func webhookPayload(e resourceEventBusiness.Event) map[string]interface{} {
	p := map[string]interface{}{"session_id": e.Data["session"]}
	if e.ResourceKind == KindMeeting {
		p["meeting_id"] = e.ResourceUUID
	} else {
		p["channel_id"] = e.ResourceUUID
	}
	if e.Actor.UserUUID != "" {
		p["actor_uuid"] = e.Actor.UserUUID
	}
	for _, k := range []string{"started_at", "ended_at"} {
		if v := e.Data[k]; v != "" {
			p[k] = v
		}
	}
	if e.Kind == resourceEventBusiness.CallEnded {
		duration, _ := strconv.Atoi(e.Data["duration_s"])
		people, _ := strconv.Atoi(e.Data["people"])
		p["duration_s"], p["people"], p["reason"] = duration, people, e.Data["reason"]
	}
	return p
}

// readsMeeting is whether someone may read a meeting's calls (MayRead): the
// rule business/Webhook holds a meeting's call's webhooks to. Someone who has
// left reads nothing.
func readsMeeting(ctx context.Context, userUUID, meetingID string) (bool, error) {
	id, err := uuid.Parse(userUUID)
	if err != nil {
		return false, nil
	}
	who, err := userDomain.ActorByUUID(ctx, id)
	if errors.Is(err, userDomain.ErrNoActor) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ok, _, err := MayRead(ctx, who, Ref{Kind: KindMeeting, ID: meetingID})
	return ok, err
}
