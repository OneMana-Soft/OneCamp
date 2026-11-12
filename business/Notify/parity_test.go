package business

import (
	"context"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
)

// Parity (A4 H2): each chat site's people, as the site worked them out alone
// before it went through Notices, against the plan of the event it now
// builds. Every difference is named in its test; anything else is a defect.

// today is the rule the sites asked the database before H2
// (GetEligibleUsersFor{Channel,Chat}Activity's SQL, gone with H2 so that
// levelLets is the one copy): of those in the place, never the writer,
// everyone on "all" (or, limited to mentions, only those mentioned) and those
// on "mention" who are mentioned. "block" (Nothing) hears nothing. Pure.
func today(levels map[string]string, writer string, mentions []string, limitToMentions bool) []string {
	var out []string
	for id, level := range levels {
		named := slices.Contains(mentions, id)
		if id == writer {
			continue
		}
		if (level == "all" && (!limitToMentions || named)) || (level == "mention" && named) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func sorted(ids []string) []string {
	out := slices.Clone(ids)
	sort.Strings(out)
	return out
}

// place is everyone's setting where the sites' events happen: ann and hal
// hear everything, bo, cy, eve and gus mentions only, dee nothing; the
// writer, me, hears everything. zed isn't in it.
func place() map[string]string {
	return map[string]string{"me": "all", "ann": "all", "hal": "all", "bo": "mention", "cy": "mention", "eve": "mention", "gus": "mention", "dee": "block"}
}

// onAll is who hears everything there: what Members reads (only those on
// "all", with no cap, as the database's rule read them).
var onAll = []string{"ann", "hal", "me"}

func planOf(t *testing.T, in *fakeInputs, ev Event) []Decision {
	t.Helper()
	ds, err := routerOn(in).Plan(context.Background(), ev)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return ds
}

// A channel post (business/Post notifyNewPost). Push: today's eligible
// people, the mentioned, the inline reply's author, those its teams and
// broadcast reached and keyword matches having been folded into one mention
// list. Email: those its own chips name and the replied-to author. Activity
// (the live Inbox item): everyone in that mention list. No difference; each
// person now carries the strongest cause.
func TestParityChannelPost(t *testing.T) {
	in := &fakeInputs{levels: place(), members: onAll, teams: []string{"eve"}, keywords: []string{"bo"}}
	ev := Event{ID: "channel:ch:p", Cause: CauseMessage, Actor: "me", Place: channel, Push: body,
		Mentions: []string{"cy", "dee", "zed"}, Author: "gus", Teams: []string{"design"}}
	ds := planOf(t, in, ev)
	if got, want := sorted(By(ds, ToPush)), today(place(), "me", []string{"cy", "dee", "zed", "gus", "eve", "bo"}, false); !reflect.DeepEqual(got, want) {
		t.Errorf("pushed %v, today %v", got, want)
	}
	if got, want := sorted(By(ds, ToEmail)), []string{"cy", "gus"}; !reflect.DeepEqual(got, want) {
		t.Errorf("emailed %v, today %v (named by their own chip, or replied to)", got, want)
	}
	if got, want := sorted(By(ds, ToInbox)), []string{"bo", "cy", "eve", "gus"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Inbox items %v, today %v", got, want)
	}
	causes := map[string]Cause{}
	for _, d := range ds {
		causes[d.UserID] = d.Cause
	}
	want := map[string]Cause{"ann": CauseAllPosts, "hal": CauseAllPosts, "bo": CauseKeyword, "cy": CauseMention, "eve": CauseTeam, "gus": CauseReplyToYou, "zed": CauseMention}
	if !reflect.DeepEqual(causes, want) {
		t.Errorf("causes %v, want %v (zed: the not_in_place note, told nothing)", causes, want)
	}
}

// A DM or group message (business/Chat notifyNewChat). Push and email: today's
// eligible people. Activity: those mentioned or replied to. No difference;
// pinned: a DM at the "mention" level drops a plain message (today's rule
// too: domain/UserChatNotification GetEligibleUsersForChatActivity).
func TestParityDM(t *testing.T) {
	in := &fakeInputs{levels: place(), members: onAll}
	ev := Event{ID: "chat:g:m", Cause: CauseMessage, Actor: "me", Place: Place{Kind: PlaceGroup, ID: "g"}, Mentions: []string{"cy", "dee"}, Author: "gus"}
	ds := planOf(t, in, ev)
	todays := today(place(), "me", []string{"cy", "dee", "gus"}, false)
	if got := sorted(By(ds, ToPush)); !reflect.DeepEqual(got, todays) {
		t.Errorf("pushed %v, today %v", got, todays)
	}
	if got := sorted(By(ds, ToEmail)); !reflect.DeepEqual(got, todays) {
		t.Errorf("emailed %v, today %v", got, todays)
	}
	if got, want := sorted(Of(ds, CauseMention, CauseReplyToYou)), []string{"cy", "gus"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Activity %v, today %v", got, want)
	}
	if slices.Contains(Told(ds), "bo") {
		t.Error("bo, on mentions only, heard a plain message in the DM")
	}
}

// A reply in a DM's or group's thread (business/Chat notifyChatThreadReply),
// and in a channel's thread (business/Comment NotifyThreadReply): today's
// people are notificationBusiness.ThreadAudience's, called as each site
// called it. Named differences: in a channel's thread, keywords now reach
// (today's thread-reply site never looked for them); an author also
// mentioned gets one Inbox item, the mention, not two.
func TestParityThreadReply(t *testing.T) {
	follows := notificationBusiness.Follows{On: []string{"eve"}, Off: []string{"hal"}}
	for _, p := range []Place{{Kind: PlaceGroup, ID: "g", ThreadRootID: "root"}, inThread} {
		t.Run(p.Kind, func(t *testing.T) {
			in := &fakeInputs{levels: place(), members: onAll, follows: follows, keywords: []string{"gus"}}
			ev := Event{ID: "comment:root:r", Cause: CauseMessage, Actor: "me", Place: p, Push: body,
				Author: "ann", Repliers: []string{"bo", "dee", "hal"}, Mentions: []string{"cy"}}
			ds := planOf(t, in, ev)
			want := sorted(notificationBusiness.ThreadAudience(place(), "me", "ann", ev.Repliers, ev.Mentions, follows).Told)
			if p.Kind == PlaceChannel {
				want = sorted(append(want, "gus")) // named: a keyword now reaches a channel's thread reply
			}
			if got := sorted(Told(ds)); !reflect.DeepEqual(got, want) {
				t.Errorf("told %v, want %v", got, want)
			}
			if got := sorted(By(ds, ToPush)); !reflect.DeepEqual(got, want) {
				t.Errorf("pushed %v, want %v", got, want)
			}
		})
	}
}

// A reaction (business/Chat tellOfReaction): today its message's author, if
// in the conversation, gets a live Activity item whatever their setting for
// it, and a push on "all" or "mention" (never on Nothing or no setting).
// Plan tells the author in their Inbox on "all" or "mention"; the live item
// to an author on Nothing stays the site's (tellOfReaction). Named
// difference: it no longer pushes (D2: reactions never push or badge).
func TestParityReaction(t *testing.T) {
	for _, c := range []struct {
		author string
		told   bool
	}{{"bo", true}, {"ann", true}, {"dee", false}, {"zed", false}, {"me", false}} {
		in := &fakeInputs{levels: place(), members: onAll}
		ds := planOf(t, in, Event{ID: "reaction:g:m:me", Cause: CauseReaction, Actor: "me", Place: Place{Kind: PlaceDM, ID: "g"}, Author: c.author})
		if got := Told(ds); (len(got) == 1 && got[0] == c.author) != c.told || len(got) > 1 {
			t.Errorf("%s's message: told %v, want told %v", c.author, got, c.told)
		}
		if p := By(ds, ToPush); len(p) != 0 {
			t.Errorf("%s's message: a reaction pushed %v", c.author, p)
		}
	}
}

// A call (business/Channel and business/Chat: a huddle started): everyone on
// "all" there but the caller, as today. No difference.
func TestParityCall(t *testing.T) {
	for _, p := range []Place{channel, {Kind: PlaceDM, ID: "g"}} {
		in := &fakeInputs{levels: place(), members: onAll}
		ds := planOf(t, in, Event{ID: "call:" + p.ID, Cause: CauseCall, Actor: "me", Place: p})
		if got, want := sorted(By(ds, ToPush)), today(place(), "me", nil, false); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: pushed %v, today %v", p.Kind, got, want)
		}
	}
}

// A mention an edit adds (business/Post tellPostMentionsAdded and business/
// Chat tellChatMentionsAdded, TellAddedMentions): today's eligible people
// limited to those it names, in a channel and in a DM or group. No
// difference: dee, on Nothing, and zed, not in it, aren't told; ann, on "all"
// but not named, isn't either.
func TestParityMentionOnEdit(t *testing.T) {
	for _, p := range []Place{channel, {Kind: PlaceDM, ID: "g"}} {
		got := Told(planOf(t, &fakeInputs{levels: place(), members: onAll}, mentionAdded(p, "me", []string{"bo", "hal", "dee", "zed", "me"})))
		if want := today(place(), "me", []string{"bo", "hal", "dee", "zed", "me"}, true); !reflect.DeepEqual(sorted(got), want) {
			t.Errorf("%s: told %v, today %v", p.Kind, sorted(got), want)
		}
	}
}

// The chat sites keep no recipient list of their own: none asks the old
// eligibility rule, pushes straight, tells through business/Notification's
// notices (an edit's mention did, V-843: no row, no Why, no retract), or
// works out a thread's audience itself (a second copy of Plan is a defect).
func TestEveryChatSiteGoesThroughOneDoor(t *testing.T) {
	sites := []string{"../Post/postBusiness.go", "../Post/postMentions.go", "../Chat/chatBusiness.go", "../Chat/chatReactions.go",
		"../Chat/chatMentions.go", "../Chat/callRing.go", "../Channel/channelBusiness.go", "../Comment/threadReply.go",
		"../BulkPostAndChat/bulkPostAndChatBusiness.go"}
	banned := []string{"GetEligibleUsersForChatActivity", "GetEligibleUsersForChannelActivity", "DeliverFrom(",
		"notificationBusiness.Deliver(", "notificationBusiness.Deliver\n", "DeliverThroughPause(", "ThreadAudience(",
		"notificationBusiness.Tell(", "TellMentionsAdded("}
	for _, f := range sites {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") {
				continue
			}
			for _, b := range banned {
				if strings.Contains(code+"\n", b) {
					t.Errorf("%s:%d keeps its own list (%s): %s", f, i+1, strings.TrimSpace(b), code)
				}
			}
		}
	}
}
