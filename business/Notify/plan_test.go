package business

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
)

// The router plans (A4 H1): the audience by cause, access, one cause a
// person, preference and the hold, from inputs read in batches, and Explain
// tells the person asking about their own part alone.

var at = time.Date(2026, 11, 7, 23, 0, 0, 0, time.UTC)

// fakeInputs answer Plan's reads and count them: who was asked about, by
// read, so a read made a person at a time fails the test.
type fakeInputs struct {
	members   []string
	levels    map[string]string
	follows   notificationBusiness.Follows
	teams     []string
	broadcast []string
	keywords  []string
	vips      map[string]bool
	held      map[string]notificationBusiness.HeldStatus

	reads map[string]int
	asked map[string][]string
}

func (f *fakeInputs) read(name string, ids ...string) {
	if f.reads == nil {
		f.reads, f.asked = map[string]int{}, map[string][]string{}
	}
	f.reads[name]++
	f.asked[name] = append(f.asked[name], ids...)
}

func (f *fakeInputs) Members(_ context.Context, _ Place) ([]string, error) {
	f.read("members")
	return f.members, nil
}

func (f *fakeInputs) Levels(_ context.Context, _ Place, ids []string) (map[string]string, error) {
	f.read("levels", ids...)
	out := map[string]string{}
	for _, id := range ids {
		if l, ok := f.levels[key(id)]; ok {
			out[key(id)] = l
		}
	}
	return out, nil
}

func (f *fakeInputs) Follows(_ context.Context, _ Place) (notificationBusiness.Follows, error) {
	f.read("follows")
	return f.follows, nil
}

func (f *fakeInputs) Reached(_ context.Context, _ Event, teams []string, broadcast string) ([]string, error) {
	f.read("reached")
	if len(teams) > 0 {
		return f.teams, nil
	}
	if broadcast != "" {
		return f.broadcast, nil
	}
	return nil, nil
}

func (f *fakeInputs) Keywords(_ context.Context, _ Place, text, _ string) ([]string, error) {
	f.read("keywords")
	if text == "" {
		return nil, nil
	}
	return f.keywords, nil
}

func (f *fakeInputs) VIPs(_ context.Context, _ string, ids []string) (map[string]bool, error) {
	f.read("vips", ids...)
	return f.vips, nil
}

func (f *fakeInputs) Held(_ context.Context, ids []string, _ time.Time) (map[string]notificationBusiness.HeldStatus, error) {
	f.read("held", ids...)
	out := map[string]notificationBusiness.HeldStatus{}
	for _, id := range ids {
		if st, ok := f.held[key(id)]; ok {
			out[key(id)] = st
		}
	}
	return out, nil
}

func routerOn(in Inputs) router { return router{in: in, now: func() time.Time { return at }} }

func planned(t *testing.T, in Inputs, ev Event) map[string]Decision {
	t.Helper()
	ds, err := routerOn(in).Plan(context.Background(), ev)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	out := map[string]Decision{}
	for _, d := range ds {
		if _, twice := out[d.UserID]; twice {
			t.Fatalf("%s decided twice: %+v", d.UserID, ds)
		}
		out[d.UserID] = d
	}
	return out
}

func all(ids ...string) map[string]string {
	out := map[string]string{}
	for _, id := range ids {
		out[id] = "all"
	}
	return out
}

var (
	loud     = []Delivery{ToInbox, ToBadge, ToPush, ToDesktop, ToSound}
	named    = []Delivery{ToInbox, ToBadge, ToPush, ToDesktop, ToSound, ToEmail}
	channel  = Place{Kind: PlaceChannel, ID: "ch"}
	inThread = Place{Kind: PlaceChannel, ID: "ch", ThreadRootID: "root"}
	body     = map[string]string{firebaseInit.FIREBASE_PUSH_DATA_BODY: "the launch moved"}
)

// Each cause has its own audience, each read through its input, and the
// actor never hears of their own message.
func TestEveryCauseHasItsAudience(t *testing.T) {
	cases := []struct {
		name  string
		in    *fakeInputs
		ev    Event
		cause Cause
		to    []Delivery
	}{
		{"dm", &fakeInputs{levels: all("ann", "me")},
			Event{Cause: CauseMessage, Actor: "me", Place: Place{Kind: PlaceDM, ID: "g"}, Audience: []string{"me", "ann"}}, CauseDM, named},
		{"mention", &fakeInputs{levels: map[string]string{"ann": "mention"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Mentions: []string{"ann", "me"}}, CauseMention, named},
		{"reply_to_you", &fakeInputs{levels: map[string]string{"ann": "mention"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Author: "ann"}, CauseReplyToYou, named},
		{"thread_reply", &fakeInputs{levels: all("ann")},
			Event{Cause: CauseMessage, Actor: "me", Place: inThread, Author: "me", Repliers: []string{"ann", "me"}}, CauseThreadReply, loud},
		{"thread_reply, following", &fakeInputs{levels: all("ann"), follows: notificationBusiness.Follows{On: []string{"ann"}}},
			Event{Cause: CauseMessage, Actor: "me", Place: inThread, Author: "me"}, CauseThreadReply, loud},
		{"team", &fakeInputs{levels: map[string]string{"ann": "mention"}, teams: []string{"ann"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Teams: []string{"design"}}, CauseTeam, loud},
		{"keyword", &fakeInputs{levels: map[string]string{"ann": "mention"}, keywords: []string{"ann"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Push: body}, CauseKeyword, loud},
		{"broadcast", &fakeInputs{levels: map[string]string{"ann": "mention"}, broadcast: []string{"ann"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Broadcast: "channel"}, CauseBroadcast, loud},
		{"all_posts", &fakeInputs{levels: all("ann", "me"), members: []string{"me", "ann"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel}, CauseAllPosts, []Delivery{ToPush, ToDesktop, ToSound}},
		{"reaction", &fakeInputs{levels: all("ann")},
			Event{Cause: CauseReaction, Actor: "me", Place: channel, Author: "ann"}, CauseReaction, []Delivery{ToInbox}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planned(t, c.in, c.ev)
			if _, told := got["me"]; told {
				t.Errorf("the actor hears of their own message: %+v", got)
			}
			want := map[string]Decision{"ann": {UserID: "ann", Cause: c.cause, To: c.to}}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("plan %+v, want %+v", got, want)
			}
		})
	}
}

// Rank is the design's: dm > mention > reply_to_you > thread_reply > team >
// keyword > broadcast > all_posts > reaction.
func TestTheStrongestCauseWins(t *testing.T) {
	order := []Cause{CauseDM, CauseMention, CauseReplyToYou, CauseThreadReply, CauseTeam, CauseKeyword, CauseBroadcast, CauseAllPosts, CauseReaction}
	for i := range order {
		for j := i + 1; j < len(order); j++ {
			if got := strongest([]Cause{order[j], order[i]}); got != order[i] {
				t.Errorf("strongest of %s and %s is %s", order[j], order[i], got)
			}
		}
	}
	// And in a plan: one decision a person, by the stronger cause.
	pairs := []struct {
		name string
		in   *fakeInputs
		ev   Event
		want Cause
	}{
		{"dm over mention", &fakeInputs{levels: all("ann")},
			Event{Cause: CauseMessage, Actor: "me", Place: Place{Kind: PlaceGroup, ID: "g"}, Audience: []string{"ann"}, Mentions: []string{"ann"}}, CauseDM},
		{"mention over reply_to_you", &fakeInputs{levels: all("ann")},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Mentions: []string{"ann"}, Author: "ann"}, CauseMention},
		{"mention over thread_reply", &fakeInputs{levels: all("ann")},
			Event{Cause: CauseMessage, Actor: "me", Place: inThread, Author: "me", Repliers: []string{"ann"}, Mentions: []string{"ann"}}, CauseMention},
		{"reply_to_you over thread_reply", &fakeInputs{levels: all("ann")},
			Event{Cause: CauseMessage, Actor: "me", Place: inThread, Author: "ann", Repliers: []string{"ann"}}, CauseReplyToYou},
		{"thread_reply over team", &fakeInputs{levels: all("ann"), teams: []string{"ann"}},
			Event{Cause: CauseMessage, Actor: "me", Place: inThread, Author: "me", Repliers: []string{"ann"}, Teams: []string{"t"}}, CauseThreadReply},
		{"team over keyword", &fakeInputs{levels: all("ann"), teams: []string{"ann"}, keywords: []string{"ann"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Teams: []string{"t"}, Push: body}, CauseTeam},
		{"keyword over broadcast", &fakeInputs{levels: all("ann"), keywords: []string{"ann"}, broadcast: []string{"ann"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Broadcast: "here", Push: body}, CauseKeyword},
		{"broadcast over all_posts", &fakeInputs{levels: all("ann"), broadcast: []string{"ann"}, members: []string{"ann"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Broadcast: "channel"}, CauseBroadcast},
	}
	for _, p := range pairs {
		got := planned(t, p.in, p.ev)
		if len(got) != 1 || got["ann"].Cause != p.want {
			t.Errorf("%s: %+v", p.name, got)
		}
	}
}

// Access: someone not in the place now hears nothing of it; someone mentioned
// there gets a note for the composer's invite prompt, and no delivery.
func TestOnlyThoseInThePlaceHear(t *testing.T) {
	in := &fakeInputs{levels: all("ann"), members: []string{"ann", "bea"}, teams: []string{"cat"}}
	got := planned(t, in, Event{Cause: CauseMessage, Actor: "me", Place: channel,
		Mentions: []string{"dan"}, Teams: []string{"t"}})
	want := map[string]Decision{
		"ann": {UserID: "ann", Cause: CauseAllPosts, To: []Delivery{ToPush, ToDesktop, ToSound}},
		"dan": {UserID: "dan", Cause: CauseMention, Note: NotInPlace},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plan %+v, want %+v", got, want)
	}
	if in.reads["levels"] != 1 {
		t.Errorf("access read %d times", in.reads["levels"])
	}
}

// Preference: the place's level, mute ("Nothing"), the thread's follow
// choice; reactions never push or badge; broadcasts never come from threads.
func TestEachPreferenceRule(t *testing.T) {
	cases := []struct {
		name string
		in   *fakeInputs
		ev   Event
		want map[string]Decision
	}{
		{"everything hears every message", &fakeInputs{levels: all("ann"), members: []string{"ann"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel},
			map[string]Decision{"ann": {UserID: "ann", Cause: CauseAllPosts, To: []Delivery{ToPush, ToDesktop, ToSound}}}},
		{"mentions only hears no message that isn't theirs", &fakeInputs{levels: map[string]string{"ann": "mention"}, members: []string{"ann"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel}, map[string]Decision{}},
		{"mentions only in a DM hears no plain message", &fakeInputs{levels: map[string]string{"ann": "mention"}},
			Event{Cause: CauseMessage, Actor: "me", Place: Place{Kind: PlaceDM, ID: "g"}, Audience: []string{"ann"}}, map[string]Decision{}},
		{"muted hears not even a mention", &fakeInputs{levels: map[string]string{"ann": "block"}},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Mentions: []string{"ann"}}, map[string]Decision{}},
		{"muted hears no reaction", &fakeInputs{levels: map[string]string{"ann": "block"}},
			Event{Cause: CauseReaction, Actor: "me", Place: channel, Author: "ann"}, map[string]Decision{}},
		{"a thread turned off is quiet", &fakeInputs{levels: all("ann"), follows: notificationBusiness.Follows{Off: []string{"ann"}}},
			Event{Cause: CauseMessage, Actor: "me", Place: inThread, Author: "me", Repliers: []string{"ann"}}, map[string]Decision{}},
		{"a thread turned off still tells a mention", &fakeInputs{levels: all("ann"), follows: notificationBusiness.Follows{Off: []string{"ann"}}},
			Event{Cause: CauseMessage, Actor: "me", Place: inThread, Author: "me", Repliers: []string{"ann"}, Mentions: []string{"ann"}},
			map[string]Decision{"ann": {UserID: "ann", Cause: CauseMention, To: named}}},
		{"muted hears a reply to their own message in its thread", &fakeInputs{levels: map[string]string{"ann": "block"}},
			Event{Cause: CauseMessage, Actor: "me", Place: inThread, Author: "ann"},
			map[string]Decision{"ann": {UserID: "ann", Cause: CauseReplyToYou, To: named}}},
		{"muted hears no reply in a thread they answered once", &fakeInputs{levels: map[string]string{"ann": "block"}},
			Event{Cause: CauseMessage, Actor: "me", Place: inThread, Author: "me", Repliers: []string{"ann"}}, map[string]Decision{}},
		{"a broadcast never comes from a thread", &fakeInputs{levels: all("ann"), broadcast: []string{"ann"}},
			Event{Cause: CauseMessage, Actor: "me", Place: inThread, Author: "me", Broadcast: "channel"}, map[string]Decision{}},
		{"a reaction never pushes or badges", &fakeInputs{levels: all("ann")},
			Event{Cause: CauseReaction, Actor: "me", Place: channel, Author: "ann"},
			map[string]Decision{"ann": {UserID: "ann", Cause: CauseReaction, To: []Delivery{ToInbox}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planned(t, c.in, c.ev); !reflect.DeepEqual(got, c.want) {
				t.Errorf("plan %+v, want %+v", got, c.want)
			}
		})
	}
}

// Hold: pause, schedule and focus time hold push, desktop and sound; the
// inbox and badge still move; email waits; a VIP and Notify anyway pass.
func TestTheHoldAndWhatPassesIt(t *testing.T) {
	till := at.Add(9 * time.Hour)
	heldAnn := map[string]notificationBusiness.HeldStatus{"ann": {Until: &till, Reason: "quiet_hours"}}
	mention := Event{Cause: CauseMessage, Actor: "me", Place: channel, Mentions: []string{"ann"}}
	cases := []struct {
		name string
		in   *fakeInputs
		ev   Event
		want Decision
	}{
		{"held: inbox and badge now, email at the end of the hold", &fakeInputs{levels: all("ann"), held: heldAnn}, mention,
			Decision{UserID: "ann", Cause: CauseMention, To: []Delivery{ToInbox, ToBadge}, Held: []Delivery{ToPush, ToDesktop, ToSound, ToEmail}, HoldTill: &till}},
		{"not held", &fakeInputs{levels: all("ann")}, mention,
			Decision{UserID: "ann", Cause: CauseMention, To: named}},
		{"a VIP passes the hold", &fakeInputs{levels: all("ann"), held: heldAnn, vips: map[string]bool{"ann": true}}, mention,
			Decision{UserID: "ann", Cause: CauseMention, To: named}},
		{"Notify anyway passes the hold", &fakeInputs{levels: all("ann"), held: heldAnn},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Mentions: []string{"ann"}, Anyway: []string{"ANN"}},
			Decision{UserID: "ann", Cause: CauseMention, To: named}},
		{"a held message where they hear everything waits for nothing", &fakeInputs{levels: all("ann"), members: []string{"ann"}, held: heldAnn},
			Event{Cause: CauseMessage, Actor: "me", Place: channel},
			Decision{UserID: "ann", Cause: CauseAllPosts, Held: []Delivery{ToPush, ToDesktop, ToSound}, HoldTill: &till}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planned(t, c.in, c.ev)
			if !reflect.DeepEqual(got, map[string]Decision{"ann": c.want}) {
				t.Errorf("plan %+v, want %+v", got["ann"], c.want)
			}
		})
	}
}

// Explain is the asker's own part, step by step, and nothing of anyone
// else's: no other person's id, no setting of theirs read, never a body.
func TestExplainAnswersTheAskerAlone(t *testing.T) {
	till := at.Add(time.Hour)
	ev := Event{ID: "channel:ch:m1", Cause: CauseMessage, Actor: "me", Place: channel, Push: body,
		Mentions: []string{"ann", "bea"}}
	in := &fakeInputs{levels: all("ann", "bea"), held: map[string]notificationBusiness.HeldStatus{"ann": {Until: &till, Reason: "paused"}}}
	r := routerOn(in)

	tr, err := r.Explain(context.Background(), "ann", ev)
	if err != nil {
		t.Fatal(err)
	}
	wantSteps := []Step{
		{Step: "audience", Verdict: "in", Causes: []Cause{CauseMention}},
		{Step: "access", Verdict: "in_place"},
		{Step: "cause", Verdict: "chosen", Causes: []Cause{CauseMention}},
		{Step: "preference", Verdict: "kept"},
		{Step: "hold", Verdict: "held:paused"},
	}
	ds, _ := routerOn(&fakeInputs{levels: all("ann", "bea"), held: in.held}).Plan(context.Background(), ev)
	if len(ds) == 0 {
		t.Fatalf("no plan to explain: trace %+v", tr)
	}
	if tr.UserID != "ann" || tr.EventID != ev.ID || !reflect.DeepEqual(tr.Steps, wantSteps) || tr.Decision == nil || !reflect.DeepEqual(*tr.Decision, ds[0]) {
		t.Errorf("trace %+v (decision %+v), want steps %+v and %+v", tr, tr.Decision, wantSteps, ds[0])
	}
	for name, ids := range in.asked {
		for _, id := range ids {
			if id != "ann" {
				t.Errorf("explaining ann's part read %s's %s", id, name)
			}
		}
	}

	// Someone the message isn't for learns that, and nothing about who it was for.
	in = &fakeInputs{levels: all("ann", "bea", "cat")}
	tr, err = routerOn(in).Explain(context.Background(), "cat", ev)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(tr)
	if tr.UserID != "cat" || tr.Decision != nil || !reflect.DeepEqual(tr.Steps, []Step{{Step: "audience", Verdict: "not_in_audience"}}) ||
		strings.Contains(string(raw), `"ann"`) || strings.Contains(string(raw), `"bea"`) || strings.Contains(string(raw), "launch") {
		t.Errorf("cat's trace %s", raw)
	}
	if in.reads["levels"]+in.reads["held"]+in.reads["vips"] != 0 {
		t.Errorf("explaining to someone it isn't for read settings: %v", in.asked)
	}

	// The actor hears nothing of their own message, and is told so.
	if tr, _ := routerOn(&fakeInputs{}).Explain(context.Background(), "me", ev); !reflect.DeepEqual(tr.Steps, []Step{{Step: "audience", Verdict: "actor"}}) {
		t.Errorf("actor's trace %+v", tr)
	}
	// Nobody asking is no answer.
	if _, err := routerOn(&fakeInputs{}).Explain(context.Background(), "", ev); err == nil {
		t.Error("Explain answered nobody")
	}
}

// A 1,000-member channel plans in budget, every input read once for all of
// them: a read made a person at a time fails.
func TestAThousandMembersPlanInBatches(t *testing.T) {
	in := &fakeInputs{levels: map[string]string{}, held: map[string]notificationBusiness.HeldStatus{}, vips: map[string]bool{}}
	till := at.Add(time.Hour)
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("u%04d", i)
		in.members = append(in.members, id)
		in.levels[id] = []string{"all", "mention", "block"}[i%3]
		switch {
		case i%10 == 0:
			in.teams = append(in.teams, id)
		case i%10 == 1:
			in.keywords = append(in.keywords, id)
		}
		if i%4 == 0 {
			in.held[id] = notificationBusiness.HeldStatus{Until: &till, Reason: "focus"}
		}
		if i%8 == 0 {
			in.vips[id] = true
		}
	}
	ev := Event{Cause: CauseMessage, Actor: "u0001", Place: channel, Push: body, Teams: []string{"t"},
		Broadcast: "here", Mentions: []string{"u0002", "u0005", "nobody"}}
	in.broadcast = in.members[:200]

	start := time.Now()
	ds, err := routerOn(in).Plan(context.Background(), ev)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("1,000 members: %d decisions in %v, reads %v", len(ds), took, in.reads)
	if took > 250*time.Millisecond {
		t.Errorf("a 1,000-member plan took %v", took)
	}
	if len(ds) < 300 {
		t.Errorf("only %d decisions", len(ds))
	}
	total := 0
	for name, n := range in.reads {
		total += n
		if n > 1 && name != "reached" || n > 2 {
			t.Errorf("%s read %d times", name, n)
		}
	}
	if total > 8 {
		t.Errorf("%d reads: %v", total, in.reads)
	}
}

// An event with no place is its site's: the site's audience, each once,
// never the actor, by inbox, push and desktop notice, or by the inbox alone
// when the site says not to send; and nothing is read.
func TestASitesAudienceIsAnsweredAsToday(t *testing.T) {
	in := &fakeInputs{}
	ev := Event{ID: "e", Cause: CauseCalendarInvited, Actor: "me", Audience: []string{"ann", "me", "", "bea", "ann"}}
	ds, err := routerOn(in).Plan(context.Background(), ev)
	want := []Decision{
		{UserID: "ann", Cause: CauseCalendarInvited, To: []Delivery{ToInbox, ToPush, ToDesktop}},
		{UserID: "bea", Cause: CauseCalendarInvited, To: []Delivery{ToInbox, ToPush, ToDesktop}},
	}
	if err != nil || !reflect.DeepEqual(ds, want) {
		t.Errorf("plan %+v (%v)", ds, err)
	}
	ev.Quiet = true
	ds, _ = routerOn(in).Plan(context.Background(), ev)
	if len(ds) != 2 || !reflect.DeepEqual(ds[1].To, []Delivery{ToInbox}) {
		t.Errorf("Don't send: %+v", ds)
	}
	if len(in.reads) != 0 {
		t.Errorf("a site's audience read %v", in.reads)
	}
}

// A reply in a channel's thread is the thread's people's, never every
// member's message: a member who hears everything in the channel, who neither
// replied in the thread nor follows it, hears nothing of it (CR-B48 P13), and
// the channel's members aren't read for it at all.
func TestAChannelThreadReplyIsNotEveryMembersMessage(t *testing.T) {
	in := &fakeInputs{levels: all("ann", "bo"), members: []string{"ann", "bo"}}
	got := planned(t, in, Event{Cause: CauseMessage, Actor: "me", Place: inThread, Author: "ann"})
	want := map[string]Decision{"ann": {UserID: "ann", Cause: CauseReplyToYou, To: named}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plan %+v, want Ann alone (her message's thread); Bo is on Everything but not in the thread", got)
	}
	if in.reads["members"] != 0 {
		t.Errorf("the channel's members read %d times for a thread reply", in.reads["members"])
	}
}

// Whose VIP the actor is gets read only for those held, in one read, and not
// at all when nobody is (CR-B48 P15): a VIP matters only to a hold.
func TestVIPsAreReadForTheHeldAlone(t *testing.T) {
	till := at.Add(time.Hour)
	ev := Event{Cause: CauseMessage, Actor: "me", Place: channel, Mentions: []string{"ann", "bo"}}

	nobody := &fakeInputs{levels: all("ann", "bo"), vips: map[string]bool{"ann": true}}
	planned(t, nobody, ev)
	if nobody.reads["vips"] != 0 {
		t.Errorf("nobody held, yet VIPs read %d times for %v", nobody.reads["vips"], nobody.asked["vips"])
	}

	one := &fakeInputs{levels: all("ann", "bo"), vips: map[string]bool{"ann": true},
		held: map[string]notificationBusiness.HeldStatus{"ann": {Until: &till, Reason: "paused"}}}
	got := planned(t, one, ev)
	if one.reads["vips"] != 1 || !reflect.DeepEqual(one.asked["vips"], []string{"ann"}) {
		t.Errorf("Ann alone held: VIPs read %d times for %v, want once for [ann]", one.reads["vips"], one.asked["vips"])
	}
	if !reflect.DeepEqual(got["ann"].To, named) {
		t.Errorf("Ann (held, the actor her VIP) told by %v, want %v", got["ann"].To, named)
	}
}

// A plan never reaches more than its write did (Event.Within): someone a
// team's mention or a broadcast reaches now whom the write's mention edges
// don't hold (they joined the team, or came online for an @here, since) is
// told nothing, no row and no push. With no write to bound it (nil, a
// thread's reply), the reach is as read.
func TestAPlanReachesNoMoreThanItsWrite(t *testing.T) {
	for _, c := range []struct {
		name   string
		within []string
		told   map[string]bool
	}{
		{"within the write", []string{"ann"}, map[string]bool{"ann": true}},
		{"no write to bound it", nil, map[string]bool{"ann": true, "bo": true, "cy": true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := &fakeInputs{levels: map[string]string{"ann": "mention", "bo": "mention", "cy": "mention"},
				teams: []string{"ann", "bo"}, broadcast: []string{"ann", "cy"}}
			got := planned(t, in, Event{Cause: CauseMessage, Actor: "me", Place: channel, Teams: []string{"design"}, Broadcast: "here", Within: c.within})
			for _, id := range []string{"ann", "bo", "cy"} {
				if d, told := got[id]; told != c.told[id] {
					t.Errorf("%s told %v (%+v), want %v", id, told, d, c.told[id])
				}
			}
		})
	}
}
