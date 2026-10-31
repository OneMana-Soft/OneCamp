package Calendar

import (
	"context"
	"testing"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func oooAt(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC) }

// answerIn is who's answer and note on the event with id, as stored.
func (h *harness) answerIn(id string, who *model.UserInfo) (string, string) {
	h.t.Helper()
	for _, g := range h.store.guests[uuid.MustParse(id)] {
		if g.UserID != nil && *g.UserID == personOf(who) {
			return g.Response, g.Note
		}
	}
	return "", ""
}

func (h *harness) oooMeeting(who *model.UserInfo, title string, start time.Time, guests ...*model.UserInfo) *Event {
	h.t.Helper()
	st, en := timed(start, 30, "UTC")
	in := EventInput{Title: title, Start: st, End: en}
	for _, g := range guests {
		in.Attendees = append(in.Attendees, GuestInput{UUID: g.UserDgraphInfo.Uuid})
	}
	e, err := h.s.Create(context.Background(), who, in)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

func (h *harness) away(who *model.UserInfo, from, to time.Time, decline, message string) *Event {
	h.t.Helper()
	in := EventInput{Kind: EventAway, Title: "Out of office", Start: When{At: &from, Zone: "UTC"}, End: When{At: &to},
		OutOfOffice: &OOOInput{Decline: decline, Message: message}}
	e, err := h.s.Create(context.Background(), who, in)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// Out of office with "new" declines an invitation that arrives later, to an
// event that starts while away, with its message, and tells the organiser;
// a guest who isn't away, and an event after it, are left to answer.
func TestOutOfOfficeDeclinesNewInvitationsWithItsMessage(t *testing.T) {
	h := newHarness(t)
	sam, maya, lee := h.member("Sam"), h.member("Maya"), h.member("Lee")
	h.away(maya, oooAt(20, 0), oooAt(23, 0), "new", "Away until Friday, ask Lee.")

	during := h.oooMeeting(sam, "Launch sync", oooAt(21, 10), maya, lee)
	if r, note := h.answerIn(during.ID, maya); r != responseDeclined || note != "Away until Friday, ask Lee." {
		t.Fatalf("Maya away: %q %q, want declined with her message", r, note)
	}
	if r, _ := h.answerIn(during.ID, lee); r != responseNeedsAction {
		t.Fatalf("Lee isn't away: %q", r)
	}
	told := false
	for _, x := range h.tells.of(toldAnswered) {
		if x.answer != nil && x.answer.key.UserID != nil && *x.answer.key.UserID == personOf(maya) && x.answer.note == "Away until Friday, ask Lee." {
			told = true
		}
	}
	if !told {
		t.Fatal("Sam should be told Maya declined, with her message")
	}

	after := h.oooMeeting(sam, "Retro", oooAt(24, 10), maya)
	if r, _ := h.answerIn(after.ID, maya); r != responseNeedsAction {
		t.Fatalf("after the away: %q, want it left to answer", r)
	}

	// Added later by a change, Maya is declined then too.
	added := h.oooMeeting(sam, "Pricing", oooAt(22, 9), lee)
	guests := []GuestInput{{UUID: lee.UserDgraphInfo.Uuid}, {UUID: maya.UserDgraphInfo.Uuid}}
	if _, err := h.s.Update(context.Background(), sam, added.ID, EventPatch{Sequence: &added.Sequence, Attendees: &guests}); err != nil {
		t.Fatal(err)
	}
	if r, _ := h.answerIn(added.ID, maya); r != responseDeclined {
		t.Fatalf("added while away: %q, want declined", r)
	}
}

// "all" also declines the invitations already there; "new" leaves them.
func TestOutOfOfficeAllDeclinesExistingInvitations(t *testing.T) {
	h := newHarness(t)
	sam, maya, lee := h.member("Sam"), h.member("Maya"), h.member("Lee")
	before := h.oooMeeting(sam, "Planning", oooAt(21, 10), maya, lee)
	h.away(lee, oooAt(20, 0), oooAt(23, 0), "new", "")
	if r, _ := h.answerIn(before.ID, lee); r != responseNeedsAction {
		t.Fatalf("new leaves what was there: %q", r)
	}
	h.away(maya, oooAt(20, 0), oooAt(23, 0), "all", "On leave.")
	if r, note := h.answerIn(before.ID, maya); r != responseDeclined || note != "On leave." {
		t.Fatalf("all declines what was there: %q %q", r, note)
	}
}

// Refusals: a rule only on out of office; someone's away never declines an
// event they organise, an answer they gave, or for anyone else; no rule, no
// decline; a message too long is refused.
func TestOutOfOfficeDeclinesOnlyForItsOwnerAsGuest(t *testing.T) {
	h := newHarness(t)
	sam, maya := h.member("Sam"), h.member("Maya")
	st, en := timed(oooAt(21, 10), 30, "UTC")
	if _, err := h.s.Create(context.Background(), sam, EventInput{Title: "Sync", Start: st, End: en, OutOfOffice: &OOOInput{Decline: "new"}}); err == nil {
		t.Fatal("a plain event can't decline invitations")
	}
	long := make([]rune, 501)
	for i := range long {
		long[i] = 'a'
	}
	from, to := oooAt(20, 0), oooAt(23, 0)
	if _, err := h.s.Create(context.Background(), maya, EventInput{Kind: EventAway, Title: "Out", Start: When{At: &from, Zone: "UTC"}, End: When{At: &to},
		OutOfOffice: &OOOInput{Decline: "new", Message: string(long)}}); err == nil {
		t.Fatal("a 501-character message should be refused")
	}

	mine := h.away(maya, from, to, "new", "Away.")
	if got, _ := h.s.Get(context.Background(), maya, mine.ID); got == nil || got.OutOfOffice == nil || got.OutOfOffice.Message != "Away." {
		t.Fatalf("Maya reads her rule back: %+v", got)
	}
	if got, _ := h.s.Get(context.Background(), sam, mine.ID); got != nil && got.OutOfOffice != nil {
		t.Fatal("Sam never reads Maya's rule")
	}
	own := h.oooMeeting(maya, "Maya's own", oooAt(21, 9), sam)
	if r, _ := h.answerIn(own.ID, sam); r != responseNeedsAction {
		t.Fatalf("Maya's away declined for Sam: %q", r)
	}

	// No rule: away declines nothing.
	lee := h.member("Lee")
	h.away(lee, from, to, "none", "")
	plain := h.oooMeeting(sam, "Review", oooAt(21, 15), lee)
	if r, _ := h.answerIn(plain.ID, lee); r != responseNeedsAction {
		t.Fatalf("away with no rule declined: %q", r)
	}
	if rules, _ := h.store.oooRules(context.Background(), nil); len(rules) != 0 {
		t.Fatalf("no ids, no rules: %v", rules)
	}
	_ = models.OOODeclineNew
}

// A weekly series and a week away (CR-68 MF1): "all" declines the one
// occurrence inside the away, and the next week's stays to answer; a new
// series that starts while away is left to answer, never declined whole.
func TestOutOfOfficeDeclinesOneOccurrenceOfASeries(t *testing.T) {
	h := newHarness(t)
	sam, maya := h.member("Sam"), h.member("Maya")
	ctx := context.Background()
	st, en := timed(oooAt(21, 10), 30, "UTC")
	_, err := h.s.Create(ctx, sam, EventInput{Title: "Weekly", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=WEEKLY;COUNT=4"},
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil {
		t.Fatal(err)
	}
	h.away(maya, oooAt(20, 0), oooAt(23, 0), "all", "On leave.")

	answers := func() map[string]string {
		evs, _, err := h.s.Range(ctx, maya, oooAt(19, 0), oooAt(19, 0).AddDate(0, 0, 28))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, e := range evs {
			if e.Title != "Weekly" {
				continue
			}
			for _, a := range e.Attendees {
				if a.UUID == maya.UserDgraphInfo.Uuid && e.Start.At != nil {
					out[e.Start.At.UTC().Format("01-02")] = a.Response
				}
			}
		}
		return out
	}
	got := answers()
	if got["10-21"] != responseDeclined || got["10-28"] != responseNeedsAction || len(got) != 4 {
		t.Fatalf("Maya's answers by week: %v, want 10-21 declined and the rest to answer", got)
	}

	// A new series that starts while away is left to answer, whole.
	st2, en2 := timed(oooAt(22, 15), 30, "UTC")
	s2, err := h.s.Create(ctx, sam, EventInput{Title: "Standup", Start: st2, End: en2, Recurrence: &Recurrence{Rule: "FREQ=DAILY;COUNT=5"},
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := h.answerIn(s2.ID, maya); r == responseDeclined {
		t.Fatal("a new series was declined whole")
	}
}

// A decline made while out of office reads "Out of office" to the organiser
// and the other guests (head 5c, gcal-9b): Maya's, declined by her rule, is
// marked; Lee's (not away) isn't, and neither is one from someone away whose
// time off declines nothing.
func TestOutOfOfficeDeclineIsMarkedForEveryoneWhoReadsIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, lee, kim := h.member("Sam"), h.member("Maya"), h.member("Lee"), h.member("Kim")
	h.away(maya, oooAt(20, 0), oooAt(23, 0), "new", "Away until Friday, ask Lee.")
	h.away(kim, oooAt(20, 0), oooAt(23, 0), "", "")
	e := h.oooMeeting(sam, "Launch sync", oooAt(21, 10), maya, lee, kim)
	for _, who := range []*model.UserInfo{lee, kim} {
		if _, err := h.s.Answer(ctx, who, e.ID, AnswerInput{Answer: Declined}); err != nil {
			t.Fatal(err)
		}
	}
	for _, reader := range []*model.UserInfo{sam, lee} {
		got, err := h.s.Get(ctx, reader, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		marked := map[string]bool{}
		for _, g := range got.Attendees {
			marked[g.UUID] = g.OutOfOffice
		}
		if !marked[maya.UserDgraphInfo.Uuid] || marked[lee.UserDgraphInfo.Uuid] || marked[kim.UserDgraphInfo.Uuid] {
			t.Fatalf("read by %s: marked %v, want Maya alone", reader.UserDgraphInfo.Uuid, marked)
		}
		// The web's read (the /event routes) says it too.
		old, err := h.s.legacyGet(ctx, reader, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		was := map[string]bool{}
		for _, g := range old.Guests {
			was[g.UserUUID] = g.OutOfOffice
		}
		if !was[maya.UserDgraphInfo.Uuid] || was[lee.UserDgraphInfo.Uuid] || was[kim.UserDgraphInfo.Uuid] {
			t.Fatalf("legacy read by %s: marked %v, want Maya alone", reader.UserDgraphInfo.Uuid, was)
		}
	}

	// An event after the away: Maya declines it herself, and it isn't marked.
	later := h.oooMeeting(sam, "Retro", oooAt(24, 10), maya)
	if _, err := h.s.Answer(ctx, maya, later.ID, AnswerInput{Answer: Declined}); err != nil {
		t.Fatal(err)
	}
	got, err := h.s.Get(ctx, sam, later.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got.Attendees {
		if g.OutOfOffice {
			t.Fatalf("after the away: %+v marked", g)
		}
	}
}
