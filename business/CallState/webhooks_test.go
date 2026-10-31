package business

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
)

// A call's beginning and end go to webhooks when it is a channel's or a
// meeting's; a DM's or a group chat's goes nowhere, as their messages don't, and
// nothing else a call does is a webhook.
func TestACallsWebhooksAreAChannelsOrAMeetingsBeginningAndEnd(t *testing.T) {
	for _, c := range []struct {
		kind  resourceEventBusiness.Kind
		place string
		want  bool
	}{
		{resourceEventBusiness.CallStarted, "channel", true},
		{resourceEventBusiness.CallEnded, "channel", true},
		{resourceEventBusiness.CallStarted, "meeting", true},
		{resourceEventBusiness.CallEnded, "meeting", true},
		{resourceEventBusiness.CallStarted, "dm", false},
		{resourceEventBusiness.CallEnded, "group", false},
		{resourceEventBusiness.CallJoined, "channel", false},
		{resourceEventBusiness.CallLocked, "meeting", false},
	} {
		if got := toWebhooks(resourceEventBusiness.Event{Kind: c.kind, ResourceKind: c.place}); got != c.want {
			t.Errorf("%s in a %s: to webhooks %v, want %v", c.kind, c.place, got, c.want)
		}
	}
}

// What a call's webhook carries: the call, where it is, who began or ended it,
// its times and, at its end, how long it went on, how many joined and why it
// ended. Never a name: the event's "by" is who started it, as the app says it.
func TestACallsWebhookCarriesIdsTimesAndCountsNeverAName(t *testing.T) {
	saved := dispatchWebhook
	t.Cleanup(func() { dispatchWebhook = saved })
	type sent struct {
		kind string
		data map[string]interface{}
	}
	var got []sent
	dispatchWebhook = func(_ context.Context, kind string, data interface{}) {
		got = append(got, sent{kind, data.(map[string]interface{})})
	}
	at := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	sendWebhook(context.Background(), resourceEventBusiness.Event{
		Kind: resourceEventBusiness.CallStarted, ResourceKind: "channel", ResourceUUID: "c1", At: at,
		Actor: resourceEventBusiness.Actor{UserUUID: "u1"},
		Data:  map[string]string{"session": "s1", "room": "c1", "kind": "huddle", "by": "Maya Chen", "started_at": "2026-10-30T09:00:00Z"},
	})
	sendWebhook(context.Background(), resourceEventBusiness.Event{
		Kind: resourceEventBusiness.CallEnded, ResourceKind: "meeting", ResourceUUID: "m1", At: at.Add(time.Hour),
		Actor: resourceEventBusiness.Actor{System: true},
		Data: map[string]string{"session": "s2", "room": "meet-m1", "kind": "meeting", "reason": "everyone_left", "duration_s": "1500",
			"people": "3", "started_at": "2026-10-30T09:35:00Z", "ended_at": "2026-10-30T10:00:00Z"},
	})
	want := []sent{
		{"call.started", map[string]interface{}{"session_id": "s1", "channel_id": "c1", "actor_uuid": "u1", "started_at": "2026-10-30T09:00:00Z"}},
		{"call.ended", map[string]interface{}{"session_id": "s2", "meeting_id": "m1", "started_at": "2026-10-30T09:35:00Z",
			"ended_at": "2026-10-30T10:00:00Z", "duration_s": 1500, "people": 3, "reason": "everyone_left"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sent %+v\nwant %+v", got, want)
	}
	for _, s := range got {
		b, _ := json.Marshal(s.data)
		if strings.Contains(string(b), "Maya") || strings.Contains(string(b), "meet-m1") {
			t.Errorf("%s carries a name or a room: %s", s.kind, b)
		}
	}
}
