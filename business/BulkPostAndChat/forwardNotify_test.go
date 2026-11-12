package business

import (
	"context"
	"slices"
	"testing"

	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A message forwarded into DMs and groups goes through the one door
// (business/Notify Tell, A4 H2), one event per conversation, with the notice
// id a new message there has, so the router decides who hears, the desktop
// app gets its notice too (BE-11-G-2) and each delivery is recorded once; a
// conversation with no message is skipped.
func TestAForwardIsDeliveredLikeANewMessage(t *testing.T) {
	var got []notifyBusiness.Event
	old := tellForward
	t.Cleanup(func() { tellForward = old })
	tellForward = func(_ context.Context, ev notifyBusiness.Event) []notifyBusiness.Decision {
		got = append(got, ev)
		return nil
	}
	sendFwdMessageNotifications(context.Background(), &dgraphStruct.DgraphUser{Uuid: "maya", UserName: "maya"}, "<p>fyi</p>",
		[]*dgraphStruct.DgraphDm{dmNode("maya jonas", "c1"), dmNode("g1", "c2"), {GroupingId: "empty"}})
	type ev struct{ id, msg, actor, place, thread, typeID, body string }
	want := []ev{
		{"chat:maya jonas:c1", "c1", "maya", "maya jonas", "c1", "maya jonas", "fyi"},
		{"chat:g1:c2", "c2", "maya", "g1", "c2", "g1", "fyi"},
	}
	var have []ev
	for _, e := range got {
		if e.Cause != notifyBusiness.CauseMessage || e.Place.Kind != notifyBusiness.PlaceDM || e.Place.ThreadRootID != "" {
			t.Errorf("told %+v, want a new message in a DM or group", e)
		}
		have = append(have, ev{e.ID, e.MessageID, e.Actor, e.Place.ID, e.Push[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID],
			e.Push[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID], e.Push[firebaseInit.FIREBASE_PUSH_DATA_BODY]})
	}
	if !slices.Equal(have, want) {
		t.Errorf("told %+v, want %+v", have, want)
	}
}
