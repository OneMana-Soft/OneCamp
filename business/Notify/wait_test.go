package business

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	deliveryModels "github.com/akashc777/OneCamp/models/postgres/NotificationDelivery"
)

// The phone waits while you're in OneCamp (A4 H4): a chat notice's push to
// someone in OneCamp (Presence Here) waits their push delay, while the
// desktop notice, inbox, badge and sound go now; a hold beats the wait, and
// Notify anyway, a delay of 0, someone not in OneCamp, a thread reply (no read
// marker) and a site's event push now, as today.

func TestAPushWaitsWhileTheyreInOneCamp(t *testing.T) {
	here := map[string]Wait{"ann": {Here: true, Delay: time.Minute}}
	till := at.Add(time.Minute)
	heldTill := at.Add(9 * time.Hour)
	heldAnn := map[string]notificationBusiness.HeldStatus{"ann": {Until: &heldTill, Reason: "paused"}}
	mention := Event{Cause: CauseMessage, Actor: "me", Place: channel, Mentions: []string{"ann"}}
	dm := Event{Cause: CauseMessage, Actor: "me", Place: Place{Kind: PlaceDM, ID: "a b"}, Audience: []string{"me", "ann"}}
	nowNamed := []Delivery{ToInbox, ToBadge, ToDesktop, ToSound, ToEmail}
	cases := []struct {
		name string
		in   *fakeInputs
		ev   Event
		want Decision
	}{
		{"in OneCamp, a minute: the push waits, the rest goes now", &fakeInputs{levels: all("ann"), waits: here}, mention,
			Decision{UserID: "ann", Cause: CauseMention, To: nowNamed, Waiting: []Delivery{ToPush}, WaitTill: &till}},
		{"five minutes wait five", &fakeInputs{levels: all("ann"), waits: map[string]Wait{"ann": {Here: true, Delay: 5 * time.Minute}}}, mention,
			Decision{UserID: "ann", Cause: CauseMention, To: nowNamed, Waiting: []Delivery{ToPush}, WaitTill: ptr(at.Add(5 * time.Minute))}},
		{"right away: the push goes now", &fakeInputs{levels: all("ann"), waits: map[string]Wait{"ann": {Here: true}}}, mention,
			Decision{UserID: "ann", Cause: CauseMention, To: named}},
		{"not in OneCamp: the push goes now", &fakeInputs{levels: all("ann"), waits: map[string]Wait{"ann": {Delay: time.Minute}}}, mention,
			Decision{UserID: "ann", Cause: CauseMention, To: named}},
		{"held: held as today, nothing waits", &fakeInputs{levels: all("ann"), waits: here, held: heldAnn}, mention,
			Decision{UserID: "ann", Cause: CauseMention, To: []Delivery{ToInbox, ToBadge}, Held: []Delivery{ToPush, ToDesktop, ToSound, ToEmail}, HoldTill: &heldTill}},
		{"Notify anyway: the push goes now", &fakeInputs{levels: all("ann"), waits: here},
			Event{Cause: CauseMessage, Actor: "me", Place: channel, Mentions: []string{"ann"}, Anyway: []string{"Ann"}},
			Decision{UserID: "ann", Cause: CauseMention, To: named}},
		{"a thread reply has no read marker: the push goes now", &fakeInputs{levels: all("ann"), waits: here},
			Event{Cause: CauseMessage, Actor: "me", Place: inThread, Mentions: []string{"ann"}},
			Decision{UserID: "ann", Cause: CauseMention, To: named}},
		{"a DM waits too", &fakeInputs{levels: all("ann"), waits: here}, dm,
			Decision{UserID: "ann", Cause: CauseDM, To: nowNamed, Waiting: []Delivery{ToPush}, WaitTill: &till}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planned(t, c.in, c.ev)
			if !reflect.DeepEqual(got["ann"], c.want) {
				t.Errorf("got %+v\nwant %+v", got["ann"], c.want)
			}
		})
	}
}

func ptr(t time.Time) *time.Time { return &t }

// A site's event is planned as today: its push goes now, and nobody's
// presence is read for it.
func TestASitesEventNeverWaits(t *testing.T) {
	in := &fakeInputs{waits: map[string]Wait{"ann": {Here: true, Delay: time.Minute}}}
	got := planned(t, in, Event{Cause: CauseCalendarInvited, Actor: "me", Audience: []string{"ann"}})
	if d := got["ann"]; !slices.Contains(d.To, ToPush) || len(d.Waiting) != 0 {
		t.Errorf("ann %+v: want the push now", d)
	}
	if in.reads["waits"] != 0 {
		t.Errorf("a site's event read who is in OneCamp %d times", in.reads["waits"])
	}
}

// Who is in OneCamp and their delays are one read for the whole event, of
// those whose push could wait alone: never the held, never a read a person.
func TestWhoWaitsIsOneRead(t *testing.T) {
	till := at.Add(time.Hour)
	in := &fakeInputs{levels: all("ann", "bea", "cy"),
		held:  map[string]notificationBusiness.HeldStatus{"cy": {Until: &till, Reason: "focus"}},
		waits: map[string]Wait{"ann": {Here: true, Delay: time.Minute}, "bea": {Here: true, Delay: 10 * time.Minute}}}
	got := planned(t, in, Event{Cause: CauseMessage, Actor: "me", Place: channel, Mentions: []string{"ann", "bea", "cy"}})
	if in.reads["waits"] != 1 {
		t.Fatalf("waits read %d times, want once", in.reads["waits"])
	}
	asked := slices.Clone(in.asked["waits"])
	slices.Sort(asked)
	if !slices.Equal(asked, []string{"ann", "bea"}) {
		t.Errorf("asked about %v, want ann and bea (cy is held)", asked)
	}
	if w := got["bea"].WaitTill; w == nil || !w.Equal(at.Add(10*time.Minute)) {
		t.Errorf("bea waits till %v, want ten minutes on", w)
	}
}

// Deliver records the waiting push as a 'waiting' row until WaitTill, sends
// the desktop half now to a waiter and both halves to everyone else, and keeps
// the push's payload for the Sweeper.
func TestDeliverRecordsAWaitAndSendsTheDesktopHalfNow(t *testing.T) {
	rows := &fakeRows{}
	got := &halves{}
	kept := &fakePayloads{}
	swapDeliverySeams(t, rows, got, kept)
	till := at.Add(time.Minute)
	ev := Event{ID: "channel:ch:m1", MessageID: "5b1e7c4e-8a8e-4f43-9d0e-111111111111", Place: channel, Push: body}
	ds := []Decision{
		{UserID: annID, Cause: CauseMention, To: []Delivery{ToInbox, ToDesktop}, Waiting: []Delivery{ToPush}, WaitTill: &till},
		{UserID: boID, Cause: CauseMention, To: []Delivery{ToInbox, ToPush, ToDesktop}},
	}
	routerOn(&fakeInputs{}).Deliver(context.Background(), ev, ds)
	r := rows.rows[ev.ID+"|"+annID+"|push"]
	if r.State != "waiting" || r.HoldUntil == nil || !r.HoldUntil.Equal(till) || r.SentAt != nil {
		t.Errorf("ann's push row %+v, want waiting until %v", r, till)
	}
	if d := rows.rows[ev.ID+"|"+annID+"|desktop"]; d.State != "sent" {
		t.Errorf("ann's desktop row %+v, want sent", d)
	}
	if !slices.Equal(got.desktop, []string{annID}) || !slices.Equal(got.both, []string{boID}) {
		t.Errorf("desktop half to %v and both to %v; want ann alone the desktop half, bea both", got.desktop, got.both)
	}
	if len(kept.keys) != 1 || kept.keys[0] != "notify:wait:"+ev.ID+":"+annID {
		t.Errorf("payloads kept %v, want ann's alone", kept.keys)
	}
}

// Every waiting push of an event keeps its payload in one write, whoever
// waits; a Retract that dropped one's row between the row and its payload
// (the message deleted at that moment) has that payload forgotten at once,
// and the others kept (CHAT70's decision 7).
func TestAPayloadKeptAfterItsRetractIsForgotten(t *testing.T) {
	rows := &fakeRows{}
	got := &halves{}
	kept := &fakePayloads{gone: []string{annID}}
	swapDeliverySeams(t, rows, got, kept)
	till := at.Add(time.Minute)
	ev := Event{ID: "channel:ch:m5", MessageID: "5b1e7c4e-8a8e-4f43-9d0e-555555555555", Place: channel, Push: body}
	routerOn(&fakeInputs{}).Deliver(context.Background(), ev, []Decision{
		{UserID: annID, Cause: CauseMention, To: []Delivery{ToInbox, ToDesktop}, Waiting: []Delivery{ToPush}, WaitTill: &till},
		{UserID: boID, Cause: CauseMention, To: []Delivery{ToInbox, ToDesktop}, Waiting: []Delivery{ToPush}, WaitTill: &till},
	})
	if kept.calls != 1 || len(kept.keys) != 2 {
		t.Errorf("payloads kept %v in %d writes, want ann's and bo's in one", kept.keys, kept.calls)
	}
	if !slices.Equal(kept.forgot, []string{"notify:wait:" + ev.ID + ":" + annID}) {
		t.Errorf("forgot %v, want ann's payload alone (her row was dropped before it was kept)", kept.forgot)
	}
}

// When the payload can't be kept (Redis blinks or isn't there), the push
// goes now through the one path and its row is marked sent: nothing is lost.
func TestAPayloadThatCantBeKeptPushesNow(t *testing.T) {
	rows := &fakeRows{}
	got := &halves{}
	kept := &fakePayloads{fail: true}
	swapDeliverySeams(t, rows, got, kept)
	till := at.Add(time.Minute)
	ev := Event{ID: "channel:ch:m2", Place: channel, Push: body}
	routerOn(&fakeInputs{}).Deliver(context.Background(), ev, []Decision{
		{UserID: annID, Cause: CauseMention, To: []Delivery{ToInbox, ToDesktop}, Waiting: []Delivery{ToPush}, WaitTill: &till},
	})
	if !slices.Equal(got.both, []string{annID}) || len(got.desktop) != 0 {
		t.Errorf("both halves to %v, desktop alone to %v; want ann pushed now", got.both, got.desktop)
	}
	if !slices.Equal(rows.settled, []string{annID + " push waiting>sent"}) {
		t.Errorf("settled %v, want ann's push marked sent", rows.settled)
	}
}

// Retract drops a waiting push and forgets its payload, as it drops a held
// one; the people whose rows it took back are told once each.
func TestRetractForgetsAWaitingPushsPayload(t *testing.T) {
	rows := &fakeRows{told: []string{boID}, dropped: []deliveryModels.Row{
		{EventID: "channel:ch:m3", UserID: annID, Delivery: "push", State: "dropped"},
		{EventID: "channel:ch:m3", UserID: annID, Delivery: "desktop", State: "retracted"},
	}}
	var forgot []string
	oldRetract, oldNotices, oldForget := retractRows, retractNotices, forgetPayloads
	t.Cleanup(func() { retractRows, retractNotices, forgetPayloads = oldRetract, oldNotices, oldForget })
	retractRows, retractNotices = rows.retract, rows.retractNotices
	forgetPayloads = func(_ context.Context, keys ...string) { forgot = append(forgot, keys...) }
	routerOn(&fakeInputs{}).Retract(context.Background(), "5b1e7c4e-8a8e-4f43-9d0e-333333333333")
	if !slices.Equal(forgot, []string{"notify:wait:channel:ch:m3:" + annID}) {
		t.Errorf("forgot %v, want ann's waiting push's payload", forgot)
	}
	if len(rows.notices) != 1 || rows.notices[0] != "5b1e7c4e-8a8e-4f43-9d0e-333333333333 "+boID+","+annID {
		t.Errorf("notices taken back %v, want bo's and ann's once each", rows.notices)
	}
}

// fakePayloads is the payload store.
type fakePayloads struct {
	keys []string
	// ttls is how long each kept payload lasts, in keys' order.
	ttls []time.Duration
	fail bool
	// calls is how many times payloads were kept: once an event.
	calls int
	// gone are the people whose push a Retract dropped before its payload
	// was kept; forgot is what was forgotten.
	gone, forgot []string
}

// halves is who each half was sent to: both, the desktop alone.
type halves struct{ both, desktop []string }

// swapDeliverySeams puts fakes in Deliver's seams for the test.
func swapDeliverySeams(t *testing.T, rows *fakeRows, got *halves, kept *fakePayloads) {
	t.Helper()
	oldRecord, oldThrough, oldHalf, oldKeep, oldSettle := recordRows, deliverThrough, deliverHalf, keepPayloads, settleRows
	oldStill, oldForget := stillWaiting, forgetPayloads
	t.Cleanup(func() {
		recordRows, deliverThrough, deliverHalf, keepPayloads, settleRows = oldRecord, oldThrough, oldHalf, oldKeep, oldSettle
		stillWaiting, forgetPayloads = oldStill, oldForget
	})
	recordRows = rows.record
	settleRows = rows.settle
	deliverThrough = func(_ context.Context, ids []string, _ map[string]string, _ string) error {
		got.both = append(got.both, ids...)
		return nil
	}
	deliverHalf = func(_ context.Context, half notificationBusiness.Half, ids []string, _ map[string]string, _ string) error {
		if half == notificationBusiness.DesktopHalf {
			got.desktop = append(got.desktop, ids...)
		}
		return nil
	}
	keepPayloads = func(_ context.Context, ps []keptPayload) error {
		kept.calls++
		if kept.fail {
			return errNoPayloadStore
		}
		for _, p := range ps {
			kept.keys, kept.ttls = append(kept.keys, p.key), append(kept.ttls, p.ttl)
		}
		return nil
	}
	stillWaiting = func(_ context.Context, eventID string) ([]string, error) {
		var still []string
		for k, r := range rows.rows {
			if strings.HasPrefix(k, eventID+"|") && r.Delivery == "push" && r.State == "waiting" && !slices.Contains(kept.gone, r.UserID) {
				still = append(still, key(r.UserID))
			}
		}
		return still, nil
	}
	forgetPayloads = func(_ context.Context, keys ...string) { kept.forgot = append(kept.forgot, keys...) }
}

// Why reads a waiting push as waiting until when, because they're in
// OneCamp, and one the wait then dropped (read first) as waited, never as
// held; one sent when its wait ended as sent, and waited first; a held one
// stays held.
func TestWhyTellsAWaitFromAHold(t *testing.T) {
	till := at.Add(time.Minute)
	ev := "channel:ch:m4"
	row := func(d, state string, hold *time.Time) deliveryModels.Row {
		return deliveryModels.Row{EventID: ev, UserID: annID, Delivery: d, Cause: string(CauseMention), State: state, HoldUntil: hold, CreatedAt: at}
	}
	for _, c := range []struct {
		name     string
		push     string
		wantWait bool
	}{{"waiting", "waiting", true}, {"read first", "dropped", true}} {
		tr := recalled(annID, ev, []deliveryModels.Row{row("push", c.push, &till), row("desktop", "sent", nil), row("inbox", "sent", nil)})
		if !slices.Equal(tr.Decision.Waiting, []Delivery{ToPush}) || tr.Decision.WaitTill == nil || !tr.Decision.WaitTill.Equal(till) || len(tr.Decision.Held) != 0 {
			t.Errorf("%s: decision %+v, want the push waiting till %v", c.name, tr.Decision, till)
		}
		if !slices.ContainsFunc(tr.Steps, func(s Step) bool { return s.Step == "wait" && s.Verdict == "in_onecamp" }) {
			t.Errorf("%s: steps %+v, want the wait's", c.name, tr.Steps)
		}
	}
	sent := recalled(annID, ev, []deliveryModels.Row{row("push", "sent", &till), row("desktop", "sent", nil), row("inbox", "sent", nil)})
	if !slices.Contains(sent.Decision.To, ToPush) || len(sent.Decision.Waiting) != 0 || len(sent.Decision.Held) != 0 ||
		!slices.ContainsFunc(sent.Steps, func(s Step) bool { return s.Step == "wait" && s.Verdict == "in_onecamp" }) {
		t.Errorf("a push sent after its wait %+v %+v, want sent, with the wait's step", sent.Decision, sent.Steps)
	}
	held := recalled(annID, ev, []deliveryModels.Row{row("push", "dropped", &till), row("desktop", "dropped", &till), row("inbox", "sent", nil)})
	if len(held.Decision.Waiting) != 0 || !slices.Contains(held.Decision.Held, ToPush) || slices.ContainsFunc(held.Steps, func(s Step) bool { return s.Step == "wait" }) {
		t.Errorf("a held then dropped push %+v %+v, want held, no wait", held.Decision, held.Steps)
	}
}

// A pass sends each waiting push to its own person alone, the push half
// alone (the desktop notice went when it was written): two people wait on
// two events, and one read the first before it was due.
func TestAPassSendsEachWaitingPushToItsOwnPersonAlone(t *testing.T) {
	p := waitingOn(annID, boID)
	p.read = map[string]*time.Time{annID + "|chA": ptr(at.Add(time.Second))}
	rows := &fakeRows{}
	passOn(t, rows, p)
	want := []sentHalf{{"e1", notificationBusiness.PushHalf, []string{boID}}, {"e2", notificationBusiness.PushHalf, []string{annID, boID}}}
	if !reflect.DeepEqual(p.sent, want) {
		t.Errorf("sent %+v\nwant %+v", p.sent, want)
	}
	if !slices.Equal(rows.settled, []string{annID + " push sent>dropped"}) {
		t.Errorf("settled %v, want ann's first push dropped (she read it)", rows.settled)
	}
	if len(p.forgot) != 4 {
		t.Errorf("forgot %v, want every payload the pass took", p.forgot)
	}
}

// A pass drops a waiting push whose person can't have it now: a hold began
// during the wait, they may no longer read the conversation (left it, or were
// taken out), or its payload is gone. The others of the pass go.
func TestAPassDropsAPushItsPersonCantHaveNow(t *testing.T) {
	till := at.Add(time.Hour)
	for _, c := range []struct {
		name string
		set  func(p *fakePass)
	}{
		{"a hold began during the wait", func(p *fakePass) {
			p.held = map[string]notificationBusiness.HeldStatus{boID: {Until: &till, Reason: "paused"}}
		}},
		{"they may no longer read it", func(p *fakePass) { p.gone = map[string]bool{boID + "|chA": true} }},
		{"its payload is gone", func(p *fakePass) { delete(p.payloads, waitKey("e1", boID)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := waitingOn(annID, boID)
			p.claimed = p.claimed[:2] // e1's alone
			c.set(p)
			rows := &fakeRows{}
			passOn(t, rows, p)
			if want := []sentHalf{{"e1", notificationBusiness.PushHalf, []string{annID}}}; !reflect.DeepEqual(p.sent, want) {
				t.Errorf("sent %+v, want ann's alone", p.sent)
			}
			if !slices.Equal(rows.settled, []string{boID + " push sent>dropped"}) {
				t.Errorf("settled %v, want bo's push dropped", rows.settled)
			}
		})
	}
}

// fakePass is a pass's seams: the push rows it claims, their payloads (by
// key; one missing is gone), its people's holds, their read markers and
// where they may no longer read (by "<person>|<place id>"), and what it sent
// and forgot.
type fakePass struct {
	claimed      []deliveryModels.Row
	payloads     map[string]waitingPush
	payloadsFail bool
	held         map[string]notificationBusiness.HeldStatus
	read         map[string]*time.Time
	gone         map[string]bool
	sent         []sentHalf
	forgot       []string
}

// sentHalf is one send of a pass: the event, the half and to whom.
type sentHalf struct {
	event string
	half  notificationBusiness.Half
	to    []string
}

// waitingOn is a pass that claims people's waiting pushes of two events, e1
// in #chA and e2 in #chB, each told at `at`, in that order.
func waitingOn(people ...string) *fakePass {
	p := &fakePass{payloads: map[string]waitingPush{}}
	for _, e := range []struct{ id, place string }{{"e1", "chA"}, {"e2", "chB"}} {
		for _, u := range people {
			p.claimed = append(p.claimed, deliveryModels.Row{EventID: e.id, UserID: u, Delivery: "push", State: "sent"})
			p.payloads[waitKey(e.id, u)] = waitingPush{Push: body, Place: Place{Kind: PlaceChannel, ID: e.place}, At: at}
		}
	}
	return p
}

// passOn runs one pass of notify.push.wait on p's seams and rows.
func passOn(t *testing.T, rows *fakeRows, p *fakePass) {
	t.Helper()
	oldClaim, oldPayloads, oldHeld, oldRead := claimDue, payloadsOf, heldOf, readSince
	oldSettle, oldHalf, oldForget := settleRows, deliverHalf, forgetPayloads
	t.Cleanup(func() {
		claimDue, payloadsOf, heldOf, readSince = oldClaim, oldPayloads, oldHeld, oldRead
		settleRows, deliverHalf, forgetPayloads = oldSettle, oldHalf, oldForget
	})
	claimDue = func(context.Context, int) ([]deliveryModels.Row, error) { return p.claimed, nil }
	payloadsOf = func(_ context.Context, keys []string) ([][]byte, error) {
		if p.payloadsFail {
			return nil, errNoPayloadStore
		}
		out := make([][]byte, len(keys))
		for i, k := range keys {
			if w, ok := p.payloads[k]; ok {
				b, err := json.Marshal(w)
				if err != nil {
					t.Fatal(err)
				}
				out[i] = b
			}
		}
		return out, nil
	}
	heldOf = func(context.Context, []string, time.Time) (map[string]notificationBusiness.HeldStatus, error) {
		return p.held, nil
	}
	readSince = func(_ context.Context, userID string, where Place) (*time.Time, bool, error) {
		k := key(userID) + "|" + where.ID
		return p.read[k], !p.gone[k], nil
	}
	settleRows = rows.settle
	deliverHalf = func(_ context.Context, half notificationBusiness.Half, ids []string, _ map[string]string, eventID string) error {
		p.sent = append(p.sent, sentHalf{eventID, half, slices.Clone(ids)})
		return nil
	}
	forgetPayloads = func(_ context.Context, keys ...string) { p.forgot = append(p.forgot, keys...) }
	if err := sendWaitingPushes(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
}

// The pass is a sweep the server runs (notify.push.wait): the tests above
// call it themselves, so this is what proves a waiting push is ever sent.
func TestTheWaitingPushPassIsASweep(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Errorf("%s isn't a registered sweep: no waiting push would ever be sent", waitJob)
		}
	}()
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: waitJob, Every: waitEvery, Run: func(context.Context) error { return nil }})
}

// A push that can't wait in a row goes now through the one path, both halves
// at once and no payload kept: an event with no id (nothing recorded), rows
// that can't be written, or a recipient whose id isn't one. A wait never
// loses a push.
func TestAPushThatCantWaitInARowGoesNow(t *testing.T) {
	till := at.Add(time.Minute)
	for _, c := range []struct {
		name, user string
		ev         Event
		rows       *fakeRows
	}{
		{"an event with no id", annID, Event{Place: channel, Push: body}, &fakeRows{}},
		{"rows that can't be written", annID, Event{ID: "channel:ch:m6", Place: channel, Push: body}, &fakeRows{fail: errors.New("the rows are down")}},
		{"a recipient whose id isn't one", "ann", Event{ID: "channel:ch:m7", Place: channel, Push: body}, &fakeRows{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, kept := &halves{}, &fakePayloads{}
			swapDeliverySeams(t, c.rows, got, kept)
			told := routerOn(&fakeInputs{}).Deliver(context.Background(), c.ev, []Decision{
				{UserID: c.user, Cause: CauseMention, To: []Delivery{ToInbox, ToDesktop}, Waiting: []Delivery{ToPush}, WaitTill: &till},
			})
			if !slices.Equal(got.both, []string{c.user}) || len(got.desktop) != 0 || kept.calls != 0 {
				t.Errorf("both halves to %v, the desktop alone to %v, %d payload writes; want %s pushed now, nothing kept", got.both, got.desktop, kept.calls, c.user)
			}
			if len(told) != 1 || !slices.Contains(told[0].To, ToPush) || len(told[0].Waiting) != 0 {
				t.Errorf("told %+v, want the push now", told)
			}
		})
	}
}

// A pass that can't read its payloads (Redis blinks) puts the rows it
// claimed back to waiting for the next pass, which their TTL outlasts: it
// sends nothing and loses nothing.
func TestAPassThatCantReadItsPayloadsPutsItsRowsBack(t *testing.T) {
	p := waitingOn(annID)
	p.payloadsFail = true
	rows := &fakeRows{}
	passOn(t, rows, p)
	if back := annID + " push sent>waiting"; !slices.Equal(rows.settled, []string{back, back}) {
		t.Errorf("settled %v, want both of ann's claimed rows back to waiting", rows.settled)
	}
	if len(p.sent) != 0 || len(p.forgot) != 0 {
		t.Errorf("sent %+v and forgot %v, want nothing (the next pass sends them)", p.sent, p.forgot)
	}
}

// A waiting push's payload lasts its wait and ten minutes more, for a pass
// that comes late (MAIN 15:0x): a minute's wait keeps it eleven minutes, ten
// minutes' twenty. Never for good, and never the grace alone (a ten-minute
// wait's payload would end as its push fell due).
func TestAWaitingPayloadLastsItsWaitAndTenMinutes(t *testing.T) {
	kept := &fakePayloads{}
	swapDeliverySeams(t, &fakeRows{}, &halves{}, kept)
	ev := Event{ID: "channel:ch:m8", Place: channel, Push: body}
	routerOn(&fakeInputs{}).Deliver(context.Background(), ev, []Decision{
		{UserID: annID, Cause: CauseMention, To: []Delivery{ToInbox, ToDesktop}, Waiting: []Delivery{ToPush}, WaitTill: ptr(at.Add(time.Minute))},
		{UserID: boID, Cause: CauseMention, To: []Delivery{ToInbox, ToDesktop}, Waiting: []Delivery{ToPush}, WaitTill: ptr(at.Add(10 * time.Minute))},
	})
	if want := []time.Duration{11 * time.Minute, 20 * time.Minute}; !slices.Equal(kept.ttls, want) {
		t.Errorf("payloads last %v, want %v", kept.ttls, want)
	}
}
