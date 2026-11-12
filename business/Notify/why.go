package business

// "Why did or didn't I get this?" (A4 H3). Test sends the person asking a
// test notice through the router to their own devices and answers each
// step's verdict (where it can show, the hold, what was told); Why is their
// record of one message: for each event about it that reached them, the
// cause, each step's verdict and each delivery's state now.
//
// Permission model: any member, guests included, may test their own devices
// and ask about their own record. Only the asker is planned for, told, or
// read about: a test's audience is the asker alone, and Why reads only the
// asker's rows, so a message that told them nothing (someone else's, or none
// at all) answers ErrNoRecord, the same either way. Neither carries a body.

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	fcmDomain "github.com/akashc777/OneCamp/domain/UserFCMToken"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	deliveryModels "github.com/akashc777/OneCamp/models/postgres/NotificationDelivery"
	"github.com/google/uuid"
)

// Devices are where the asker's notices can show: their devices with push on,
// and the OneCamp apps (desktop or browser) open now, which show a desktop
// notice. PushKnown is false when their devices couldn't be read, AppsKnown
// when the broker couldn't be asked.
type Devices struct {
	Push      int  `json:"push_devices"`
	PushKnown bool `json:"push_known"`
	Apps      int  `json:"apps_open"`
	AppsKnown bool `json:"apps_known"`
}

// TestResult is what a test notice did: the plan's steps for the asker, where
// it can show, and the deliveries told now.
type TestResult struct {
	Trace
	Devices Devices    `json:"devices"`
	Told    []Delivery `json:"told"`
}

// TooSoon is a second test within the minute: refused until RetryAt.
type TooSoon struct{ RetryAt time.Time }

func (e *TooSoon) Error() string {
	return "business/Notify Test: one test a minute, the next at " + e.RetryAt.UTC().Format(time.RFC3339)
}

// ErrNoRecord is a message that told the asker nothing: someone else's, an id
// that is no message, or one from before the record.
var ErrNoRecord = errors.New("business/Notify Why: no record of that for you")

// Seams for tests: the asker's devices with push on, their apps open now,
// and their rows of a message.
var (
	pushDevices = func(ctx context.Context, asker string) (int, error) {
		_, tokens, err := fcmDomain.ReachableThroughPause(ctx, []string{asker})
		n := 0
		for _, t := range tokens {
			if t != "" {
				n++
			}
		}
		return n, err
	}
	appsOpen = mqttBusiness.AppsOpen
	rowsOf   = deliveryModels.OfMessage
)

// testTitle is a test notice's whole text: a title, never a body.
const testTitle = "Test from OneCamp: notifications reach this device"

// Test sends asker a test notice at at, through the router as any notice
// goes: planned for asker alone (their hold decides, as it would for a
// message), then delivered to their own devices. Its event is the minute's
// ("test:<asker>:<minute>"), so the record's one row per event, person and
// delivery is the limit: a second test within the minute tells nothing new
// and answers TooSoon with the next minute.
func Test(ctx context.Context, asker string, at time.Time) (TestResult, error) {
	id, err := uuid.Parse(asker)
	if err != nil || id == uuid.Nil {
		return TestResult{}, errNoAsker
	}
	asker = id.String()
	minute := at.UTC().Truncate(time.Minute)
	ev := Event{
		ID:       "test:" + asker + ":" + minute.Format("200601021504"),
		Cause:    CauseTest,
		Audience: []string{asker},
		Push: map[string]string{
			firebaseInit.FIREBASE_PUSH_DATA_TYPE:  firebaseInit.FIREBASE_PUSH_DATA_TYPE_TEST,
			firebaseInit.FIREBASE_PUSH_DATA_TITLE: testTitle,
		},
	}
	tr, err := Notices.Explain(ctx, asker, ev)
	if err != nil {
		return TestResult{}, err
	}
	res := TestResult{Trace: tr, Told: []Delivery{}}
	if tr.Decision != nil {
		told := Notices.Deliver(ctx, ev, []Decision{*tr.Decision})
		if len(told) == 0 {
			return TestResult{}, &TooSoon{RetryAt: minute.Add(time.Minute)}
		}
		res.Told = append(res.Told, told[0].To...)
	}
	res.Devices = devicesOf(ctx, asker)
	return res, nil
}

// devicesOf is where asker's notices can show now.
func devicesOf(ctx context.Context, asker string) Devices {
	var d Devices
	n, err := pushDevices(ctx, asker)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Notify Test: could not read %s's devices: %+v", asker, err)
	} else {
		d.Push, d.PushKnown = n, true
	}
	d.Apps, d.AppsKnown = appsOpen(ctx, asker)
	return d
}

// Why is asker's record of messageID: for each event about it that reached
// them, newest first, the trace of what was decided (its cause, each step's
// verdict, what was told and what held) and each delivery as it stands now.
// Only asker's rows are read, and only theirs answered whatever the read
// let through; none answers ErrNoRecord, as does an id that is no message.
func Why(ctx context.Context, asker, messageID string) ([]Trace, error) {
	me, err := uuid.Parse(asker)
	if err != nil || me == uuid.Nil {
		return nil, errNoAsker
	}
	msg, err := uuid.Parse(messageID)
	if err != nil {
		return nil, ErrNoRecord
	}
	rows, err := rowsOf(ctx, me.String(), msg.String())
	if err != nil {
		return nil, err
	}
	var order []string
	byEvent := map[string][]deliveryModels.Row{}
	for _, r := range rows {
		if key(r.UserID) != key(me.String()) {
			continue
		}
		if _, seen := byEvent[r.EventID]; !seen {
			order = append(order, r.EventID)
		}
		byEvent[r.EventID] = append(byEvent[r.EventID], r)
	}
	if len(order) == 0 {
		return nil, ErrNoRecord
	}
	latest := func(rs []deliveryModels.Row) time.Time {
		var t time.Time
		for _, r := range rs {
			if r.CreatedAt.After(t) {
				t = r.CreatedAt
			}
		}
		return t
	}
	slices.SortStableFunc(order, func(a, b string) int { return latest(byEvent[b]).Compare(latest(byEvent[a])) })
	out := make([]Trace, 0, len(order))
	for _, id := range order {
		out = append(out, recalled(me.String(), id, byEvent[id]))
	}
	return out, nil
}

// deliveryOrder is the order a trace lists deliveries in.
var deliveryOrder = []Delivery{ToInbox, ToBadge, ToPush, ToDesktop, ToSound, ToEmail}

// recalled is the trace of one event for asker from its rows: they were in
// its audience by the cause recorded, in the place, the cause chosen, their
// setting kept it (or no row was written), and then what the hold did. What
// was sent, then retracted, was told; what was held, then dropped, was held.
// Pure.
func recalled(asker, eventID string, rows []deliveryModels.Row) Trace {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b deliveryModels.Row) int {
		return cmp.Compare(slices.Index(deliveryOrder, Delivery(a.Delivery)), slices.Index(deliveryOrder, Delivery(b.Delivery)))
	})
	cause := Cause(rows[0].Cause)
	tr := Trace{UserID: asker, EventID: eventID}
	d := Decision{UserID: asker, Cause: cause}
	anyHoldable := false
	for _, r := range rows {
		to := Delivery(r.Delivery)
		tr.Deliveries = append(tr.Deliveries, DeliveryState{Delivery: to, State: r.State, HoldUntil: r.HoldUntil, At: r.CreatedAt})
		anyHoldable = anyHoldable || holdable(to)
		switch r.State {
		case "held", "dropped":
			d.Held = append(d.Held, to)
			if r.HoldUntil != nil && (d.HoldTill == nil || r.HoldUntil.After(*d.HoldTill)) {
				d.HoldTill = r.HoldUntil
			}
		default:
			d.To = append(d.To, to)
		}
	}
	hold := "nothing_to_hold"
	switch {
	case len(d.Held) > 0:
		hold = "held"
	case anyHoldable:
		hold = "not_held"
	}
	tr.Steps = []Step{
		{Step: "audience", Verdict: "in", Causes: []Cause{cause}},
		{Step: "access", Verdict: "in_place"},
		{Step: "cause", Verdict: "chosen", Causes: []Cause{cause}},
		{Step: "preference", Verdict: "kept"},
		{Step: "hold", Verdict: hold},
	}
	tr.Decision = &d
	return tr
}
