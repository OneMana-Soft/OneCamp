package business

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	deliveryModels "github.com/akashc777/OneCamp/models/postgres/NotificationDelivery"
)

// sentPush is one push call: who, the push's data, the notice's id.
type sentPush struct {
	to   []string
	data map[string]string
	id   string
}

// testWorld is the router on fakes for Test: its rows, every push, and whose
// devices were asked about.
type testWorld struct {
	rows  *fakeRows
	mu    sync.Mutex
	sent  []sentPush
	asked []string
}

func (w *testWorld) push(to []string, data map[string]string, id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sent = append(w.sent, sentPush{to: slices.Clone(to), data: maps.Clone(data), id: id})
}

func withTestWorld(t *testing.T, in *fakeInputs) *testWorld {
	t.Helper()
	rows, _ := withFakes(t)
	w := &testWorld{rows: rows}
	oldNotices, oldDeliver, oldThrough, oldDevices, oldApps := Notices, deliver, deliverThrough, pushDevices, appsOpen
	Notices = routerOn(in)
	deliver = func(_ context.Context, to []string, data map[string]string, id string) { w.push(to, data, id) }
	deliverThrough = func(_ context.Context, to []string, data map[string]string, id string) error {
		w.push(to, data, id)
		return nil
	}
	pushDevices = func(_ context.Context, id string) (int, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.asked = append(w.asked, id)
		return 2, nil
	}
	appsOpen = func(_ context.Context, id string) (int, bool) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.asked = append(w.asked, id)
		return 1, true
	}
	t.Cleanup(func() {
		Notices, deliver, deliverThrough, pushDevices, appsOpen = oldNotices, oldDeliver, oldThrough, oldDevices, oldApps
	})
	return w
}

func verdicts(steps []Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Step+" "+s.Verdict)
	}
	return out
}

// A test notice goes to the one asking alone, by push and desktop notice,
// with a title and never a body; only their rows are written, only their
// devices and hold are read, and the answer says each step.
func TestATestNoticeReachesOnlyTheOneAskingAndCarriesNoBody(t *testing.T) {
	in := &fakeInputs{}
	w := withTestWorld(t, in)

	res, err := Test(context.Background(), annID, at.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.sent) != 1 || !slices.Equal(w.sent[0].to, []string{annID}) {
		t.Fatalf("the test was pushed to %v; want the one asking alone", w.sent)
	}
	data := w.sent[0].data
	if _, has := data[firebaseInit.FIREBASE_PUSH_DATA_BODY]; has {
		t.Errorf("the test's push carries a body: %v", data)
	}
	if data[firebaseInit.FIREBASE_PUSH_DATA_TITLE] == "" || data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] != firebaseInit.FIREBASE_PUSH_DATA_TYPE_TEST {
		t.Errorf("the test's push is %v; want a title and the test's type", data)
	}
	if len(w.rows.rows) == 0 {
		t.Fatal("the test wrote no delivery rows: nothing would stop a second one in the minute")
	}
	for _, r := range w.rows.rows {
		if key(r.UserID) != key(annID) || r.Cause != string(CauseTest) {
			t.Errorf("the test wrote a row %+v; want the one asking's alone", r)
		}
	}
	if got := in.asked["held"]; !slices.Equal(got, []string{annID}) {
		t.Errorf("the test read the hold of %v; want the one asking's alone", got)
	}
	for _, id := range w.asked {
		if key(id) != key(annID) {
			t.Errorf("the test read the devices of %s", id)
		}
	}
	if !slices.Equal(res.Told, []Delivery{ToPush, ToDesktop}) {
		t.Errorf("the test told %v; want push and desktop", res.Told)
	}
	if want := []string{"audience site", "hold not_held"}; !slices.Equal(verdicts(res.Steps), want) {
		t.Errorf("the test's steps are %v; want %v", verdicts(res.Steps), want)
	}
	if res.UserID != annID || res.Decision == nil || res.Decision.UserID != annID {
		t.Errorf("the test answers for %q (decision %+v); want the one asking", res.UserID, res.Decision)
	}
	if res.Devices != (Devices{Push: 2, PushKnown: true, Apps: 1, AppsKnown: true}) {
		t.Errorf("devices are %+v; want 2 with push and 1 app open", res.Devices)
	}
}

// One test a minute: a second within the minute is refused with when the next
// may go, tells nobody and writes nothing; the next minute's goes.
func TestASecondTestWithinTheMinuteIsRefusedWithItsRetryTime(t *testing.T) {
	w := withTestWorld(t, &fakeInputs{})
	ctx := context.Background()
	if _, err := Test(ctx, annID, at.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	written := len(w.rows.rows)

	_, err := Test(ctx, annID, at.Add(50*time.Second))
	var soon *TooSoon
	if !errors.As(err, &soon) {
		t.Fatalf("a second test in the minute answered %v; want TooSoon", err)
	}
	if want := at.Add(time.Minute); !soon.RetryAt.Equal(want) {
		t.Errorf("the retry time is %s; want %s, the next minute", soon.RetryAt, want)
	}
	if len(w.sent) != 1 || len(w.rows.rows) != written {
		t.Errorf("the refused test pushed (%d pushes) or wrote rows (%d, was %d)", len(w.sent), len(w.rows.rows), written)
	}

	if _, err := Test(ctx, annID, at.Add(65*time.Second)); err != nil {
		t.Errorf("the next minute's test answered %v; want it sent", err)
	}
	if len(w.sent) != 2 {
		t.Errorf("the next minute's test made %d pushes in all; want 2", len(w.sent))
	}
	// Another person's test in the same minute is theirs: never refused for
	// the first one's.
	if _, err := Test(ctx, boID, at.Add(55*time.Second)); err != nil {
		t.Errorf("another person's test in the same minute answered %v", err)
	}
}

// A test held by the person's schedule says so and until when: the push and
// desktop notice wait (recorded held), nothing is pushed, and it isn't
// refused as too soon.
func TestATestHeldByYourScheduleSaysUntilWhen(t *testing.T) {
	nine := at.Add(10 * time.Hour)
	in := &fakeInputs{held: map[string]notificationBusiness.HeldStatus{key(annID): {Until: &nine, Reason: "quiet_hours"}}}
	w := withTestWorld(t, in)

	res, err := Test(context.Background(), annID, at)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"audience site", "hold held:quiet_hours"}; !slices.Equal(verdicts(res.Steps), want) {
		t.Errorf("the steps are %v; want %v", verdicts(res.Steps), want)
	}
	d := res.Decision
	if d == nil || !slices.Equal(d.Held, []Delivery{ToPush, ToDesktop}) || d.HoldTill == nil || !d.HoldTill.Equal(nine) || len(d.To) != 0 {
		t.Fatalf("the decision is %+v; want push and desktop held until %s", d, nine)
	}
	if len(w.sent) != 0 || len(res.Told) != 0 {
		t.Errorf("a held test pushed %v and told %v; want nothing now", w.sent, res.Told)
	}
	// The web reads told's length: nothing told is an empty list, never null.
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if got := string(fields["told"]); got != "[]" {
		t.Errorf("a held test answers \"told\":%s; want []", got)
	}
	for _, r := range w.rows.rows {
		if r.State != "held" || r.HoldUntil == nil || !r.HoldUntil.Equal(nine) {
			t.Errorf("row %+v; want held until %s", r, nine)
		}
	}
}

// rowsWorld swaps the record for rows, and notes whose rows of which message
// were asked for.
func rowsWorld(t *testing.T, rows []deliveryModels.Row) *[][2]string {
	t.Helper()
	var asked [][2]string
	old := rowsOf
	rowsOf = func(_ context.Context, user, message string) ([]deliveryModels.Row, error) {
		asked = append(asked, [2]string{user, message})
		return rows, nil
	}
	t.Cleanup(func() { rowsOf = old })
	return &asked
}

var (
	nineAM  = at.Add(10 * time.Hour)
	mention = "channel:ch:" + msgID
	reacted = "chat:dm:" + msgID + ":" + boID
)

// Why is the asker's own record of a message, read for them alone: a row of
// anyone else's, even one the read let through, is never answered.
func TestWhyIsTheAskersOwnRecordAlone(t *testing.T) {
	asked := rowsWorld(t, []deliveryModels.Row{
		{EventID: mention, UserID: annID, Delivery: "inbox", Cause: string(CauseMention), State: "sent", CreatedAt: at},
		{EventID: mention, UserID: annID, Delivery: "push", Cause: string(CauseMention), State: "held", HoldUntil: &nineAM, CreatedAt: at},
		{EventID: mention, UserID: annID, Delivery: "desktop", Cause: string(CauseMention), State: "held", HoldUntil: &nineAM, CreatedAt: at},
		{EventID: mention, UserID: boID, Delivery: "push", Cause: string(CauseMention), State: "sent", CreatedAt: at},
	})

	got, err := Why(context.Background(), annID, msgID)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][2]string{{annID, msgID}}; !slices.Equal(*asked, want) {
		t.Errorf("Why read the rows of %v; want the asker's of the message", *asked)
	}
	if len(got) != 1 {
		t.Fatalf("Why answered %d events; want 1", len(got))
	}
	tr := got[0]
	if tr.UserID != annID || tr.EventID != mention {
		t.Errorf("the trace is for %q about %q", tr.UserID, tr.EventID)
	}
	want := []string{"audience in", "access in_place", "cause chosen", "preference kept", "hold held"}
	if !slices.Equal(verdicts(tr.Steps), want) {
		t.Errorf("steps are %v; want %v", verdicts(tr.Steps), want)
	}
	if !slices.Equal(tr.Steps[0].Causes, []Cause{CauseMention}) {
		t.Errorf("the audience step found %v; want the mention", tr.Steps[0].Causes)
	}
	d := tr.Decision
	if d == nil || d.UserID != annID || d.Cause != CauseMention || !slices.Equal(d.To, []Delivery{ToInbox}) ||
		!slices.Equal(d.Held, []Delivery{ToPush, ToDesktop}) || d.HoldTill == nil || !d.HoldTill.Equal(nineAM) {
		t.Errorf("the decision is %+v; want the inbox now, push and desktop held until 09:00", d)
	}
	if len(tr.Deliveries) != 3 {
		t.Fatalf("deliveries are %+v; want the asker's three", tr.Deliveries)
	}
	for _, s := range tr.Deliveries {
		if s.Delivery == ToPush && (s.State != "held" || s.HoldUntil == nil) {
			t.Errorf("the push is %+v; want held until 09:00 (bo's sent push is not the asker's)", s)
		}
	}
}

// A message that told the asker nothing answers ErrNoRecord: one of someone
// else's, one with no rows, or an id that is no message (never read).
func TestWhyOfAMessageThatToldYouNothingIsNoRecord(t *testing.T) {
	asked := rowsWorld(t, []deliveryModels.Row{{EventID: mention, UserID: boID, Delivery: "push", Cause: string(CauseMention), State: "sent", CreatedAt: at}})
	ctx := context.Background()
	if _, err := Why(ctx, annID, msgID); !errors.Is(err, ErrNoRecord) {
		t.Errorf("another person's record answered %v; want ErrNoRecord", err)
	}
	if _, err := Why(ctx, annID, "not-a-message"); !errors.Is(err, ErrNoRecord) {
		t.Errorf("an id that is no message answered %v; want ErrNoRecord", err)
	}
	if _, err := Why(ctx, "", msgID); err == nil {
		t.Error("nobody asking answered a record")
	}
	if len(*asked) != 1 {
		t.Errorf("Why read %v; want only the one well-formed question", *asked)
	}
}

// Each event about the message is its own trace, newest first, saying what
// was held, dropped (held, then deleted) or retracted (sent, then deleted).
func TestWhyTellsEachEventApartNewestFirst(t *testing.T) {
	later := at.Add(time.Hour)
	rowsWorld(t, []deliveryModels.Row{
		{EventID: reacted, UserID: annID, Delivery: "inbox", Cause: string(CauseReaction), State: "retracted", CreatedAt: later},
		{EventID: mention, UserID: annID, Delivery: "push", Cause: string(CauseMention), State: "retracted", CreatedAt: at},
		{EventID: mention, UserID: annID, Delivery: "email", Cause: string(CauseMention), State: "dropped", HoldUntil: &nineAM, CreatedAt: at},
	})
	got, err := Why(context.Background(), annID, msgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].EventID != reacted || got[1].EventID != mention {
		t.Fatalf("Why answered %+v; want the reaction, then the mention", got)
	}
	if v := verdicts(got[0].Steps); v[len(v)-1] != "hold nothing_to_hold" {
		t.Errorf("a reaction's hold step is %v; want nothing_to_hold (the inbox alone)", v)
	}
	m := got[1]
	if v := verdicts(m.Steps); v[len(v)-1] != "hold held" {
		t.Errorf("the mention's hold step is %v; want held", v)
	}
	states := map[Delivery]string{}
	for _, s := range m.Deliveries {
		states[s.Delivery] = s.State
	}
	if states[ToPush] != "retracted" || states[ToEmail] != "dropped" {
		t.Errorf("the mention's deliveries are %v; want push retracted, email dropped", states)
	}
	if !slices.Equal(m.Decision.To, []Delivery{ToPush}) || !slices.Equal(m.Decision.Held, []Delivery{ToEmail}) {
		t.Errorf("the mention's decision is %+v; want push told, email held", m.Decision)
	}
}

// The commonest Why: a push that went. Its hold step says it wasn't held,
// never that there was nothing to hold.
func TestWhyOfASentPushSaysItWasNotHeld(t *testing.T) {
	rowsWorld(t, []deliveryModels.Row{
		{EventID: mention, UserID: annID, Delivery: "inbox", Cause: string(CauseMention), State: "sent", CreatedAt: at},
		{EventID: mention, UserID: annID, Delivery: "push", Cause: string(CauseMention), State: "sent", CreatedAt: at},
	})
	got, err := Why(context.Background(), annID, msgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("Why answered %+v; want the mention alone", got)
	}
	if v := verdicts(got[0].Steps); v[len(v)-1] != "hold not_held" {
		t.Errorf("a sent push's hold step is %v; want not_held", v)
	}
}

// jsonKeys is every key anywhere in v's JSON.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch x := n.(type) {
		case map[string]any:
			for k, c := range x {
				seen[k] = true
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(tree)
	keys := slices.Collect(maps.Keys(seen))
	sort.Strings(keys)
	return keys
}

// Both answers carry ids and verdicts alone: never a body, a title or the
// push's data, whatever else is added.
func TestTheAnswersCarryIdsAndVerdictsAlone(t *testing.T) {
	allowed := map[string]bool{
		"user_id": true, "event_id": true, "steps": true, "step": true, "verdict": true, "causes": true,
		"decision": true, "cause": true, "to": true, "held": true, "hold_till": true, "note": true,
		"deliveries": true, "delivery": true, "state": true, "hold_until": true, "at": true,
		"devices": true, "push_devices": true, "push_known": true, "apps_open": true, "apps_known": true, "told": true,
	}
	nine := nineAM
	withTestWorld(t, &fakeInputs{held: map[string]notificationBusiness.HeldStatus{key(annID): {Until: &nine, Reason: "focus"}}})
	res, err := Test(context.Background(), annID, at)
	if err != nil {
		t.Fatal(err)
	}
	rowsWorld(t, []deliveryModels.Row{
		{EventID: mention, UserID: annID, Delivery: "push", Cause: string(CauseMention), State: "held", HoldUntil: &nineAM, CreatedAt: at},
		{EventID: mention, UserID: annID, Delivery: "inbox", Cause: string(CauseMention), State: "sent", CreatedAt: at},
	})
	why, err := Why(context.Background(), annID, msgID)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"the test's answer": res, "why's answer": why} {
		keys := jsonKeys(t, v)
		if len(keys) < 5 {
			t.Errorf("%s has keys %v; want its steps and decision", name, keys)
		}
		for _, k := range keys {
			if !allowed[k] {
				t.Errorf("%s carries %q: only ids and verdicts may go out", name, k)
			}
		}
	}
}
