package Calendar

// Proposing another time (calendar head 4, gcal-4) over the store in memory:
// a guest proposes with their answer; whoever may change the event sees it,
// and no other guest does; it stands until the event's time changes; a
// series takes one per event; and only whoever may change the event
// dismisses it. The SQL is held to the same answers by
// tests/integration/calendar_proposals_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// proposing is another time, from start for minutes.
func proposing(start time.Time, minutes int) *ProposalInput {
	end := start.Add(time.Duration(minutes) * time.Minute)
	return &ProposalInput{Start: &start, End: &end}
}

// proposalSeen is what reader sees of name's proposal on event id.
func (h *harness) proposalSeen(reader *model.UserInfo, id, name string) *Proposal {
	h.t.Helper()
	v, err := h.s.Get(context.Background(), reader, id)
	if err != nil {
		h.t.Fatal(err)
	}
	a := answerFor(v, name)
	if a == nil {
		h.t.Fatalf("%s isn't on the event", name)
	}
	return a.Proposal
}

// The proposer and whoever may change the event see a proposal, in both of
// the calendar's lists; another guest doesn't, until the organiser lets
// guests change the event. The organiser hears of it in words.
func TestAGuestProposesAnotherTimeAndWhoMayChangeItSeesIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	e := h.create(sam, "Design review", maya, jonas)
	at := time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)
	got, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Declined, Note: noteText("I'm out at nine"), Proposal: proposing(at, 30)})
	if err != nil {
		t.Fatal(err)
	}
	if p := answerFor(got, "Maya").Proposal; p == nil || !p.Start.Equal(at) || !p.End.Equal(at.Add(30*time.Minute)) {
		t.Errorf("Maya's own proposal, to her: %+v", p)
	}
	if p := h.proposalSeen(sam, e.ID, "Maya"); p == nil || !p.Start.Equal(at) {
		t.Errorf("Maya's proposal, to the organiser: %+v", p)
	}
	if p := h.proposalSeen(jonas, e.ID, "Maya"); p != nil {
		t.Errorf("another guest sees Maya's proposal: %+v", p)
	}
	from, to := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		who  *model.UserInfo
		sees bool
	}{{sam, true}, {maya, true}, {jonas, false}} {
		legacy, err := h.s.legacyRange(ctx, c.who, from, to)
		if err != nil || len(legacy) != 1 {
			t.Fatalf("the legacy list: %+v %v", legacy, err)
		}
		for _, g := range legacy[0].Guests {
			if g.UserUUID == maya.UserDgraphInfo.Uuid && (g.Proposal != nil) != c.sees {
				t.Errorf("the legacy list to %s: Maya's proposal %+v", c.who.UserDgraphInfo.UserName, g.Proposal)
			}
		}
	}
	yes := true
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), GuestsCanModify: &yes}); err != nil {
		t.Fatal(err)
	}
	if p := h.proposalSeen(jonas, e.ID, "Maya"); p == nil {
		t.Error("a guest who may change the event doesn't see the proposal")
	}

	told := h.tells.of(toldAnswered)
	if len(told) != 1 || told[0].answer.proposal == nil || !told[0].answer.proposal.Start.Equal(at) {
		t.Fatalf("the organiser told: %+v", told)
	}
	notices := captureNotices(t)
	h.s.tellMembers(ctx, maya, told[0], true)
	c := (*notices)[0]
	if c.n.Title != "Maya proposed a new time for Design review" ||
		c.n.Body != "Fri 16 Oct, 11:00–11:30 instead of Fri 16 Oct, 09:15–09:30 · “I'm out at nine”" ||
		c.n.Data["proposed_start"] != "2026-10-16T11:00:00Z" || c.n.Data["proposed_end"] != "2026-10-16T11:30:00Z" ||
		len(c.r) != 1 || c.r[0].UserUUID != sam.UserDgraphInfo.Uuid {
		t.Errorf("the proposal told %+v to %+v", c.n, c.r)
	}
	// The same proposal again tells nobody; a tap keeps it.
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Declined, Proposal: proposing(at, 30)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Tentative}); err != nil {
		t.Fatal(err)
	}
	if p := h.proposalSeen(sam, e.ID, "Maya"); p == nil {
		t.Error("a tap took Maya's proposal away")
	}
	if all := h.tells.of(toldAnswered); len(all) != 2 || all[1].answer.proposal != nil {
		t.Errorf("told again: %+v", all)
	}
	// {} takes it back.
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Tentative, Proposal: &ProposalInput{}}); err != nil {
		t.Fatal(err)
	}
	if p := h.proposalSeen(sam, e.ID, "Maya"); p != nil {
		t.Errorf("taken back, still seen: %+v", p)
	}
}

// A proposal stands while the event is still at the time it was proposed
// against: moving the event, the organiser's "Use this time" or any other
// change of its time, sets it aside.
func TestAProposalStandsUntilTheEventMoves(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.create(sam, "Design review", maya)
	at := time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Declined, Proposal: proposing(at, 15)}); err != nil {
		t.Fatal(err)
	}
	title := "Design review, round two"
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Title: &title}); err != nil {
		t.Fatal(err)
	}
	if p := h.proposalSeen(sam, e.ID, "Maya"); p == nil {
		t.Error("a new title set the proposal aside")
	}
	start, end := timed(at, 15, "Europe/London")
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(1), Start: &start, End: &end}); err != nil {
		t.Fatal(err)
	}
	if p := h.proposalSeen(sam, e.ID, "Maya"); p != nil {
		t.Errorf("the event moved to it, and the proposal still stands: %+v", p)
	}
	if p := h.proposalSeen(maya, e.ID, "Maya"); p != nil {
		t.Errorf("to Maya, after the move: %+v", p)
	}
}

// What can be proposed: a time with both ends, after it starts, still to
// come, not the event's own, for an event at a time that hasn't ended.
func TestAProposalIsATimeStillToCome(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.create(sam, "Design review", maya)
	at := time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)
	own := time.Date(2026, 10, 16, 9, 15, 0, 0, time.UTC)
	for name, in := range map[string]*ProposalInput{
		"no end":            {Start: &at},
		"ends as it starts": proposing(at, 0),
		"already passed":    proposing(time.Date(2026, 10, 15, 11, 0, 0, 0, time.UTC), 30),
		"years ahead":       proposing(time.Date(2029, 1, 1, 9, 0, 0, 0, time.UTC), 30),
		"its own time":      proposing(own, 15),
	} {
		var ie *EventInputError
		if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Declined, Proposal: in}); !errors.As(err, &ie) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A day takes none.
	day, err := h.s.Create(ctx, sam, EventInput{Title: "Offsite", Start: When{Date: "2026-10-20"}, End: When{Date: "2026-10-21"},
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil {
		t.Fatal(err)
	}
	var ie *EventInputError
	if _, err := h.s.Answer(ctx, maya, day.ID, AnswerInput{Answer: Declined, Proposal: proposing(at, 30)}); !errors.As(err, &ie) {
		t.Errorf("a day: %v", err)
	}
	// An event that has ended takes none.
	h.clock = time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Declined, Proposal: proposing(at, 30)}); !errors.As(err, &ie) {
		t.Errorf("an event that has ended: %v", err)
	}
	// The organiser has no invitation to answer, so nothing to propose with.
	if _, err := h.s.Answer(ctx, sam, e.ID, AnswerInput{Answer: Accepted, Proposal: proposing(at, 30)}); !errors.As(err, &ie) {
		t.Errorf("the organiser proposed: %v", err)
	}
}

// A series takes a proposal for one of its events, which stands on that
// event alone; one for the whole series is refused.
func TestASeriesTakesAProposalForOneOfItsEvents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya := h.member("Sam"), h.member("Maya")
	h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon)
	in := h.between(maya, from, to)
	at := time.Date(2026, 10, 27, 14, 0, 0, 0, lon)
	var ie *EventInputError
	if _, err := h.s.Answer(ctx, maya, in[1].ID, AnswerInput{Answer: Declined, Proposal: proposing(at, 15)}); !errors.As(err, &ie) {
		t.Errorf("a proposal for the whole series: %v", err)
	}
	if _, err := h.s.Answer(ctx, maya, in[1].ID, AnswerInput{Answer: Declined, Scope: ScopeThis, Proposal: proposing(at, 15)}); err != nil {
		t.Fatal(err)
	}
	for i, ev := range h.between(sam, from, to) {
		a := answerFor(&ev, "Maya")
		if (a.Proposal != nil) != (i == 1) {
			t.Errorf("instance %d: %+v", i, a.Proposal)
		}
	}
}

// Only whoever may change an event dismisses a proposal on it: a guest who
// may not is refused, and someone not on it finds nothing. The organiser
// dismisses it, and the guest's answer and note stay.
func TestOnlyWhoMayChangeAnEventDismissesAProposal(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas, eve := h.member("Sam"), h.member("Maya"), h.member("Jonas"), h.member("Eve")
	e := h.create(sam, "Design review", maya, jonas)
	at := time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Declined, Note: noteText("Clash"), Proposal: proposing(at, 15)}); err != nil {
		t.Fatal(err)
	}
	mayas := DismissInput{UserUUID: maya.UserDgraphInfo.Uuid}
	if _, err := h.s.DismissProposal(ctx, jonas, e.ID, mayas); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("another guest dismissed it: %v", err)
	}
	if _, err := h.s.DismissProposal(ctx, maya, e.ID, mayas); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("its guest dismissed it as the organiser would: %v", err)
	}
	if _, err := h.s.DismissProposal(ctx, eve, e.ID, mayas); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("someone not on the event: %v", err)
	}
	if p := h.proposalSeen(sam, e.ID, "Maya"); p == nil {
		t.Fatal("refused dismissals took it away")
	}
	got, err := h.s.DismissProposal(ctx, sam, e.ID, mayas)
	if err != nil {
		t.Fatal(err)
	}
	if a := answerFor(got, "Maya"); a.Proposal != nil || a.Response != responseDeclined || a.Note != "Clash" {
		t.Errorf("after the organiser dismissed it: %+v", a)
	}
	if _, err := h.s.DismissProposal(ctx, sam, e.ID, mayas); err != nil {
		t.Errorf("dismissed twice: %v", err)
	}
	var ie *EventInputError
	if _, err := h.s.DismissProposal(ctx, sam, e.ID, DismissInput{UserUUID: eve.UserDgraphInfo.Uuid}); !errors.As(err, &ie) {
		t.Errorf("someone who isn't a guest: %v", err)
	}
	if _, err := h.s.DismissProposal(ctx, sam, e.ID, DismissInput{UserUUID: "not-an-id"}); !errors.As(err, &ie) {
		t.Errorf("an id that isn't one: %v", err)
	}
}

// A guest invited by email proposes from their own page, for a single event:
// the organiser sees it, the page shows it to that guest alone, and a
// series' page takes none.
func TestAGuestByEmailProposesFromTheirPage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.withEmails(sam, "Intro call", []string{"Alex Kim <alex@example.org>"}, maya)
	alex := answerOf(h.store.guests[uuid.MustParse(e.ID)], models.GuestKey{Email: "alex@example.org"})
	raw, _ := helpers.DecryptSecret(alex.LinkSealed)
	v, err := h.s.invitation(ctx, raw, h.clock)
	if err != nil || !v.Proposable || v.You.Proposal != nil {
		t.Fatalf("the page: %+v %v", v, err)
	}
	at := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	if v, err = h.s.answerInvitation(ctx, raw, AnswerInput{Answer: Declined, Proposal: proposing(at, 45)}, h.clock); err != nil {
		t.Fatal(err)
	}
	if v.You.Proposal == nil || !v.You.Proposal.Start.Equal(at) {
		t.Errorf("the page, after proposing: %+v", v.You)
	}
	if p := h.proposalSeen(sam, e.ID, "alex@example.org"); p == nil || !p.Start.Equal(at) {
		t.Errorf("Alex's proposal, to the organiser: %+v", p)
	}
	if p := h.proposalSeen(maya, e.ID, "alex@example.org"); p != nil {
		t.Errorf("Alex's proposal, to another guest: %+v", p)
	}
	told := h.tells.of(toldAnswered)
	if len(told) != 1 || told[0].answer.proposal == nil || told[0].answer.key.Email != "alex@example.org" {
		t.Errorf("the organiser told: %+v", told)
	}
	// Another guest's page never shows it.
	for _, g := range v.Guests {
		if strings.Contains(g.Name, "Alex") && g.Response == "" {
			t.Errorf("a guest on the page: %+v", g)
		}
	}
}
