package Calendar

import (
	"context"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// Calendar head C, "Away, finished" (gcal-9b): an out of office declines the
// occurrences of a new series that start while it runs, one by one, and the
// ones a change for all or following events brings it into; and its mark is
// the record of the decline it made, never inferred from the times.

// weeklyAnswers is who's answer to each occurrence of the events titled
// title in [from, to), by the occurrence's UTC date ("01-02").
func (h *harness) weeklyAnswers(who, guest *model.UserInfo, title string, from, to time.Time) map[string]string {
	h.t.Helper()
	evs, _, err := h.s.Range(context.Background(), who, from, to)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range evs {
		if e.Title != title || e.Start.At == nil {
			continue
		}
		for _, a := range e.Attendees {
			if a.UUID == guest.UserDgraphInfo.Uuid {
				out[e.Start.At.UTC().Format("01-02")] = a.Response
			}
		}
	}
	return out
}

// A new series whose occurrences start while a guest is away: those inside
// the away are declined, each alone with the away's message; the series
// itself and the occurrences after it stay to answer.
func TestOutOfOfficeDeclinesANewSeriesOccurrencesInsideIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	h.away(maya, oooAt(20, 0), oooAt(23, 0), "new", "On leave, back Friday.")
	st, en := timed(oooAt(21, 15), 30, "UTC")
	s, err := h.s.Create(ctx, sam, EventInput{Title: "Standup", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=DAILY;COUNT=4"},
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil {
		t.Fatal(err)
	}
	got := h.weeklyAnswers(maya, maya, "Standup", oooAt(19, 0), oooAt(26, 0))
	want := map[string]string{"10-21": responseDeclined, "10-22": responseDeclined, "10-23": responseNeedsAction, "10-24": responseNeedsAction}
	for day, r := range want {
		if got[day] != r {
			t.Fatalf("Maya's answers by day: %v, want %v", got, want)
		}
	}
	if r, _ := h.answerIn(s.ID, maya); r == responseDeclined {
		t.Fatal("the series was declined whole")
	}
	for _, a := range h.store.answered {
		if a.Note != "On leave, back Friday." || !a.ByAway {
			t.Fatalf("an occurrence's decline: note %q, by away %v; want the away's message, recorded as its", a.Note, a.ByAway)
		}
	}
}

// A guest added for all events, or for this and following, of a series
// already there: the occurrences inside their away are declined as for a
// new invitation.
func TestOutOfOfficeDeclinesWhenAChangeBringsThemIn(t *testing.T) {
	for _, scope := range []string{ScopeAll, ScopeFollowing} {
		t.Run(scope, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			sam, maya := h.member("Sam"), h.member("Maya")
			st, en := timed(oooAt(19, 15), 30, "UTC")
			if _, err := h.s.Create(ctx, sam, EventInput{Title: "Standup", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=DAILY;COUNT=6"}}); err != nil {
				t.Fatal(err)
			}
			h.away(maya, oooAt(21, 0), oooAt(23, 0), "new", "On leave.")
			in := h.between(sam, oooAt(19, 0), oooAt(26, 0))
			guests := []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}
			if _, err := h.s.Update(ctx, sam, in[1].ID, EventPatch{Sequence: &in[1].Sequence, Attendees: &guests, Scope: scope}); err != nil {
				t.Fatal(err)
			}
			got := h.weeklyAnswers(maya, maya, "Standup", oooAt(19, 0), oooAt(26, 0))
			if got["10-21"] != responseDeclined || got["10-22"] != responseDeclined || got["10-23"] != responseNeedsAction || got["10-20"] != responseNeedsAction {
				t.Fatalf("Maya's answers by day after a change for %s: %v, want 10-21 and 10-22 declined, 10-20 and 10-23 to answer", scope, got)
			}
		})
	}
}

// Only a decline the away made reads "Out of office": one Maya made by hand
// before she set her away, inside its time, doesn't (CR-68's SF on the
// inferred mark), and answering again by hand clears the record.
func TestOutOfOfficeMarkIsTheRecordOfItsDecline(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	byHand := h.oooMeeting(sam, "Copy sync", oooAt(21, 11), maya)
	dentist, still := "Clashes with the dentist.", "Still away."
	if _, err := h.s.Answer(ctx, maya, byHand.ID, AnswerInput{Answer: Declined, Note: &dentist}); err != nil {
		t.Fatal(err)
	}
	h.away(maya, oooAt(20, 0), oooAt(23, 0), "new", "On leave.")
	byAway := h.oooMeeting(sam, "Launch sync", oooAt(21, 10), maya)
	marked := func(id string) bool {
		t.Helper()
		got, err := h.s.Get(ctx, sam, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range got.Attendees {
			if g.UUID == maya.UserDgraphInfo.Uuid {
				return g.OutOfOffice
			}
		}
		return false
	}
	if !marked(byAway.ID) || marked(byHand.ID) {
		t.Fatalf("marked: by the away %v, by hand %v; want the away's alone", marked(byAway.ID), marked(byHand.ID))
	}
	if _, err := h.s.Answer(ctx, maya, byAway.ID, AnswerInput{Answer: Declined, Note: &still}); err != nil {
		t.Fatal(err)
	}
	if marked(byAway.ID) {
		t.Fatal("a decline Maya made again by hand still reads as her away's")
	}
}

// CR-B1 P1 (M1, M2): Maya is away 20 to 25 Oct; at 22 Oct 08:00 Sam adds her
// to a daily standup for all events. Only the days from now inside the away
// are declined (22 to 24; 20 and 21 are past), and Sam is told once, not
// once a day.
func TestOutOfOfficeDeclinesOnlyDaysAheadAndTellsOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	st, en := timed(oooAt(19, 9), 15, "UTC")
	if _, err := h.s.Create(ctx, sam, EventInput{Title: "Standup", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=DAILY;COUNT=10"}}); err != nil {
		t.Fatal(err)
	}
	h.away(maya, oooAt(20, 0), oooAt(25, 0), "new", "On leave.")
	h.clock = oooAt(22, 8)
	in := h.between(sam, oooAt(19, 0), oooAt(30, 0))
	h.tells.mu.Lock()
	before := len(h.tells.tells)
	h.tells.mu.Unlock()
	guests := []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}
	if _, err := h.s.Update(ctx, sam, in[0].ID, EventPatch{Sequence: &in[0].Sequence, Attendees: &guests, Scope: ScopeAll}); err != nil {
		t.Fatal(err)
	}
	got := h.weeklyAnswers(maya, maya, "Standup", oooAt(19, 0), oooAt(30, 0))
	for day, want := range map[string]string{"10-20": responseNeedsAction, "10-21": responseNeedsAction, "10-22": responseDeclined,
		"10-23": responseDeclined, "10-24": responseDeclined, "10-25": responseNeedsAction} {
		if got[day] != want {
			t.Fatalf("Maya's answers by day: %v; want %s %s", got, day, want)
		}
	}
	h.tells.mu.Lock()
	answered := 0
	for _, tl := range h.tells.tells[before:] {
		if tl.what == toldAnswered {
			answered++
		}
	}
	h.tells.mu.Unlock()
	if answered != 1 {
		t.Fatalf("Sam was told of Maya's away declines %d times, want once", answered)
	}
}

// CR-B1 M3a: two guests away at different times on one series are each
// declined only inside their own away.
func TestOutOfOfficeDeclinesEachGuestInsideTheirOwnAway(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, lee := h.member("Sam"), h.member("Maya"), h.member("Lee")
	h.away(maya, oooAt(20, 0), oooAt(22, 0), "new", "Maya away.")
	h.away(lee, oooAt(23, 0), oooAt(25, 0), "new", "Lee away.")
	st, en := timed(oooAt(19, 9), 15, "UTC")
	if _, err := h.s.Create(ctx, sam, EventInput{Title: "Standup", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=DAILY;COUNT=8"},
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}, {UUID: lee.UserDgraphInfo.Uuid}}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who      *model.UserInfo
		declined map[string]bool
	}{{maya, map[string]bool{"10-20": true, "10-21": true}}, {lee, map[string]bool{"10-23": true, "10-24": true}}} {
		got := h.weeklyAnswers(sam, c.who, "Standup", oooAt(19, 0), oooAt(27, 0))
		for day, r := range got {
			if (r == responseDeclined) != c.declined[day] {
				t.Fatalf("%s's answers by day: %v, want declined only on %v", c.who.UserDgraphInfo.UserName, got, c.declined)
			}
		}
	}
}
