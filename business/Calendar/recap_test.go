package Calendar

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// recapEvent is Sam's event with Maya, a guest by email, and its id.
func recapEvent(t *testing.T, h *harness, sam, maya *model.UserInfo) (*Event, uuid.UUID) {
	t.Helper()
	st, en := timed(time.Date(2026, 10, 16, 9, 15, 0, 0, time.UTC), 30, "Europe/London")
	e, err := h.s.Create(context.Background(), sam, EventInput{Title: "Launch sync", Notes: "Private terms: 4% discount", Location: "Room 4", Start: st, End: en,
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}, {Email: "guest@example.org", Name: "Gita"}}})
	if err != nil {
		t.Fatal(err)
	}
	return e, uuid.MustParse(e.ID)
}

var launchRecap = Recap{Key: "call:s1", Text: "**📝 Summary**\n- We ship the beta on Friday.\n\n**✅ Decisions**\n- The Business tier launches.",
	Items: []RecapItem{{Text: "Send the deck", Owner: "Maya", Due: "2026-11-14"}, {Text: "Book the room"}}}

// The recap reaches the event's members (who may read its call's
// transcript), each once, through the router: the organiser too, as nobody
// is its actor. A member it doesn't invite never hears of it.
func TestARecapReachesTheEventsMembersThroughTheRouter(t *testing.T) {
	h := newHarness(t)
	sam, maya, lee := h.member("Sam"), h.member("Maya"), h.member("Lee")
	_, id := recapEvent(t, h, sam, maya)
	got := captureNotices(t)
	captureMail(t, true)
	if err := h.s.tellRecap(context.Background(), id, launchRecap); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("told %d notices, want one", len(*got))
	}
	n, told := (*got)[0].n, (*got)[0].r
	var who []string
	for _, r := range told {
		who = append(who, r.UserUUID+"|"+r.Reason)
	}
	slices.Sort(who)
	want := []string{sam.UserDgraphInfo.Uuid + "|organiser", maya.UserDgraphInfo.Uuid + "|guest"}
	slices.Sort(want)
	if !slices.Equal(who, want) || slices.Contains(who, lee.UserDgraphInfo.Uuid+"|guest") {
		t.Errorf("told %v, want the organiser and the member guest alone", who)
	}
	if n.Kind != "calendar.recap" || n.Email != notificationBusiness.EventCalendarRecap || n.ResourceUUID != id.String() ||
		n.Title != "Recap of Launch sync" || n.Body != "We ship the beta on Friday." || n.Path != "/app/calendar/event/"+id.String() ||
		n.Data["call"] != "call:s1" || n.DedupKey != "calendar.recap|"+id.String()+"|call:s1" {
		t.Errorf("notice = %+v", n)
	}
	if strings.Contains(n.Body+n.Title, "4% discount") {
		t.Errorf("the notice says the event's notes: %+v", n)
	}
}

// A recap's notice keeps its words as written: the Inbox and a push show them
// as text, and its email shows them as plain text, escaped once where emails
// are made (tests/integration: TestARecapsNoticeEmailShowsWhatTheRecapSaysAsPlainText).
func TestARecapsNoticeKeepsItsWordsAsWritten(t *testing.T) {
	h := newHarness(t)
	sam, maya := h.member("Sam"), h.member("Maya")
	_, id := recapEvent(t, h, sam, maya)
	got := captureNotices(t)
	captureMail(t, true)
	const wrote = `We ship <a href="https://x">y</a> & <img src=z>`
	if err := h.s.tellRecap(context.Background(), id, Recap{Key: "call:s2", Text: "**📝 Summary**\n- " + wrote}); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0].n.Body != wrote {
		t.Fatalf("told %+v, want one notice whose body is %q as written", *got, wrote)
	}
}

// Guests by email get the recap by email only when the organiser turned on
// "Send the recap to guests": its words and action items, and nothing else
// of the event's: not its notes, its place, its call or an answer link.
func TestARecapIsEmailedToGuestsOnlyWhenTheOrganiserSaysSo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e, id := recapEvent(t, h, sam, maya)
	captureNotices(t)
	mail := captureMail(t, true)
	if err := h.s.tellRecap(ctx, id, launchRecap); err != nil || len(*mail) != 0 {
		t.Fatalf("switch off: %v, sent %+v; want no email to guests", err, *mail)
	}
	if _, err := h.s.SetRecapToGuests(ctx, sam, e.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := h.s.tellRecap(ctx, id, launchRecap); err != nil || len(*mail) != 1 {
		t.Fatalf("switch on: %v, sent %d emails, want one", err, len(*mail))
	}
	m := (*mail)[0]
	if m.to != "guest@example.org" || m.subject != "Recap: Launch sync" || m.ics != "" {
		t.Errorf("mail = %+v", m)
	}
	for _, want := range []string{"We ship the beta on Friday.", "Decisions</strong>", "The Business tier launches.", "Action items", "Send the deck (Maya, due 2026-11-14)", "Book the room"} {
		if !strings.Contains(m.html, want) {
			t.Errorf("the email lacks %q", want)
		}
	}
	for _, never := range []string{"4% discount", "Room 4", "/r/", "/rsvp/", "call:s1", "transcript", "recording"} {
		if strings.Contains(strings.ToLower(m.html), strings.ToLower(never)) {
			t.Errorf("the email says %q", never)
		}
	}
	// A server that emails nobody outside sends none.
	off := captureMail(t, false)
	_ = h.s.tellRecap(ctx, id, launchRecap)
	if len(*off) != 0 {
		t.Errorf("sent %d with outside email off", len(*off))
	}
}

// The switch is the organiser's alone, and it moves no sequence: a guest who
// may change the event is refused, a guest who may only read it too, anyone
// else is told there is no such event. Only the organiser reads it.
func TestOnlyTheOrganiserSendsTheRecapToGuests(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, lee := h.member("Sam"), h.member("Maya"), h.member("Lee")
	e, _ := recapEvent(t, h, sam, maya)
	if _, err := h.s.SetRecapToGuests(ctx, maya, e.ID, true); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("Maya, a guest: %v", err)
	}
	yes := true
	cur, _ := h.s.Get(ctx, sam, e.ID)
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(cur.Sequence), GuestsCanModify: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.SetRecapToGuests(ctx, maya, e.ID, true); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("Maya, a guest who may change the event: %v", err)
	}
	if _, err := h.s.SetRecapToGuests(ctx, lee, e.ID, true); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("Lee, not on it: %v", err)
	}
	before, _ := h.s.Get(ctx, sam, e.ID)
	got, err := h.s.SetRecapToGuests(ctx, sam, e.ID, true)
	if err != nil || !got.RecapToGuests || got.Sequence != before.Sequence {
		t.Fatalf("Sam: %+v, %v; want it on at sequence %d", got, err, before.Sequence)
	}
	if seen, _ := h.s.Get(ctx, maya, e.ID); seen.RecapToGuests {
		t.Error("a guest reads the organiser's switch")
	}
	if got, err := h.s.SetRecapToGuests(ctx, sam, e.ID, false); err != nil || got.RecapToGuests {
		t.Errorf("off again: %+v, %v", got, err)
	}
}

// A repeating event's switch is its series': the recap of an instance's call
// goes to guests by email when the series says so.
func TestARecapOfASeriesFollowsTheSeriesSwitch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	st, en := timed(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), 15, "Europe/London")
	s, err := h.s.Create(ctx, sam, EventInput{Title: "Stand-up", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=WEEKLY"},
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}, {Email: "guest@example.org"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.SetRecapToGuests(ctx, sam, instanceRef(uuid.MustParse(s.ID), *st.At, false), true); err != nil {
		t.Fatal(err)
	}
	captureNotices(t)
	mail := captureMail(t, true)
	if err := h.s.tellRecap(ctx, uuid.MustParse(s.ID), launchRecap); err != nil || len(*mail) != 1 {
		t.Errorf("the series' recap: %v, %d emails, want one", err, len(*mail))
	}
}

// A recap that says nothing tells nobody; one of an event that's gone is
// refused as no such event.
func TestAnEmptyRecapTellsNobody(t *testing.T) {
	h := newHarness(t)
	got := captureNotices(t)
	if err := h.s.tellRecap(context.Background(), uuid.New(), Recap{Key: "call:s1"}); err != nil || len(*got) != 0 {
		t.Errorf("an empty recap: %v, told %d", err, len(*got))
	}
	if err := h.s.tellRecap(context.Background(), uuid.New(), launchRecap); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("no such event: %v", err)
	}
}

// "This and following" keeps the series' switch on the new series it makes.
func TestANewSeriesKeepsItsSeriesSwitch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam := h.member("Sam")
	st, en := timed(time.Date(2026, 10, 20, 9, 15, 0, 0, time.UTC), 15, "Europe/London")
	s, err := h.s.Create(ctx, sam, EventInput{Title: "Stand-up", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=WEEKLY"},
		Attendees: []GuestInput{{Email: "alex@example.org"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.SetRecapToGuests(ctx, sam, s.ID, true); err != nil {
		t.Fatal(err)
	}
	in := h.between(sam, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))
	later := "Stand-up (later)"
	fresh, err := h.s.Update(ctx, sam, in[1].ID, EventPatch{Sequence: &in[1].Sequence, Title: &later, Scope: ScopeFollowing})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == s.ID || !h.store.events[uuid.MustParse(fresh.ID)].RecapToGuests {
		t.Errorf("the new series %s doesn't keep the switch", fresh.ID)
	}
}

// An edition without AI keeps no action items and has no maker: every Make
// task is an item not found, and the calendar imports no AI code for it.
func TestMakeTaskWithoutAMakerFindsNoItem(t *testing.T) {
	was := actionTaskMaker
	t.Cleanup(func() { actionTaskMaker = was })
	actionTaskMaker = nil
	if got, err := MakeActionTask(context.Background(), &model.UserInfo{}, "e1", "call:s1", 0, "p1"); got != nil || !errors.Is(err, ErrNoActionItem) {
		t.Errorf("MakeActionTask = %+v, %v; want ErrNoActionItem", got, err)
	}
}

// A member of the conversation the recap was posted to (the event's DM or
// group) has it there: the chat message is all they get, never a second
// notice in their inbox or by email. The event's other members still hear of
// it, and its guests by email are mailed as the organiser's switch says.
func TestARecapPostedToTheirConversationIsAllItsMembersGet(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e, id := recapEvent(t, h, sam, maya)
	if _, err := h.s.SetRecapToGuests(ctx, sam, e.ID, true); err != nil {
		t.Fatal(err)
	}
	samID, mayaID := uuid.MustParse(sam.UserDgraphInfo.Uuid), uuid.MustParse(maya.UserDgraphInfo.Uuid)
	for _, c := range []struct {
		name   string
		inChat []uuid.UUID
		want   []string
	}{
		{"Maya had it in the chat", []uuid.UUID{mayaID}, []string{samID.String()}},
		{"both had it in their DM", []uuid.UUID{samID, mayaID}, nil},
		{"nobody had it", nil, []string{samID.String(), mayaID.String()}},
	} {
		got := captureNotices(t)
		mail := captureMail(t, true)
		r := launchRecap
		r.InChat = c.inChat
		if err := h.s.tellRecap(ctx, id, r); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var told []string
		for _, n := range *got {
			for _, to := range n.r {
				told = append(told, to.UserUUID)
			}
		}
		slices.Sort(told)
		slices.Sort(c.want)
		if !slices.Equal(told, c.want) {
			t.Errorf("%s: told %v, want %v", c.name, told, c.want)
		}
		if len(*mail) != 1 || (*mail)[0].to != "guest@example.org" {
			t.Errorf("%s: mailed %+v, want the guest by email mailed as before", c.name, *mail)
		}
	}
}

// A recap whose words say only that there is nothing ("N/A", "None", "No
// action items") has no line for its notice: the notice says how many
// action items it has.
func TestARecapThatSaysNoneTellsItsActionItems(t *testing.T) {
	for _, none := range []string{"N/A", "n/a.", "None", "No action items."} {
		r := Recap{Key: "call:s1", Text: "**📝 Summary**\n- " + none, Items: []RecapItem{{Text: "Send the deck"}, {Text: "Book the room"}}}
		if got := recapLine(r); got != "2 action items" {
			t.Errorf("a summary of %q: the notice says %q, want \"2 action items\"", none, got)
		}
	}
}

// The recap's words are the model's, which anyone on the call can steer, and
// its email goes to guests outside the workspace: its words, and each action
// item's words, owner and due date, reach them as text alone, never as a
// link, an image or a script; its plain-text part says them as written.
func TestARecapsEmailHoldsWhatTheModelWroteAsText(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e, id := recapEvent(t, h, sam, maya)
	if _, err := h.s.SetRecapToGuests(ctx, sam, e.ID, true); err != nil {
		t.Fatal(err)
	}
	captureNotices(t)
	mail := captureMail(t, true)
	link, img, script, js := `<a href="javascript:alert(1)">the deck</a>`, `<img src=x onerror=alert(2)>`, `<script>alert(3)</script>`, `javascript:alert(4)`
	r := Recap{Key: "call:s1", Text: "**📝 Summary**\n- R&D ships " + link + "\n- " + img + " " + js,
		Items: []RecapItem{{Text: script, Owner: link, Due: img}, {Text: "Read " + js, Owner: script, Due: js}}}
	if err := h.s.tellRecap(ctx, id, r); err != nil || len(*mail) != 1 {
		t.Fatalf("tellRecap: %v, %d emails, want one", err, len(*mail))
	}
	m := (*mail)[0]
	low := strings.ToLower(m.html)
	for _, live := range []string{"<a ", "<a>", "<img", "<script", `href="javascript`} {
		if strings.Contains(low, live) {
			t.Errorf("the email to guests holds %q live", live)
		}
	}
	for _, as := range []string{"&lt;script&gt;alert(3)&lt;/script&gt;", "&lt;img src=x onerror=alert(2)&gt;", "R&amp;D ships &lt;a href=", "Read javascript:alert(4)"} {
		if !strings.Contains(m.html, as) {
			t.Errorf("the email lacks %q, escaped as text", as)
		}
	}
	for _, said := range []string{"R&D ships " + link, img + " " + js, script + " (" + link + ", due " + img + ")", "Read " + js + " (" + script + ", due " + js + ")"} {
		if !strings.Contains(m.text, said) {
			t.Errorf("the plain text lacks %q as written: %q", said, m.text)
		}
	}
}

// A moved instance keeps a row of its own, and its switch is still its
// series': turned on for the series after the move, the moved instance's
// recap is emailed to its guests.
func TestAMovedInstancesRecapFollowsItsSeriesSwitch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	st, en := timed(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), 15, "Europe/London")
	s, err := h.s.Create(ctx, sam, EventInput{Title: "Stand-up", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=WEEKLY"},
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}, {Email: "guest@example.org"}}})
	if err != nil {
		t.Fatal(err)
	}
	second := instanceRef(uuid.MustParse(s.ID), st.At.AddDate(0, 0, 7), false)
	cur, err := h.s.Get(ctx, sam, second)
	if err != nil {
		t.Fatal(err)
	}
	p := patchAt(*cur, ScopeThis)
	p.Title = str("Stand-up, offsite")
	if _, err := h.s.Update(ctx, sam, second, p); err != nil {
		t.Fatal(err)
	}
	var moved uuid.UUID
	h.store.mu.Lock()
	for rid, ev := range h.store.events {
		if ev.SeriesID != nil && ev.SeriesID.String() == s.ID && !ev.RecapToGuests {
			moved = rid
		}
	}
	h.store.mu.Unlock()
	if moved == uuid.Nil {
		t.Fatal("the moved instance has no row of its own with the switch off")
	}
	if _, err := h.s.SetRecapToGuests(ctx, sam, s.ID, true); err != nil {
		t.Fatal(err)
	}
	captureNotices(t)
	mail := captureMail(t, true)
	if err := h.s.tellRecap(ctx, moved, launchRecap); err != nil || len(*mail) != 1 || (*mail)[0].subject != "Recap: Stand-up, offsite" {
		t.Errorf("the moved instance's recap: %v, %+v; want one email to its guest", err, *mail)
	}
}

// The public demo never emails a recap to anyone (the guard the demo's
// seeded guests rely on: outsidersMailed is off in DEMO_MODE): with email
// on and the organiser's switch on, a demo server sends its guest nothing,
// and the switch still saves. The same server outside the demo sends one.
func TestTheDemoNeverEmailsARecapToGuests(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e, id := recapEvent(t, h, sam, maya)
	captureNotices(t)
	t.Setenv("RESEND_API_KEY", "re_not_a_real_key")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	guard := outsidersMailed
	mail := captureMail(t, true)
	outsidersMailed = guard // the server's own rule, not the test's
	t.Setenv("DEMO_MODE", "true")
	got, err := h.s.SetRecapToGuests(ctx, sam, e.ID, true)
	if err != nil || got == nil || !got.RecapToGuests {
		t.Fatalf("the demo's switch: %+v, %v; want it saved on", got, err)
	}
	if err := h.s.tellRecap(ctx, id, launchRecap); err != nil || len(*mail) != 0 {
		t.Fatalf("on the demo: %v, sent %+v; want nothing sent", err, *mail)
	}
	t.Setenv("DEMO_MODE", "")
	if err := h.s.tellRecap(ctx, id, launchRecap); err != nil || len(*mail) != 1 {
		t.Errorf("outside the demo: %v, sent %d; want the one email (the test reaches the send)", err, len(*mail))
	}
}
