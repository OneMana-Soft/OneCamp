package Calendar

// Calendar A2 over the store in memory: answers for an event, a series or one
// instance alone; guests invited by email, each once, with a link of their
// own; who is told of what, in which words and through which deliveries; the
// invitation emails; and nudges. The SQL is held to
// the same answers by tests/integration/calendar_attendance_test.go.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
)

// noteText is a note an answer sends.
func noteText(s string) *string { return &s }

// answerFor is someone's attendee entry on an event as reader sees it.
func answerFor(e *Event, name string) *Attendee {
	for i := range e.Attendees {
		if e.Attendees[i].Name == name || strings.EqualFold(e.Attendees[i].Email, name) {
			return &e.Attendees[i]
		}
	}
	return nil
}

// emailGuest makes who's event with guests by email (and members).
func (h *harness) withEmails(who *model.UserInfo, title string, emails []string, members ...*model.UserInfo) *Event {
	h.t.Helper()
	start, end := timed(time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC), 45, "Europe/London")
	in := EventInput{Title: title, Start: start, End: end}
	for _, m := range members {
		in.Attendees = append(in.Attendees, GuestInput{UUID: m.UserDgraphInfo.Uuid})
	}
	for _, e := range emails {
		in.Attendees = append(in.Attendees, GuestInput{Email: e})
	}
	e, err := h.s.Create(context.Background(), who, in)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// A guest answers yes, no or maybe with a note and stays on the guest list;
// the answer is theirs, not a change to the event, so its sequence stays.
func TestAGuestAnswersAndStaysOnTheList(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	e := h.create(sam, "Design review", maya)
	got, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Tentative, Note: noteText(" Ten minutes late ")})
	if err != nil {
		t.Fatal(err)
	}
	if a := answerFor(got, "Maya"); a == nil || a.Response != responseTentative || a.Note != "Ten minutes late" || a.RespondedAt == nil {
		t.Fatalf("Maya's answer as she sees it: %+v", a)
	}
	// An answer that leaves its note out (a tap on the card) keeps it.
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Declined}); err != nil {
		t.Fatal(err)
	}
	seen, err := h.s.Get(ctx, sam, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a := answerFor(seen, "Maya"); a == nil || a.Response != responseDeclined || a.Note != "Ten minutes late" {
		t.Errorf("Sam sees Maya %+v", a)
	}
	if seen.Sequence != e.Sequence {
		t.Errorf("an answer moved the sequence: %d, was %d", seen.Sequence, e.Sequence)
	}
	// One that sends an empty note clears it.
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Declined, Note: noteText("")}); err != nil {
		t.Fatal(err)
	}
	if seen, err = h.s.Get(ctx, sam, e.ID); err != nil {
		t.Fatal(err)
	}
	if a := answerFor(seen, "Maya"); a == nil || a.Note != "" {
		t.Errorf("an empty note left %+v", a)
	}
	if _, err := h.s.Get(ctx, maya, e.ID); err != nil {
		t.Errorf("Maya said no and lost the event: %v", err)
	}
	for name, c := range map[string]struct {
		who *model.UserInfo
		in  AnswerInput
	}{
		"the organiser": {sam, AnswerInput{Answer: Accepted}},
		"a stranger":    {jonas, AnswerInput{Answer: Accepted}},
		"no answer":     {maya, AnswerInput{Answer: "perhaps"}},
		"a long note":   {maya, AnswerInput{Answer: Accepted, Note: noteText(strings.Repeat("x", 501))}},
		"a bad scope":   {maya, AnswerInput{Answer: Accepted, Scope: "following"}},
	} {
		_, err := h.s.Answer(ctx, c.who, e.ID, c.in)
		var input *EventInputError
		if !errors.As(err, &input) && !errors.Is(err, ErrEventNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := h.s.Answer(ctx, jonas, e.ID, AnswerInput{Answer: Accepted}); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("a stranger is told nothing's there: %v", err)
	}
	// The organiser heard of each answer once: the same one again tells nobody.
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Declined}); err != nil {
		t.Fatal(err)
	}
	// Told: maybe with a note, no (the note kept), the note cleared.
	told := h.tells.of(toldAnswered)
	if len(told) != 3 || told[0].answer.response != responseTentative || told[1].answer.response != responseDeclined ||
		told[1].answer.note != "Ten minutes late" || told[2].answer.note != "" {
		t.Errorf("answers told: %+v", told)
	}
	if n := countKind(h, resourceEventBusiness.CalendarEventRSVP); n != 4 {
		t.Errorf("%d event.rsvp announced, want one per answer (4)", n)
	}
}

func countKind(h *harness, k resourceEventBusiness.Kind) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, e := range h.emitted {
		if e.Kind == k {
			n++
		}
	}
	return n
}

// An answer for one instance of a series is that instance's alone; one for
// all of it clears it.
func TestAnAnswerForOneInstanceIsThatInstancesAlone(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya := h.member("Sam"), h.member("Maya")
	h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon)
	in := h.between(maya, from, to)
	if _, err := h.s.Answer(ctx, maya, in[0].ID, AnswerInput{Answer: Accepted}); err != nil {
		t.Fatal(err)
	}
	got, err := h.s.Answer(ctx, maya, in[1].ID, AnswerInput{Answer: Declined, Note: noteText("Dentist"), Scope: ScopeThis})
	if err != nil {
		t.Fatal(err)
	}
	if a := answerFor(got, "Maya"); a == nil || a.Response != responseDeclined {
		t.Errorf("the instance answered: %+v", a)
	}
	want := []string{responseAccepted, responseDeclined, responseAccepted}
	for i, e := range h.between(sam, from, to) {
		if a := answerFor(&e, "Maya"); a == nil || a.Response != want[i] {
			t.Errorf("instance %d: Sam sees Maya %+v, want %s", i, a, want[i])
		}
	}
	// Legacy JSON says it too, per instance.
	legacy, err := h.s.legacyRange(ctx, sam, from, to)
	if err != nil || len(legacy) != 3 || len(legacy[1].Guests) != 1 || legacy[1].Guests[0].Response != responseDeclined || legacy[1].Guests[0].Note != "Dentist" {
		t.Errorf("the legacy list: %+v %v", legacy, err)
	}
	// A declined instance doesn't make Maya busy; the others do.
	busy, err := h.s.busyEvents(ctx, maya.UserDgraphInfo.Uuid, from, to)
	if err != nil || len(busy) != 2 {
		t.Errorf("Maya is busy for %d instances, want 2: %v", len(busy), err)
	}
	// A tap on the instance's card, with no note, keeps the instance's note.
	if got, err = h.s.Answer(ctx, maya, in[1].ID, AnswerInput{Answer: Tentative, Scope: ScopeThis}); err != nil {
		t.Fatal(err)
	}
	if a := answerFor(got, "Maya"); a == nil || a.Response != responseTentative || a.Note != "Dentist" {
		t.Errorf("the instance's note after a tap: %+v", a)
	}
	// Yes to all of it clears the one instance's answer, its note with it:
	// the series' own note (none) stands.
	if _, err := h.s.Answer(ctx, maya, in[2].ID, AnswerInput{Answer: Accepted}); err != nil {
		t.Fatal(err)
	}
	for i, e := range h.between(sam, from, to) {
		if a := answerFor(&e, "Maya"); a == nil || a.Response != responseAccepted || a.Note != "" {
			t.Errorf("after all: instance %d %+v", i, a)
		}
	}
}

// Guests by email: each once, an address that is a member's is the member,
// the organiser never a guest of their own event, and each guest by email
// with a link of their own. Only the organiser is told whether each was sent.
func TestGuestsByEmailAreInvitedOnceWithLinksOfTheirOwn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	h.store.addresses["maya@acme.example"] = personOf(maya)
	h.store.addresses["sam@acme.example"] = personOf(sam)
	if _, err := h.s.Create(ctx, sam, EventInput{Title: "Nope", Start: When{At: ptrTime(h.clock), Zone: "UTC"}, End: When{At: ptrTime(h.clock.Add(time.Hour))},
		Attendees: []GuestInput{{Email: "not an address"}}}); err == nil || !strings.Contains(err.Error(), "isn't an email address") {
		t.Errorf("a bad address: %v", err)
	}
	e := h.withEmails(sam, "Intro call", []string{"Alex Kim <ALEX@example.org>", "alex@example.org", "Maya@ACME.example", "sam@acme.example", "jo@example.org"})
	if n := len(e.Attendees); n != 3 {
		t.Fatalf("%d guests, want Alex, Maya and Jo: %+v", n, e.Attendees)
	}
	if a := answerFor(e, "alex@example.org"); a == nil || a.Name != "Alex Kim" || a.UUID != "" || a.Response != responseNeedsAction {
		t.Errorf("Alex: %+v", a)
	}
	if a := answerFor(e, "Maya"); a == nil || a.UUID != maya.UserDgraphInfo.Uuid || a.Email != "" {
		t.Errorf("Maya's address made her the guest: %+v", a)
	}
	rows := h.store.guests[uuid.MustParse(e.ID)]
	var hashes [][]byte
	for _, g := range rows {
		if g.UserID != nil {
			if len(g.LinkHash) != 0 || g.LinkSealed != "" {
				t.Errorf("a member got a link: %+v", g)
			}
			continue
		}
		raw, err := helpers.DecryptSecret(g.LinkSealed)
		if err != nil || !bytes.Equal(helpers.LinkSecretHash(raw), g.LinkHash) {
			t.Errorf("%s's link doesn't unseal to its hash: %v", g.Email, err)
		}
		for _, other := range hashes {
			if bytes.Equal(other, g.LinkHash) {
				t.Error("two guests share a link")
			}
		}
		hashes = append(hashes, g.LinkHash)
	}
	// Guests don't hear what was sent; the organiser does.
	if err := h.store.setInviteState(ctx, uuid.MustParse(e.ID), "jo@example.org", models.InviteNotSent); err != nil {
		t.Fatal(err)
	}
	mine, _ := h.s.Get(ctx, sam, e.ID)
	theirs, _ := h.s.Get(ctx, maya, e.ID)
	if a := answerFor(mine, "jo@example.org"); a == nil || a.Invite != models.InviteNotSent {
		t.Errorf("the organiser: %+v", a)
	}
	if a := answerFor(theirs, "jo@example.org"); a == nil || a.Invite != "" {
		t.Errorf("a guest is told what was sent: %+v", a)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// Taking a guest by email off ends their link at once, and keeping the list
// keeps their answer.
func TestTakingAGuestByEmailOffEndsTheirLink(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam := h.member("Sam")
	e := h.withEmails(sam, "Intro call", []string{"alex@example.org", "jo@example.org"})
	alex := answerOf(h.store.guests[uuid.MustParse(e.ID)], models.GuestKey{Email: "alex@example.org"})
	raw, _ := helpers.DecryptSecret(alex.LinkSealed)
	if _, err := h.s.answerInvitation(ctx, raw, AnswerInput{Answer: Accepted, Note: noteText("See you there")}, h.clock); err != nil {
		t.Fatal(err)
	}
	keep := []GuestInput{{Email: "ALEX@example.org"}}
	got, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Attendees: &keep})
	if err != nil {
		t.Fatal(err)
	}
	if a := answerFor(got, "alex@example.org"); len(got.Attendees) != 1 || a == nil || a.Response != responseAccepted || a.Note != "See you there" {
		t.Errorf("Alex kept, answer and all: %+v", got.Attendees)
	}
	jo := answerOf(h.store.guests[uuid.MustParse(e.ID)], models.GuestKey{Email: "jo@example.org"})
	if jo != nil {
		t.Errorf("Jo is still on it: %+v", jo)
	}
	keep = nil
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(1), Attendees: &keep}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.invitation(ctx, raw, h.clock); !errors.Is(err, ErrInvitationGone) {
		t.Errorf("Alex's link after he was taken off: %v", err)
	}
}

// A guest's page: their answer from their link, for the whole event; the
// other guests by name only when guests may see the list, and never an
// address; the same "not available" for every link that can't be answered.
func TestAGuestAnswersFromTheirLink(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.withEmails(sam, "Intro call", []string{"Alex Kim <alex@example.org>", "jo@example.org"}, maya)
	alex := answerOf(h.store.guests[uuid.MustParse(e.ID)], models.GuestKey{Email: "alex@example.org"})
	raw, _ := helpers.DecryptSecret(alex.LinkSealed)
	v, err := h.s.invitation(ctx, raw, h.clock)
	if err != nil {
		t.Fatal(err)
	}
	if v.Title != "Intro call" || v.Organiser != "Sam" || v.You.Email != "alex@example.org" || v.You.Response != responseNeedsAction {
		t.Errorf("the page: %+v", v)
	}
	names := []string{}
	for _, g := range v.Guests {
		names = append(names, g.Name)
		if strings.Contains(g.Name, "@") {
			t.Errorf("an address on the page: %q", g.Name)
		}
	}
	if strings.Join(names, ",") != "Maya,jo" {
		t.Errorf("guests on the page: %v", names)
	}
	v, err = h.s.answerInvitation(ctx, raw, AnswerInput{Answer: Accepted, Note: noteText("Thanks"), Scope: ScopeThis}, h.clock)
	if err != nil || v.You.Response != responseAccepted || v.You.Note != "Thanks" {
		t.Fatalf("answered: %+v %v", v, err)
	}
	told := h.tells.of(toldAnswered)
	if len(told) != 1 || told[0].answer.key.Email != "alex@example.org" {
		t.Errorf("the organiser told: %+v", told)
	}
	// Guests hidden: nobody else on the page.
	hide := false
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), GuestsCanSeeGuests: &hide}); err != nil {
		t.Fatal(err)
	}
	if v, err := h.s.invitation(ctx, raw, h.clock); err != nil || len(v.Guests) != 0 {
		t.Errorf("hidden list: %+v %v", v, err)
	}
	for name, secret := range map[string]string{"unknown": "nope", "empty": "", "long": strings.Repeat("a", 200)} {
		if _, err := h.s.invitation(ctx, secret, h.clock); !errors.Is(err, ErrInvitationGone) {
			t.Errorf("%s link: %v", name, err)
		}
	}
	if _, err := h.s.invitation(ctx, raw, h.clock.Add(60*24*time.Hour)); !errors.Is(err, ErrInvitationGone) {
		t.Errorf("a link long after the meeting: %v", err)
	}
	if err := h.s.Delete(ctx, sam, e.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.answerInvitation(ctx, raw, AnswerInput{Answer: Declined}, h.clock); !errors.Is(err, ErrInvitationGone) {
		t.Errorf("a deleted event's link: %v", err)
	}
}

// capturedNotices are the notices a tell sends, for one test.
func captureNotices(t *testing.T) *[]capturedNotice {
	t.Helper()
	var mu sync.Mutex
	var got []capturedNotice
	old := tellNotice
	t.Cleanup(func() { tellNotice = old })
	tellNotice = func(_ context.Context, n notificationBusiness.Notice, r []notificationBusiness.NoticeRecipient) []string {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, capturedNotice{n, r})
		return nil
	}
	return &got
}

type capturedNotice struct {
	n notificationBusiness.Notice
	r []notificationBusiness.NoticeRecipient
}

// Members hear of a change once, what changed in words on their own clock,
// each in their own zone; Don't send keeps it in the inbox.
func TestGuestsHearWhatChangedOnTheirOwnClock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	h.zone(maya, "Asia/Kolkata")
	h.zone(jonas, "Europe/London")
	e := h.create(sam, "Design review", maya, jonas)
	got := captureNotices(t)
	for _, tl := range h.tells.of(toldMade) {
		h.s.tellMembers(ctx, sam, tl, true)
	}
	if len(*got) != 2 {
		t.Fatalf("invitations in %d zones, want 2: %+v", len(*got), *got)
	}
	for _, c := range *got {
		if c.n.Kind != noticeInvited || c.n.Title != "Sam invited you to Design review" || c.n.Path != "/app/calendar/event/"+e.ID || c.r[0].InboxOnly {
			t.Errorf("invitation %+v to %+v", c.n, c.r)
		}
	}
	*got = nil
	start, end := timed(time.Date(2026, 10, 16, 10, 15, 0, 0, time.UTC), 15, "Europe/London")
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Start: &start, End: &end}); err != nil {
		t.Fatal(err)
	}
	changed := h.tells.of(toldChanged)
	if len(changed) != 1 {
		t.Fatalf("%d changes told", len(changed))
	}
	h.s.tellMembers(ctx, sam, changed[0], true)
	bodies := map[string]string{}
	for _, c := range *got {
		bodies[c.r[0].UserUUID] = c.n.Body
		if c.n.Kind != noticeUpdated || c.n.Data["was_start"] == "" || c.n.Data["changed"] != "time" {
			t.Errorf("change %+v", c.n)
		}
	}
	if bodies[maya.UserDgraphInfo.Uuid] != "Now Fri 16 Oct, 15:45–16:00 · was Fri 16 Oct, 14:45–15:00" {
		t.Errorf("Maya (India) reads %q", bodies[maya.UserDgraphInfo.Uuid])
	}
	if bodies[jonas.UserDgraphInfo.Uuid] != "Now Fri 16 Oct, 11:15–11:30 · was Fri 16 Oct, 10:15–10:30" {
		t.Errorf("Jonas (London) reads %q", bodies[jonas.UserDgraphInfo.Uuid])
	}
	// Don't send: in the inbox alone.
	*got = nil
	h.s.tellMembers(ctx, sam, changed[0], false)
	for _, c := range *got {
		if !c.r[0].InboxOnly {
			t.Errorf("Don't send pushed %+v", c.r)
		}
	}
	// A colour isn't news to anyone.
	*got = nil
	hue := "sky"
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(1), Hue: &hue}); err != nil {
		t.Fatal(err)
	}
	all := h.tells.of(toldChanged)
	h.s.tellMembers(ctx, sam, all[len(all)-1], true)
	if len(*got) != 0 {
		t.Errorf("a colour told %+v", *got)
	}
}

// The organiser hears of an answer in their inbox, never by push (D-CAL-17),
// and by email too when they asked to be.
func TestTheOrganiserHearsOfAnswersInTheirInbox(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.create(sam, "Design review", maya)
	got := captureNotices(t)
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Accepted, Note: noteText("Ten minutes late")}); err != nil {
		t.Fatal(err)
	}
	h.s.tellMembers(ctx, maya, h.tells.of(toldAnswered)[0], true)
	c := (*got)[0]
	if c.n.Kind != noticeAnswered || c.n.Title != "Maya is going to Design review" || c.n.Body != "“Ten minutes late”" ||
		len(c.r) != 1 || c.r[0].UserUUID != sam.UserDgraphInfo.Uuid || !c.r[0].InboxOnly || c.n.Email != "" {
		t.Errorf("answer told %+v to %+v", c.n, c.r)
	}
	yes := true
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), EmailOnAnswers: &yes}); err != nil {
		t.Fatal(err)
	}
	*got = nil
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Declined}); err != nil {
		t.Fatal(err)
	}
	all := h.tells.of(toldAnswered)
	h.s.tellMembers(ctx, maya, all[len(all)-1], true)
	c = (*got)[0]
	if c.r[0].InboxOnly || !c.r[0].NoPush || c.n.Email != notificationBusiness.EventCalendarAnswered || c.n.Title != "Maya can't go to Design review" {
		t.Errorf("an organiser who asked for email: %+v %+v", c.n, c.r)
	}
	// Only the organiser asks for email on answers.
	if _, err := h.s.Update(ctx, maya, e.ID, EventPatch{Sequence: seq(1), EmailOnAnswers: &yes}); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("a guest set it: %v", err)
	}
}

// capturedMail is an email to a guest by email.
type capturedMail struct {
	to, subject, html string
	ics               string
}

func captureMail(t *testing.T, on bool) *[]capturedMail {
	t.Helper()
	var mu sync.Mutex
	var got []capturedMail
	oldOn, oldSend := outsidersMailed, sendGuestMail
	t.Cleanup(func() { outsidersMailed, sendGuestMail = oldOn, oldSend })
	outsidersMailed = func() bool { return on }
	sendGuestMail = func(_ context.Context, opt emailService.SendOptions) error {
		mu.Lock()
		defer mu.Unlock()
		m := capturedMail{to: opt.To, subject: opt.Subject, html: opt.HTML}
		for _, a := range opt.Attachments {
			m.ics = strings.ReplaceAll(string(a.Content), "\r\n ", "")
		}
		got = append(got, m)
		return nil
	}
	return &got
}

// A guest by email is sent a real invitation: the meeting's calendar file
// with them as its one attendee and no member's address in it, the answers
// in the body (never in the file), and a cancellation when taken off.
func TestAGuestByEmailIsSentARealInvitation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	h.people[sam.UserDgraphInfo.Uuid].EmailID = "sam@acme.example"
	h.people[maya.UserDgraphInfo.Uuid].EmailID = "maya@acme.example"
	mail := captureMail(t, true)
	e := h.withEmails(sam, "Intro call", []string{"Alex Kim <alex@example.org>"}, maya)
	h.s.tellOutsiders(ctx, h.tells.of(toldMade)[0], true)
	if len(*mail) != 1 {
		t.Fatalf("%d emails", len(*mail))
	}
	m := (*mail)[0]
	if m.to != "alex@example.org" || !strings.HasPrefix(m.subject, "Invitation: Intro call · ") {
		t.Errorf("mail %+v", m)
	}
	for _, want := range []string{"METHOD:REQUEST", "UID:" + e.ICalUID, "SEQUENCE:0", `ORGANIZER;CN="Sam":mailto:sam@acme.example`,
		`ATTENDEE;CN="Alex Kim";ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:alex@example.org`} {
		if !strings.Contains(m.ics, want) {
			t.Errorf("no %q in the file:\n%s", want, m.ics)
		}
	}
	if strings.Contains(m.ics, "maya@acme.example") || strings.Contains(m.ics, "/rsvp/") {
		t.Errorf("the file carries a member's address or the answer link:\n%s", m.ics)
	}
	if !strings.Contains(m.html, "/rsvp/") || !strings.Contains(m.html, "?answer=yes") || !strings.Contains(m.html, "invited you to this meeting from OneCamp") {
		t.Errorf("the body has no answers or no why:\n%s", m.html)
	}
	if g := answerOf(h.store.guests[uuid.MustParse(e.ID)], models.GuestKey{Email: "alex@example.org"}); g.InviteState != models.InviteSent {
		t.Errorf("state %q", g.InviteState)
	}
	// Taken off: told it's off their calendar.
	*mail = nil
	keep := []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Attendees: &keep}); err != nil {
		t.Fatal(err)
	}
	h.s.tellOutsiders(ctx, h.tells.of(toldChanged)[0], true)
	if len(*mail) != 1 || !strings.Contains((*mail)[0].ics, "METHOD:CANCEL") || !strings.HasPrefix((*mail)[0].subject, "Cancelled: ") {
		t.Errorf("the cancellation: %+v", *mail)
	}
}

// Where this server sends no such email (Cloud's essentials, the demo), the
// guest is recorded as not sent, for the organiser to share their link; and
// Don't send sends nothing.
func TestAnInvitationNotSentIsSaidSo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam := h.member("Sam")
	mail := captureMail(t, false)
	e := h.withEmails(sam, "Intro call", []string{"alex@example.org"})
	h.s.tellOutsiders(ctx, h.tells.of(toldMade)[0], true)
	if len(*mail) != 0 {
		t.Errorf("sent %+v", *mail)
	}
	if g := answerOf(h.store.guests[uuid.MustParse(e.ID)], models.GuestKey{Email: "alex@example.org"}); g.InviteState != models.InviteNotSent {
		t.Errorf("state %q", g.InviteState)
	}
	link, err := h.s.GuestLink(ctx, sam, e.ID, "ALEX@example.org")
	if err != nil || !strings.Contains(link, "/rsvp/") {
		t.Fatalf("the organiser's copy: %q %v", link, err)
	}
	secret := link[strings.LastIndex(link, "/")+1:]
	if _, err := h.s.invitation(ctx, secret, h.clock); err != nil {
		t.Errorf("the copied link opens nothing: %v", err)
	}
	// Don't send: nothing at all.
	mail2 := captureMail(t, true)
	h.s.tellOutsiders(ctx, h.tells.of(toldMade)[0], false)
	if len(*mail2) != 0 {
		t.Errorf("Don't send sent %+v", *mail2)
	}
}

// The organiser asks again those who haven't answered, each at most once a
// day; a guest can't.
func TestANudgeAsksOnlyThoseWhoHaventAnswered(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	e := h.withEmails(sam, "Intro call", []string{"alex@example.org"}, maya, jonas)
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Accepted}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Nudge(ctx, maya, e.ID); !errors.Is(err, ErrNotEventCreator) {
		t.Errorf("a guest nudged: %v", err)
	}
	n, err := h.s.Nudge(ctx, sam, e.ID)
	if err != nil || n != 2 {
		t.Fatalf("asked %d (want Jonas and Alex): %v", n, err)
	}
	if n, _ := h.s.Nudge(ctx, sam, e.ID); n != 0 {
		t.Errorf("asked again within the day: %d", n)
	}
	h.clock = h.clock.Add(25 * time.Hour)
	if n, _ := h.s.Nudge(ctx, sam, e.ID); n != 2 {
		t.Errorf("a day later: %d", n)
	}
	got := captureNotices(t)
	h.s.tellMembers(ctx, sam, h.tells.of(toldNudged)[0], true)
	if len(*got) != 1 || (*got)[0].n.Title != "Sam is waiting for your answer" || (*got)[0].n.Data["again"] != "true" ||
		(*got)[0].r[0].UserUUID != jonas.UserDgraphInfo.Uuid {
		t.Errorf("nudge %+v", *got)
	}
}

// "This and following" gives guests by email a link of their own on the new
// series; the old one still answers the old part.
func TestASplitGivesGuestsByEmailNewLinks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam := h.member("Sam")
	st, en := timed(time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, "Europe/London")
	s, err := h.s.Create(ctx, sam, EventInput{Title: "Stand-up", Start: st, End: en, Recurrence: &Recurrence{Rule: "FREQ=WEEKLY"},
		Attendees: []GuestInput{{Email: "alex@example.org"}}})
	if err != nil {
		t.Fatal(err)
	}
	old := answerOf(h.store.guests[uuid.MustParse(s.ID)], models.GuestKey{Email: "alex@example.org"})
	from, to := time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon)
	in := h.between(sam, from, to)
	later := "Stand-up (later)"
	fresh, err := h.s.Update(ctx, sam, in[1].ID, EventPatch{Sequence: &in[1].Sequence, Title: &later, Scope: ScopeFollowing})
	if err != nil {
		t.Fatal(err)
	}
	moved := answerOf(h.store.guests[uuid.MustParse(fresh.ID)], models.GuestKey{Email: "alex@example.org"})
	if moved == nil || len(moved.LinkHash) == 0 || bytes.Equal(moved.LinkHash, old.LinkHash) {
		t.Fatalf("the new series' guest: %+v", moved)
	}
	raw, _ := helpers.DecryptSecret(old.LinkSealed)
	if v, err := h.s.invitation(ctx, raw, h.clock); err != nil || v.Title != "Stand-up" {
		t.Errorf("the old link: %+v %v", v, err)
	}
	split := h.tells.of(toldChanged)
	if len(split) != 2 || !split[0].outsidersOnly || !split[1].splitTail || split[1].event.ID.String() != fresh.ID {
		t.Errorf("tellings of a split: %+v", split)
	}
}

// Words for notices and invitations. Pure.
func TestWhenWords(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	at := func(h, m int) *time.Time { t := time.Date(2026, 10, 16, h, m, 0, 0, time.UTC); return &t }
	for _, c := range []struct {
		e    *models.Event
		want string
	}{
		{&models.Event{StartAt: at(14, 0), EndAt: at(15, 0)}, "Fri 16 Oct, 10:00–11:00"},
		{&models.Event{StartAt: at(2, 0), EndAt: at(5, 0)}, "Thu 15 Oct 22:00 – Fri 16 Oct 01:00"},
		{&models.Event{StartDate: "2026-10-16", EndDate: "2026-10-17"}, "Fri 16 Oct"},
		{&models.Event{StartDate: "2026-10-16", EndDate: "2026-10-19"}, "Fri 16 Oct – Sun 18 Oct"},
	} {
		if got := whenWords(c.e, ny); got != c.want {
			t.Errorf("%q, want %q", got, c.want)
		}
	}
	for r, want := range map[string]string{responseAccepted: "is going to", responseTentative: "might go to", responseDeclined: "can't go to"} {
		if got := answerVerb(r); got != want {
			t.Errorf("%s: %q", r, got)
		}
	}
}
