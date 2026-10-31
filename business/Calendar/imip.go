package Calendar

// Invitations by email (iMIP, RFC 6047) to guests outside the workspace:
// what Gmail, Outlook and Apple Mail show as an invitation, the meeting's
// calendar file attached, with three answers that open the guest's own page
// (rsvp.go), no account needed.
//
//   - Invited, or the meeting changed for them: METHOD REQUEST with the whole
//     event as it stands (a series with its changed and skipped instances),
//     so their calendar matches it. Cancelled, or taken off it: CANCEL. Asked
//     again for an answer: the REQUEST again, saying so.
//   - Each message's SEQUENCE counts the seconds since the event was made, as
//     a booking's invites do (invite.go): every message is newer than the
//     last, whatever was written in between.
//   - The guest is the one ATTENDEE in their own message: no member's address
//     goes outside the workspace in a calendar file. The ORGANIZER is the
//     organiser, so a reply from the guest's own calendar reaches them.
//   - The answer link is the guest's credential: it is in the email body
//     alone, never in the calendar file, which is forwarded and shared far
//     more freely than an email.
//   - Sent through the workspace's email, under its rules (the daily cap, the
//     reserve for password resets; Cloud's essentials-only tier sends none,
//     owner question O2), and never from the public demo. What wasn't sent is
//     recorded on the guest (invite_state), and the organiser can copy the
//     guest's link instead.

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	"github.com/akashc777/OneCamp/business/ICS"
	recurrence "github.com/akashc777/OneCamp/business/Recurrence"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	authService "github.com/akashc777/OneCamp/services/Auth"
	emailService "github.com/akashc777/OneCamp/services/Email"
)

// Seams for tests: whether this server emails people outside the workspace,
// and where such an email goes.
var (
	outsidersMailed = func() bool { return emailService.NotificationEmailEnabled() && !helpers.DemoMode() }
	sendGuestMail   = func(ctx context.Context, opt emailService.SendOptions) error {
		_, err := emailService.SendEmailWithOptions(ctx, opt)
		return err
	}
)

// inviteKind is why an invitation is sent.
type inviteKind int

const (
	inviteNew         inviteKind = iota // put on the event
	inviteChanged                       // the meeting changed
	inviteAgain                         // asked again for an answer
	inviteCallAdded                     // the meeting changed: its call was added
	inviteCallRemoved                   // the meeting changed: its call was taken off
)

// maxNotesInMail is the most of an event's notes an invitation quotes.
const maxNotesInMail = 1500

// emailGuests are the guests by email among guests.
func emailGuests(guests []models.Attendee) []models.Attendee {
	var out []models.Attendee
	for _, g := range guests {
		if g.UserID == nil && g.Email != "" {
			out = append(out, g)
		}
	}
	return out
}

// tellOutsiders tells guests by email of a change, by iMIP.
func (s *service) tellOutsiders(ctx context.Context, t telling, send bool) {
	switch t.what {
	case toldMade:
		if send {
			s.invite(ctx, t.event, emailGuests(t.guestsAfter), inviteNew)
		}
	case toldChanged:
		if !send {
			return
		}
		var added, kept []models.Attendee
		before := emailsOf(t.guestsBefore)
		for _, g := range emailGuests(t.guestsAfter) {
			if containsFold(before, g.Email) {
				kept = append(kept, g)
			} else {
				added = append(added, g)
			}
		}
		var removed []models.Attendee
		after := emailsOf(t.guestsAfter)
		for _, g := range emailGuests(t.guestsBefore) {
			if !containsFold(after, g.Email) {
				removed = append(removed, g)
			}
		}
		s.invite(ctx, t.event, added, inviteNew)
		if toldAbout(t.changed) {
			s.invite(ctx, t.event, kept, changeKind(t.changed, t.event))
		}
		if !t.splitTail {
			s.cancelFor(ctx, t.event, removed)
		}
	case toldCancelled:
		if !send {
			return
		}
		if t.ref != "" && t.ref != t.event.ID.String() {
			// One instance cancelled: the series as it is now, without it.
			s.invite(ctx, t.event, emailGuests(t.guestsAfter), inviteChanged)
			return
		}
		s.cancelFor(ctx, t.event, emailGuests(t.guestsAfter))
	case toldNudged:
		s.invite(ctx, t.event, emailGuests(t.nudged), inviteAgain)
	}
}

func containsFold(list []string, s string) bool {
	for _, l := range list {
		if strings.EqualFold(l, s) {
			return true
		}
	}
	return false
}

// organiserAddress is an event's organiser as its invitations name them.
func (s *service) organiserAddress(ctx context.Context, e *models.Event) ICS.Address {
	a := ICS.Address{Name: "", Email: emailService.SenderAddress()}
	people, err := s.people(ctx, []string{e.CreatedBy.String()})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: an invitation's organiser couldn't be read: %v", err)
	}
	for _, p := range people {
		if p != nil && p.Uuid == e.CreatedBy.String() {
			a.Name = p.DisplayName()
			if p.EmailID != "" {
				a.Email = p.EmailID
			}
		}
	}
	if a.Name == "" {
		a.Name = "Someone"
	}
	return a
}

// changeKind is why a change's invitation is sent: the call added or taken
// off when that is the change (Add and Remove call), any other change
// otherwise. Pure.
func changeKind(changed []string, e *models.Event) inviteKind {
	if len(changed) != 1 || changed[0] != "call" {
		return inviteChanged
	}
	if e.CallRoom != "" {
		return inviteCallAdded
	}
	return inviteCallRemoved
}

// mailSequence is an invitation's SEQUENCE at now: 0 for the first, then
// the seconds since the event was made, so each is newer than the last. Pure.
func mailSequence(e *models.Event, kind inviteKind, now time.Time) int {
	if kind == inviteNew && e.Sequence == 0 {
		return 0
	}
	if s := int(now.Sub(e.CreatedAt) / time.Second); s > 1 {
		return s
	}
	return 1
}

// plainNotes is an event's notes as text, for an invitation's calendar file
// and email: as much as an email quotes.
func plainNotes(e *models.Event) string {
	notes := notesAsText(e)
	if len([]rune(notes)) > maxNotesInMail {
		notes = string([]rune(notes)[:maxNotesInMail]) + "…"
	}
	return notes
}

// notesAsText is an event's notes as text, all of them.
func notesAsText(e *models.Event) string {
	notes := strings.TrimSpace(e.Notes)
	if e.NotesFormat == notesHTML && notes != "" {
		doc, err := contentcodec.FromHTML(notes, contentcodec.HTMLOptions{Foreign: true})
		if err != nil {
			return ""
		}
		notes = strings.TrimSpace(contentcodec.PlainText(doc))
	}
	return notes
}

// calendarFile is the event as an iCalendar message for one guest: a single
// event, or a series with its changed instances, each its own VEVENT; the
// guest as its one attendee, with their answer. conference is the event's
// call's plain link (no key: a calendar file is shared and synced), "" for none.
func (s *service) calendarFile(ctx context.Context, method string, e *models.Event, guest models.Attendee, organiser ICS.Address, seq int, now time.Time, conference string) ([]byte, error) {
	attendee := ICS.Attendee{Address: ICS.Address{Name: guest.Name, Email: guest.Email}, Optional: guest.Role == roleOptional,
		PartStat: partStatOf(guest.Response), RSVP: guest.Response == responseNeedsAction}
	base := func(row *models.Event) ICS.Event {
		ev := ICS.Event{UID: e.ICalUID, Sequence: seq, Stamp: now, Summary: row.Title, Description: plainNotes(row), Location: row.Location,
			Organizer: organiser, Attendees: []ICS.Attendee{attendee}, Transparent: row.Transparency == transparencyTransparent, Conference: conference}
		if conference != "" && !strings.Contains(ev.Description, conference) {
			// Calendars that don't show CONFERENCE still show the notes.
			ev.Description = strings.TrimSpace(ev.Description + "\n\nJoin the call: " + conference)
		}
		if row.AllDay() {
			ev.StartDate, ev.EndDate = row.StartDate, row.EndDate
		} else {
			ev.Start, ev.End = *row.StartAt, *row.EndAt
		}
		return ev
	}
	if e.RRule == "" || method == ICS.Cancel {
		return ICS.WriteMessage(ICS.Message{Method: method, Events: []ICS.Event{base(e)}}), nil
	}
	overrides, err := s.store.overridesOfSeries(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	return ICS.WriteMessage(ICS.Message{Method: method, Events: seriesEvents(e, overrides, base)}), nil
}

// seriesEvents is an event as VEVENTs: a single event alone, or a series in
// its zone with the instances skipped in it and added to it, and each
// instance changed on its own (an instance cancelled on its own is skipped).
// base says what each row holds. Invitations and exports both write events
// through it. Pure.
func seriesEvents(e *models.Event, overrides []*models.Event, base func(row *models.Event) ICS.Event) []ICS.Event {
	master := base(e)
	if e.RRule == "" {
		return []ICS.Event{master}
	}
	master.RRule, master.Zone = e.RRule, e.TZ
	master.ExDates = append([]time.Time(nil), e.Exdates...)
	master.RDates = append([]time.Time(nil), e.Rdates...)
	out := []ICS.Event{master}
	for _, o := range overrides {
		if o.RecurrenceID == nil || o.SeriesID == nil || *o.SeriesID != e.ID {
			continue
		}
		if !live(o) {
			out[0].ExDates = append(out[0].ExDates, *o.RecurrenceID)
			continue
		}
		ov := base(o)
		rid := *o.RecurrenceID
		ov.RecurrenceID, ov.Zone = &rid, e.TZ
		out = append(out, ov)
	}
	return out
}

// partStatOf is a stored answer as iCalendar says it. Pure.
func partStatOf(response string) string {
	switch response {
	case responseAccepted:
		return ICS.Accepted
	case responseDeclined:
		return ICS.Declined
	case responseTentative:
		return ICS.Tentative
	}
	return ICS.NeedsAction
}

// linkOf is a guest by email's answer link and its secret, the key that
// also lets them into the event's call (meet-02): theirs, unsealed; or, when
// it can't be (the server's key changed), a new one, kept for them.
func (s *service) linkOf(ctx context.Context, e *models.Event, g models.Attendee) (link, key string, err error) {
	if raw := helpers.OpenLinkSecret(g.LinkSealed, g.LinkHash); raw != "" {
		return rsvpURL(raw), raw, nil
	}
	fresh, err := withLink(models.Attendee{Email: g.Email})
	if err != nil {
		return "", "", err
	}
	if err := s.store.setLink(ctx, e.ID, g.Email, fresh.LinkHash, fresh.LinkSealed); err != nil {
		return "", "", err
	}
	raw, err := helpers.DecryptSecret(fresh.LinkSealed)
	if err != nil {
		return "", "", err
	}
	return rsvpURL(raw), raw, nil
}

// rsvpURL is the guest's page for an answer link's secret.
func rsvpURL(raw string) string { return authService.FrontendBaseURL() + "/rsvp/" + raw }

// callURL is where an event's call at address (its "/r/<slug>") is joined.
// With a guest's key it lets them in without knocking: the key rides in the
// fragment, as the web's lib/calls/invitationKey.ts writes and reads it, so it
// reaches no server log. Without one (the calendar file, which never carries
// the key) it is the call's plain link. Pure.
func callURL(address, key string) string {
	u := authService.FrontendBaseURL() + address
	if key == "" {
		return u
	}
	return u + "#invitation=" + url.QueryEscape(key)
}

// mailWhen is an event's time in an invitation: in its own zone, said ("Thursday
// 16 October 2026, 10:00–11:00 (Europe/London)"), or its days. Pure.
func mailWhen(e *models.Event) string {
	if e.AllDay() {
		first, _ := parseDate(e.StartDate)
		after, _ := parseDate(e.EndDate)
		last := after.AddDate(0, 0, -1)
		if !last.After(first) {
			return first.Format("Monday 2 January 2006") + " (all day)"
		}
		return first.Format("Monday 2 January 2006") + " to " + last.Format("Monday 2 January 2006") + " (all day)"
	}
	return FormatWhen(*e.StartAt, *e.EndAt, e.TZ)
}

// invitationWords are an invitation's subject, headline and why it came.
// Pure.
func invitationWords(kind inviteKind, organiser string, e *models.Event) (subject, title string) {
	when := mailWhen(e)
	switch kind {
	case inviteCallAdded:
		return "Updated invitation: " + e.Title + " · " + when, organiser + " added a call to " + e.Title
	case inviteCallRemoved:
		return "Updated invitation: " + e.Title + " · " + when, organiser + " removed the call from " + e.Title
	case inviteChanged:
		return "Updated invitation: " + e.Title + " · " + when, organiser + " changed " + e.Title
	case inviteAgain:
		return "Invitation: " + e.Title + " · " + when, organiser + " is waiting for your answer"
	}
	return "Invitation: " + e.Title + " · " + when, organiser + " invited you to " + e.Title
}

// invite emails each guest by email their invitation to e, and records
// whether it went.
func (s *service) invite(ctx context.Context, e *models.Event, guests []models.Attendee, kind inviteKind) {
	if len(guests) == 0 {
		return
	}
	if !outsidersMailed() {
		for _, g := range guests {
			s.inviteState(ctx, e, g.Email, models.InviteNotSent)
		}
		return
	}
	organiser := s.organiserAddress(ctx, e)
	now := s.now()
	seq := mailSequence(e, kind, now)
	repeats := ""
	if e.RRule != "" {
		if r, err := ruleOf(e); err == nil {
			start, loc, _ := seriesTime(e)
			repeats = recurrence.Rules.Describe(r, start, loc)
		}
	}
	// The event's own call while it answers (meet-02): the email's Join call
	// carries each guest's key, the calendar file the plain link.
	address := ""
	if call := s.liveCall(ctx, e.CallRoom); call != nil {
		address = call.Address
	}
	conference := ""
	if address != "" {
		conference = callURL(address, "")
	}
	for _, g := range guests {
		link, key, err := s.linkOf(ctx, e, g)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: an invitation's answer link couldn't be made: %v", err)
			s.inviteState(ctx, e, g.Email, models.InviteFailed)
			continue
		}
		join := ""
		if address != "" {
			join = callURL(address, key)
		}
		file, err := s.calendarFile(ctx, ICS.Request, e, g, organiser, seq, now, conference)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: an invitation's calendar file couldn't be written: %v", err)
			s.inviteState(ctx, e, g.Email, models.InviteFailed)
			continue
		}
		subject, title := invitationWords(kind, organiser.Name, e)
		state := models.InviteSent
		if err := s.mailGuest(ctx, g, organiser, ICS.Request, file, subject, title, e, repeats, link, join); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: an invitation wasn't sent: %v", err)
			state = models.InviteFailed
		}
		s.inviteState(ctx, e, g.Email, state)
	}
}

// cancelFor emails each guest by email that e is off their calendar: it was
// cancelled, or they were taken off it.
func (s *service) cancelFor(ctx context.Context, e *models.Event, guests []models.Attendee) {
	if len(guests) == 0 || !outsidersMailed() {
		return
	}
	organiser := s.organiserAddress(ctx, e)
	now := s.now()
	seq := mailSequence(e, inviteChanged, now)
	for _, g := range guests {
		// Only someone who was sent the invitation is sent its end.
		if g.InviteState != models.InviteSent {
			continue
		}
		file, err := s.calendarFile(ctx, ICS.Cancel, e, g, organiser, seq, now, "")
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a cancellation's calendar file couldn't be written: %v", err)
			continue
		}
		subject := "Cancelled: " + e.Title + " · " + mailWhen(e)
		title := organiser.Name + " cancelled " + e.Title
		if err := s.mailGuest(ctx, g, organiser, ICS.Cancel, file, subject, title, e, "", "", ""); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a cancellation wasn't sent: %v", err)
		}
	}
}

// inviteState records what became of a guest's invitation, said in the log
// when it can't be.
func (s *service) inviteState(ctx context.Context, e *models.Event, email, state string) {
	if err := s.store.setInviteState(ctx, e.ID, email, state); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: an invitation's state wasn't kept: %v", err)
	}
}

// mailGuest sends one guest an invitation or a cancellation, with its
// calendar file, and the answers when link is set.
func (s *service) mailGuest(ctx context.Context, g models.Attendee, organiser ICS.Address, method string, file []byte,
	subject, title string, e *models.Event, repeats, link, join string) error {
	var b strings.Builder
	line := func(s string) {
		if b.Len() > 0 {
			b.WriteString("<br>")
		}
		b.WriteString(s)
	}
	if repeats != "" {
		line(html.EscapeString(repeats))
	}
	if e.Location != "" {
		line(html.EscapeString("Where: " + e.Location))
	}
	if notes := plainNotes(e); notes != "" && method != ICS.Cancel {
		line("<br>" + strings.ReplaceAll(html.EscapeString(notes), "\n", "<br>"))
	}
	if join != "" {
		line(`<br><a href="` + html.EscapeString(join) + `"><strong>Join the call</strong></a>`)
	}
	cta := ""
	if link != "" {
		esc := html.EscapeString(link)
		line(`<br><strong>Going?</strong> <a href="` + esc + `?answer=yes">Yes</a> · <a href="` + esc + `?answer=maybe">Maybe</a> · <a href="` + esc + `?answer=no">No</a>`)
		cta = link
	}
	why := organiser.Name + " invited you to this meeting from OneCamp."
	if method == ICS.Cancel {
		why = organiser.Name + " sent this from OneCamp. The calendar file attached takes the meeting off your calendar."
	}
	htmlBody, text, err := emailService.Render(emailService.TemplateData{
		RecipientName: g.Name, ActorName: organiser.Name, Title: title, Subtitle: mailWhen(e), Body: b.String(),
		CTAText: "Answer", CTAURL: cta, BrandName: "OneCamp", Why: why,
	})
	if err != nil {
		return fmt.Errorf("rendering it: %w", err)
	}
	replyTo := ""
	if organiser.Email != emailService.SenderAddress() {
		replyTo = organiser.Email
	}
	return sendGuestMail(ctx, emailService.SendOptions{
		From: emailService.SenderAddress(), To: g.Email, Subject: subject, HTML: htmlBody, Text: text, ReplyTo: replyTo,
		Keep:        emailService.InvitationReserve,
		Attachments: []emailService.Attachment{{Filename: "invite.ics", ContentType: "text/calendar; charset=utf-8; method=" + method, Content: file}},
		Tags:        map[string]string{"kind": "calendar_invitation"},
	})
}
