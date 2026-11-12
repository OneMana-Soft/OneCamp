package Calendar

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
)

// planSpy is the router, recording what it was asked to plan.
type planSpy struct {
	notifyBusiness.Router
	asked []notifyBusiness.Event
}

func (p *planSpy) Plan(ctx context.Context, ev notifyBusiness.Event) ([]notifyBusiness.Decision, error) {
	p.asked = append(p.asked, ev)
	return p.Router.Plan(ctx, ev)
}

// PARITY (A4 H1): the calendar's notices go through the router's Plan, and
// tell the same people by the same deliveries as before it: each guest but
// the organiser, once, as a guest, by push and email as their rules let them,
// or in the inbox alone when the organiser said not to send.
func TestCalendarNoticesThroughThePlanAreUnchanged(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	h.create(sam, "Design review", sam, maya, jonas)
	spy := &planSpy{Router: notifyBusiness.Notices}
	was := notifyBusiness.Notices
	notifyBusiness.Notices = spy
	t.Cleanup(func() { notifyBusiness.Notices = was })

	for _, send := range []bool{true, false} {
		spy.asked = nil
		got := captureNotices(t)
		for _, tl := range h.tells.of(toldMade) {
			h.s.tellMembers(ctx, sam, tl, send)
		}
		var told []notificationBusiness.NoticeRecipient
		for _, c := range *got {
			told = append(told, c.r...)
		}
		slices.SortFunc(told, func(a, b notificationBusiness.NoticeRecipient) int {
			return strings.Compare(a.UserUUID, b.UserUUID)
		})
		want := []notificationBusiness.NoticeRecipient{
			{UserUUID: maya.UserDgraphInfo.Uuid, Reason: "guest", InboxOnly: !send},
			{UserUUID: jonas.UserDgraphInfo.Uuid, Reason: "guest", InboxOnly: !send},
		}
		slices.SortFunc(want, func(a, b notificationBusiness.NoticeRecipient) int {
			return strings.Compare(a.UserUUID, b.UserUUID)
		})
		if !reflect.DeepEqual(told, want) {
			t.Errorf("send %v: told %+v, want %+v", send, told, want)
		}
		if len(spy.asked) != 1 {
			t.Fatalf("send %v: planned %d times, want once", send, len(spy.asked))
		}
		ev := spy.asked[0]
		if ev.Cause != notifyBusiness.CauseCalendarInvited || ev.Actor != sam.UserDgraphInfo.Uuid || ev.Quiet != !send || ev.Place.Kind != "" ||
			!slices.Contains(ev.Audience, maya.UserDgraphInfo.Uuid) || !slices.Contains(ev.Audience, jonas.UserDgraphInfo.Uuid) {
			t.Errorf("send %v: planned %+v", send, ev)
		}
	}
}
