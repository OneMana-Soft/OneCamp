package business

import (
	"context"
	"reflect"
	"testing"
)

// Day 0 plans today's notices: the site's audience, each by push and desktop
// notice, never the actor, and delivers them through business/Notification
// Deliver with the event's push and its id, untouched.
func TestTodaysNoticesGoThroughDeliver(t *testing.T) {
	ev := Event{ID: "chat:g:m", Cause: CauseMessage, Actor: "me", Push: map[string]string{"title": "Maya"}, Audience: []string{"ann", "me", "bea", ""}}
	ds, err := Notices.Plan(context.Background(), ev)
	want := []Decision{
		{UserID: "ann", Cause: CauseMessage, To: []Delivery{ToPush, ToDesktop}},
		{UserID: "bea", Cause: CauseMessage, To: []Delivery{ToPush, ToDesktop}},
	}
	if err != nil || !reflect.DeepEqual(ds, want) {
		t.Fatalf("plan: %+v (%v)", ds, err)
	}

	was := deliver
	defer func() { deliver = was }()
	var got []string
	var gotData map[string]string
	var gotID string
	calls := 0
	deliver = func(_ context.Context, to []string, data map[string]string, id string) {
		calls++
		got, gotData, gotID = to, data, id
	}
	Notices.Deliver(context.Background(), ev, append(ds, Decision{UserID: "cat", To: []Delivery{ToInbox}}))
	if calls != 1 || !reflect.DeepEqual(got, []string{"ann", "bea"}) || gotData["title"] != "Maya" || gotID != "chat:g:m" {
		t.Errorf("delivered %v %v %q in %d calls", got, gotData, gotID, calls)
	}

	calls = 0
	Notices.Deliver(context.Background(), ev, nil)
	if calls != 0 {
		t.Error("nobody to tell, yet Deliver was called")
	}
}
