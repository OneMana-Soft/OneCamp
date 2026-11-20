package business

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// heardEmail is one email notifyNewChat asked for: to whom, and whether it
// says they were named ("mentioned you").
type heardEmail struct {
	to    []string
	named bool
}

// newMessageIn runs a new message by ada in a group, naming mentions and
// answering author's message, where the router decided decided, and says the
// emails it asked for (an empty one is sent to nobody, so isn't counted),
// whom its Activity line went to, and the event it told.
func newMessageIn(t *testing.T, author string, mentions []string, decided []notifyBusiness.Decision) (emails []heardEmail, items []string, told notifyBusiness.Event) {
	t.Helper()
	oldTell, oldEmail, oldPublish := tellChat, emailChatDM, publishActivityTo
	t.Cleanup(func() { tellChat, emailChatDM, publishActivityTo = oldTell, oldEmail, oldPublish })
	tellChat = func(_ context.Context, ev notifyBusiness.Event) []notifyBusiness.Decision {
		told = ev
		return decided
	}
	emailChatDM = func(_, _, _, _, _, _ string, to []string, named bool) {
		if len(to) > 0 {
			emails = append(emails, heardEmail{to: to, named: named})
		}
	}
	publishActivityTo = func(to string, _ *dgraphModels.UnifiedActivityItem) { items = append(items, to) }
	named := make([]*dgraphStruct.DgraphUser, len(mentions))
	for i, m := range mentions {
		named[i] = &dgraphStruct.DgraphUser{Uuid: m}
	}
	notifyNewChat(context.Background(), "Ada", "ship it", ada, "group-1", named, "message-1", "Ada", nil, author, nil)
	return emails, items, told
}

// A message in a group is emailed to each by how they heard it, as its
// Activity line reads them: the one it names or answers is told so, and the
// rest of the group get it as a message, so someone who set the group to
// mentions only isn't emailed "mentioned you" for another's mention (nor let
// through by the send's re-check, which reads that Heard).
func TestANewMessageIsEmailedByHowEachHeardIt(t *testing.T) {
	bo, cy := jonas, sam
	t.Run("a mention: the one named is told so, the rest are not", func(t *testing.T) {
		emails, items, _ := newMessageIn(t, "", []string{bo}, []notifyBusiness.Decision{
			{UserID: bo, Cause: notifyBusiness.CauseMention, To: []notifyBusiness.Delivery{notifyBusiness.ToEmail}},
			{UserID: cy, Cause: notifyBusiness.CauseDM, To: []notifyBusiness.Delivery{notifyBusiness.ToEmail}},
		})
		if want := []heardEmail{{to: []string{bo}, named: true}, {to: []string{cy}}}; !reflect.DeepEqual(emails, want) {
			t.Errorf("emails %+v, want %+v", emails, want)
		}
		if want := []string{bo}; !reflect.DeepEqual(items, want) {
			t.Errorf("Activity to %v, want %v", items, want)
		}
	})
	t.Run("a reply: the one answered is told so, the rest are not", func(t *testing.T) {
		emails, items, _ := newMessageIn(t, cy, nil, []notifyBusiness.Decision{
			{UserID: bo, Cause: notifyBusiness.CauseDM, To: []notifyBusiness.Delivery{notifyBusiness.ToEmail}},
			{UserID: cy, Cause: notifyBusiness.CauseReplyToYou, To: []notifyBusiness.Delivery{notifyBusiness.ToEmail}},
		})
		if want := []heardEmail{{to: []string{cy}, named: true}, {to: []string{bo}}}; !reflect.DeepEqual(emails, want) {
			t.Errorf("emails %+v, want %+v", emails, want)
		}
		if want := []string{cy}; !reflect.DeepEqual(items, want) {
			t.Errorf("Activity to %v, want %v", items, want)
		}
	})
	t.Run("nobody named: one plain email to those told by email", func(t *testing.T) {
		emails, _, _ := newMessageIn(t, "", nil, []notifyBusiness.Decision{
			{UserID: bo, Cause: notifyBusiness.CauseDM, To: []notifyBusiness.Delivery{notifyBusiness.ToEmail}},
			{UserID: cy, Cause: notifyBusiness.CauseDM, To: []notifyBusiness.Delivery{notifyBusiness.ToPush}},
		})
		if want := []heardEmail{{to: []string{bo}}}; !reflect.DeepEqual(emails, want) {
			t.Errorf("emails %+v, want %+v", emails, want)
		}
	})
}

// A mention in a group bigger than GroupEmailMost still emails the one it
// names, told so, while the rest, whom the plan doesn't email there, get no
// email (business/Notify TestABigGroupEmailsOnlyThoseItNamesOrAnswers plans
// the event as it is told here: a DM place, no audience, so the plan counts
// the conversation's members).
func TestABigGroupsMentionIsStillEmailed(t *testing.T) {
	decided := []notifyBusiness.Decision{{UserID: jonas, Cause: notifyBusiness.CauseMention, To: []notifyBusiness.Delivery{notifyBusiness.ToInbox, notifyBusiness.ToEmail}}}
	for i := range notifyBusiness.GroupEmailMost {
		decided = append(decided, notifyBusiness.Decision{UserID: fmt.Sprintf("member-%d", i), Cause: notifyBusiness.CauseDM, To: []notifyBusiness.Delivery{notifyBusiness.ToInbox, notifyBusiness.ToPush}})
	}
	emails, items, told := newMessageIn(t, "", []string{jonas}, decided)
	if told.Place.Kind != notifyBusiness.PlaceDM || told.Place.ID != "group-1" || len(told.Audience) != 0 {
		t.Errorf("told %+v, want the group as a DM place with no audience", told.Place)
	}
	if want := []heardEmail{{to: []string{jonas}, named: true}}; !reflect.DeepEqual(emails, want) {
		t.Errorf("emails %+v, want %+v", emails, want)
	}
	if want := []string{jonas}; !reflect.DeepEqual(items, want) {
		t.Errorf("Activity to %v, want %v", items, want)
	}
}

// A mention an edit adds to a DM's or a group's message, or to a reply in its
// thread, is asked again at its send (business/Notification verdicts): the
// message in its conversation, the reply when there is one, as someone it
// names.
func TestAnEditsChatMentionIsAskedAgainAtTheSend(t *testing.T) {
	ada := &dgraphStruct.DgraphUser{Uuid: "a", UserName: "Ada"}
	for _, reply := range []string{"", "c1"} {
		got := chatMentionNotice(ada, "Grp-1", "m1", reply, "<p>@Bo</p>").About
		want := &notificationBusiness.About{Kind: notificationBusiness.AboutChat, Item: "m1", Place: "Grp-1", Reply: reply, Heard: notifylevel.Named}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("reply %q: asked again as %+v, want %+v", reply, got, want)
		}
	}
}
