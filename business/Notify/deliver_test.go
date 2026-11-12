package business

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	deliveryModels "github.com/akashc777/OneCamp/models/postgres/NotificationDelivery"
)

// fakeRows is notification_deliveries: its unique key, and what Retract does.
type fakeRows struct {
	mu        sync.Mutex
	rows      map[string]deliveryModels.Row
	fail      error
	retracted []string
	// told is whose rows of a message a retract takes back.
	told []string
	// notices is each notice take-back: the message and whose.
	notices []string
}

func (f *fakeRows) record(_ context.Context, rows []deliveryModels.Row) ([]deliveryModels.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	if f.rows == nil {
		f.rows = map[string]deliveryModels.Row{}
	}
	var fresh []deliveryModels.Row
	for _, r := range rows {
		k := r.EventID + "|" + key(r.UserID) + "|" + r.Delivery
		if _, had := f.rows[k]; had {
			continue
		}
		f.rows[k] = r
		fresh = append(fresh, deliveryModels.Row{UserID: key(r.UserID), Delivery: r.Delivery})
	}
	return fresh, nil
}

func (f *fakeRows) retract(_ context.Context, messageID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retracted = append(f.retracted, messageID)
	if f.fail != nil {
		return nil, f.fail
	}
	return f.told, nil
}

func (f *fakeRows) retractNotices(_ context.Context, messageID string, people []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notices = append(f.notices, messageID+" "+strings.Join(people, ","))
	return nil
}

// pushes is who each push call told, gated (the push gate's own hold) or
// through (the plan's hold already decided).
type pushes struct {
	mu             sync.Mutex
	gated, through [][]string
}

// withFakes swaps the rows and both pushes for the test.
func withFakes(t *testing.T) (*fakeRows, *pushes) {
	t.Helper()
	rows, p := &fakeRows{}, &pushes{}
	oldRecord, oldRetract, oldNotices, oldDeliver, oldThrough := recordRows, retractRows, retractNotices, deliver, deliverThrough
	recordRows, retractRows, retractNotices = rows.record, rows.retract, rows.retractNotices
	deliver = func(_ context.Context, to []string, _ map[string]string, _ string) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.gated = append(p.gated, slices.Clone(to))
	}
	deliverThrough = func(_ context.Context, to []string, _ map[string]string, _ string) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.through = append(p.through, slices.Clone(to))
		return nil
	}
	t.Cleanup(func() {
		recordRows, retractRows, retractNotices, deliver, deliverThrough = oldRecord, oldRetract, oldNotices, oldDeliver, oldThrough
	})
	return rows, p
}

const (
	msgID = "6f1c0d7e-8a6b-4c55-9d1e-2b3c4d5e6f70"
	annID = "0a6e3c1d-1111-4c55-9d1e-2b3c4d5e6f70"
	boID  = "0b6e3c1d-2222-4c55-9d1e-2b3c4d5e6f70"
)

var (
	posted  = Event{ID: "channel:ch:" + msgID, Cause: CauseMessage, Actor: "me", Place: channel, MessageID: msgID}
	twoTold = []Decision{
		{UserID: annID, Cause: CauseMention, To: named},
		{UserID: boID, Cause: CauseAllPosts, To: []Delivery{ToPush, ToDesktop, ToSound}},
	}
)

// Each (event, person, delivery) is one row: a second fire of the same event
// (a retry, a double send) records nothing new and tells nobody again.
func TestADoubleFireTellsNobodyTwice(t *testing.T) {
	rows, p := withFakes(t)
	r := routerOn(&fakeInputs{})
	first := r.Deliver(context.Background(), posted, twoTold)
	if !reflect.DeepEqual(first, twoTold) {
		t.Errorf("first fire told %+v, want %+v", first, twoTold)
	}
	if again := r.Deliver(context.Background(), posted, twoTold); len(again) != 0 {
		t.Errorf("second fire told %+v, want nobody", again)
	}
	if want := [][]string{{annID, boID}}; !reflect.DeepEqual(p.through, want) || len(p.gated) != 0 {
		t.Errorf("pushes through %v gated %v, want %v through the plan's hold, once", p.through, p.gated, want)
	}
	if len(rows.rows) != len(named)+3 {
		t.Errorf("%d rows, want one per person and delivery (%d)", len(rows.rows), len(named)+3)
	}
	for _, r := range rows.rows {
		if r.State != "sent" || r.SentAt == nil || r.MessageID != msgID || r.EventID != posted.ID {
			t.Errorf("row %+v: want sent now, of the message and the event", r)
		}
	}
	if r := rows.rows[posted.ID+"|"+annID+"|email"]; r.Cause != string(CauseMention) {
		t.Errorf("Ann's email row's cause %q, want %q", r.Cause, CauseMention)
	}
}

// A mention an edit adds goes through the door (V-843): Plan's, of those it
// newly names (never the writer, nor whom it named before), one row per
// person and delivery, cause chat.mention, of the message the save is of,
// pushed as the notice reads; its id is the message's, so a retry, a second
// save, or a remove and a re-add records nothing new and pushes nobody again.
func TestAnEditsMentionGoesThroughTheDoor(t *testing.T) {
	rows, p := withFakes(t)
	old := Notices
	Notices = routerOn(&fakeInputs{levels: map[string]string{"me": "all", annID: "mention", boID: "mention"}})
	t.Cleanup(func() { Notices = old })
	n := notificationBusiness.Notice{Actor: &dgraphStruct.DgraphUser{Uuid: "me", UserName: "Me"}, Title: "Me mentioned you in #launch",
		Push: &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL, ID: "ch", Thread: msgID}}
	save := notificationBusiness.MentionSave{Kind: "post", ID: msgID, Before: []string{boID}, After: []string{boID, annID, "me"}}
	var data map[string]string
	deliverThrough = func(_ context.Context, to []string, d map[string]string, _ string) error {
		p.through, data = append(p.through, slices.Clone(to)), d
		return nil
	}
	if told := TellAddedMentions(context.Background(), channel, n, save); len(told) != 1 || told[0].UserID != annID || !slices.Equal(told[0].To, named) {
		t.Fatalf("told %+v; want Ann alone, by every delivery of a mention", told)
	}
	if len(rows.rows) != len(named) {
		t.Errorf("%d rows, want Ann's %d", len(rows.rows), len(named))
	}
	for _, r := range rows.rows {
		if r.EventID != "mention:post:"+msgID || r.MessageID != msgID || r.Cause != string(CauseMention) || key(r.UserID) != annID {
			t.Errorf("row %+v: want Ann's, chat.mention, of the message, under its id", r)
		}
	}
	if !reflect.DeepEqual(p.through, [][]string{{annID}}) || data[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != n.Title ||
		data[firebaseInit.FIREBASE_PUSH_DATA_TYPE] != firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL || data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] != msgID {
		t.Errorf("pushed %v with %v; want Ann, as the notice reads", p.through, data)
	}
	save.Before, save.After = []string{boID}, []string{boID, annID}
	if again := TellAddedMentions(context.Background(), channel, n, save); len(again) != 0 || len(rows.rows) != len(named) || len(p.through) != 1 {
		t.Errorf("a re-add told %+v (%d rows, %d pushes); want nobody again", again, len(rows.rows), len(p.through))
	}
	save.Before = save.After
	if none := TellAddedMentions(context.Background(), channel, n, save); none != nil {
		t.Errorf("a save that adds nobody told %+v", none)
	}
}

// A row that can't be written never fails the write that caused it: it is
// logged, and everyone planned is told as today.
func TestARowWriteFailureNeverFailsTheWrite(t *testing.T) {
	rows, p := withFakes(t)
	rows.fail = errors.New("database is down")
	var told []Decision
	func() {
		defer func() {
			if v := recover(); v != nil {
				t.Fatalf("a row write's failure panicked: %v", v)
			}
		}()
		told = routerOn(&fakeInputs{}).Deliver(context.Background(), posted, twoTold)
	}()
	if !reflect.DeepEqual(told, twoTold) {
		t.Errorf("told %+v, want everyone planned", told)
	}
	if want := [][]string{{annID, boID}}; !reflect.DeepEqual(p.through, want) {
		t.Errorf("pushes %v, want %v", p.through, want)
	}
}

// A held delivery is a row in state held until the hold ends (the Sweeper's,
// A4 H4), never a push now; the inbox and badge move now.
func TestAHeldDeliveryIsARowNotAPush(t *testing.T) {
	rows, p := withFakes(t)
	till := at.Add(9 * time.Hour)
	ds := []Decision{{UserID: annID, Cause: CauseMention, To: []Delivery{ToInbox, ToBadge},
		Held: []Delivery{ToPush, ToDesktop, ToSound, ToEmail}, HoldTill: &till}}
	told := routerOn(&fakeInputs{}).Deliver(context.Background(), posted, ds)
	if !reflect.DeepEqual(told, ds) {
		t.Errorf("told %+v, want %+v", told, ds)
	}
	if len(p.through)+len(p.gated) != 0 {
		t.Errorf("a held person pushed now: through %v gated %v", p.through, p.gated)
	}
	var states []string
	for _, r := range rows.rows {
		states = append(states, r.Delivery+":"+r.State)
		if r.State == "held" && (r.HoldUntil == nil || !r.HoldUntil.Equal(till) || r.SentAt != nil) {
			t.Errorf("held row %+v: want hold_until %v and not sent", r, till)
		}
	}
	sort.Strings(states)
	want := []string{"badge:sent", "desktop:held", "email:held", "inbox:sent", "push:held", "sound:held"}
	if !reflect.DeepEqual(states, want) {
		t.Errorf("rows %v, want %v", states, want)
	}
}

// An event its site decided (the calendar's) is pushed through the push gate,
// as today; nothing is recorded for an event with no id.
func TestASitesEventIsPushedThroughTheGate(t *testing.T) {
	rows, p := withFakes(t)
	ds := []Decision{{UserID: annID, Cause: CauseCalendarInvited, To: []Delivery{ToInbox, ToPush, ToDesktop}}}
	routerOn(&fakeInputs{}).Deliver(context.Background(), Event{ID: "calendar:ev:1", Cause: CauseCalendarInvited}, ds)
	if want := [][]string{{annID}}; !reflect.DeepEqual(p.gated, want) || len(p.through) != 0 {
		t.Errorf("gated %v through %v, want %v gated", p.gated, p.through, want)
	}
	if len(rows.rows) != 3 {
		t.Errorf("%d rows, want 3", len(rows.rows))
	}
	routerOn(&fakeInputs{}).Deliver(context.Background(), Event{Cause: CauseCalendarInvited}, ds)
	if len(rows.rows) != 3 || len(p.gated) != 2 {
		t.Errorf("an event with no id: %d rows, %d pushes; want none recorded, told as today", len(rows.rows), len(p.gated))
	}
}

// Retract takes back a deleted message's rows, by its id; an id that isn't
// one asks nothing, and a failure is logged, never the delete's.
func TestRetractTakesBackTheMessagesNotices(t *testing.T) {
	rows, _ := withFakes(t)
	r := routerOn(&fakeInputs{})
	r.Retract(context.Background(), msgID)
	r.Retract(context.Background(), "not-a-message")
	rows.fail = errors.New("database is down")
	r.Retract(context.Background(), msgID)
	if want := []string{msgID, msgID}; !reflect.DeepEqual(rows.retracted, want) {
		t.Errorf("retracted %v, want %v", rows.retracted, want)
	}
	if len(rows.notices) != 0 {
		t.Errorf("notices taken back %v with no rows taken back, want none", rows.notices)
	}
}

// Retract takes the message's inbox notices back from whose rows it took
// back, by the same path: one rule for every delete (V-843).
func TestRetractTakesTheInboxNoticesBackFromWhomItTold(t *testing.T) {
	rows, _ := withFakes(t)
	rows.told = []string{"sam", "ada"}
	routerOn(&fakeInputs{}).Retract(context.Background(), msgID)
	if want := []string{msgID + " sam,ada"}; !reflect.DeepEqual(rows.notices, want) {
		t.Errorf("notices taken back %v, want %v", rows.notices, want)
	}
}
