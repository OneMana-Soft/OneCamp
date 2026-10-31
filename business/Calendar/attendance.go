package Calendar

// Attendance (PROGRAM.md §4.2 #40, calendar A2): who is on an event and what
// each of them answered.
//
//   - Guests are workspace members, by id or by an address that is theirs,
//     and anyone else by email. Each is on an event once; its organiser never
//     is.
//   - A guest answers yes, no or maybe, with a note, for a whole event or
//     series, or for one instance of a series alone. Saying no keeps them on
//     the guest list, as Google does: leaving is its own act (LeaveEvent).
//   - A guest invited by email answers from the link in their invitation
//     (rsvp.go), with no account.
//   - An answer is the guest's, not a change to the event: the event's
//     sequence stays, so nobody's open edit goes stale because a guest said
//     yes, and the organiser hears of it in their inbox (notify.go).

import (
	"context"
	"net/mail"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// maxEmailLength is the longest address mail carries (RFC 5321).
const maxEmailLength = 254

// responseOf is an answer as it is stored. Pure.
func responseOf(a Answer) (string, bool) {
	switch a {
	case Accepted:
		return responseAccepted, true
	case Declined:
		return responseDeclined, true
	case Tentative:
		return responseTentative, true
	}
	return "", false
}

// cleanAddress is an address as a guest is kept by: the address alone, in
// lower case, and the name it came with ("Alex Kim <alex@example.org>"); or
// an error said in words. Pure.
func cleanAddress(raw string) (email, name string, err error) {
	raw = strings.TrimSpace(raw)
	a, perr := mail.ParseAddress(raw)
	if perr != nil || a.Address == "" || len(a.Address) > maxEmailLength || !strings.Contains(a.Address, "@") {
		return "", "", inputError(`"` + clip(raw, 60) + `" isn't an email address.`)
	}
	return strings.ToLower(a.Address), a.Name, nil
}

// cleanName is a guest's name as it is kept: on one line, without control
// characters, at most maxGuestNameRunes. Pure.
func cleanName(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }), " ")
	if utf8.RuneCountInString(s) > maxGuestNameRunes {
		s = string([]rune(s)[:maxGuestNameRunes])
	}
	return s
}

// clip is s cut to n characters, for an error that quotes what was typed.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// invitable is who of wanted can be put on an event organised by organiser,
// each once, in the order asked, with the role each was asked with: active
// workspace members with an account (by id, or by an address that is
// theirs), and anyone else by a well-formed address (lower case, with their
// name). Never the organiser.
func (s *service) invitable(ctx context.Context, organiser uuid.UUID, wanted []GuestInput) ([]GuestInput, error) {
	type ask struct {
		g     GuestInput
		email string // a guest asked for by address, cleaned
	}
	asks := make([]ask, 0, len(wanted))
	var emails []string
	for _, g := range wanted {
		if strings.TrimSpace(g.UUID) != "" {
			asks = append(asks, ask{g: g})
			continue
		}
		if strings.TrimSpace(g.Email) == "" {
			continue
		}
		email, name, err := cleanAddress(g.Email)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(g.Name) == "" {
			g.Name = name
		}
		g.Name = cleanName(g.Name)
		asks = append(asks, ask{g: g, email: email})
		emails = append(emails, email)
	}
	// An address that is a member's is that member.
	byEmail := map[string]uuid.UUID{}
	if len(emails) > 0 {
		var err error
		if byEmail, err = s.store.peopleByEmail(ctx, emails); err != nil {
			return nil, err
		}
	}
	var ids []string
	seen := map[string]bool{organiser.String(): true}
	for i, a := range asks {
		if a.email != "" {
			id, ok := byEmail[a.email]
			if !ok {
				continue
			}
			asks[i].g.UUID = id.String()
		}
		id, err := uuid.Parse(strings.TrimSpace(asks[i].g.UUID))
		if err != nil || seen[id.String()] {
			continue
		}
		seen[id.String()] = true
		ids = append(ids, id.String())
	}
	if len(asks) > maxGuests {
		return nil, inputError("An event can have up to 500 guests.")
	}
	members := map[string]bool{}
	if len(ids) > 0 {
		active, err := s.members(ctx, ids)
		if err != nil {
			return nil, err
		}
		var activeIDs []uuid.UUID
		for _, u := range active {
			if u != nil {
				activeIDs = appendUUIDs(activeIDs, u.Uuid)
			}
		}
		// A guest is a row of event_attendees, which names an account:
		// someone the graph knows and Postgres doesn't can't be one.
		accounts, err := s.store.known(ctx, activeIDs)
		if err != nil {
			return nil, err
		}
		for id := range accounts {
			members[id.String()] = true
		}
	}
	var out []GuestInput
	taken := map[string]bool{}
	for _, a := range asks {
		r := a.g.Role
		if r != roleOptional {
			r = roleRequired
		}
		if id, err := uuid.Parse(strings.TrimSpace(a.g.UUID)); err == nil {
			if id == organiser {
				continue
			}
			// A member who is active, once; an address that is someone
			// who isn't (left the workspace) is invited as an address.
			if members[id.String()] && !taken[id.String()] {
				taken[id.String()] = true
				out = append(out, GuestInput{UUID: id.String(), Role: r})
				continue
			}
			if a.email == "" || members[id.String()] {
				continue
			}
		}
		if a.email == "" || taken["mailto:"+a.email] {
			continue
		}
		taken["mailto:"+a.email] = true
		out = append(out, GuestInput{Email: a.email, Name: a.g.Name, Role: r})
	}
	return out, nil
}

// newGuest is the row of a guest put on an event: a member, or someone by
// email with their own answer link.
func newGuest(event uuid.UUID, g GuestInput, now time.Time) (models.Attendee, error) {
	if g.UUID != "" {
		id := uuid.MustParse(g.UUID)
		return models.Attendee{EventID: event, UserID: &id, Role: g.Role, Response: responseNeedsAction, AddedAt: now}, nil
	}
	return withLink(models.Attendee{EventID: event, Email: g.Email, Name: g.Name, Role: g.Role, Response: responseNeedsAction, AddedAt: now})
}

// withLink gives a guest by email an answer link of their own: kept as its
// hash, which finds them, and sealed with the server's key, so the organiser
// can copy it or have it sent again. A member needs none.
func withLink(a models.Attendee) (models.Attendee, error) {
	if a.UserID != nil {
		return a, nil
	}
	_, hash, sealed, err := helpers.NewSealedLinkSecret()
	if err != nil {
		return a, err
	}
	a.LinkHash, a.LinkSealed = hash, sealed
	return a, nil
}

// copyGuests is guests on another event, each with their answer, and a guest
// by email with a link of their own for it (models.Attendee.CopyTo).
func copyGuests(guests []models.Attendee, event uuid.UUID) ([]models.Attendee, error) {
	out := make([]models.Attendee, 0, len(guests))
	for _, g := range guests {
		c, err := withLink(g.CopyTo(event))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// emailsOf are the addresses of guests by email, in order.
func emailsOf(guests []models.Attendee) []string {
	var out []string
	for _, a := range guests {
		if a.UserID == nil && a.Email != "" {
			out = append(out, strings.ToLower(a.Email))
		}
	}
	return out
}

// occurrenceKey names one instance of one series. Pure.
func occurrenceKey(series uuid.UUID, rid time.Time) string {
	return series.String() + "|" + rid.UTC().Format(time.RFC3339Nano)
}

// withAnswers is each occurrence's guests as they stand for it, by the id the
// reader knows it by: its event's or series' guests, with what each answered
// for that instance alone. One read for all of them; one that fails leaves
// the series' answers, said in the log.
func (s *service) withAnswers(ctx context.Context, rows []instance, guests map[uuid.UUID][]models.Attendee) map[string][]models.Attendee {
	out := make(map[string][]models.Attendee, len(rows))
	var series []uuid.UUID
	var rids []time.Time
	for _, inst := range rows {
		out[inst.ref] = guests[inst.owner().ID]
		if inst.rid != nil && len(guests[inst.owner().ID]) > 0 {
			series, rids = append(series, inst.owner().ID), append(rids, *inst.rid)
		}
	}
	if len(series) == 0 {
		return out
	}
	answers, err := s.store.instanceAnswers(ctx, series, rids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: answers for single instances couldn't be read: %v", err)
		return out
	}
	byInstance := map[string][]models.InstanceAnswer{}
	for _, a := range answers {
		k := occurrenceKey(a.EventID, a.RecurrenceID)
		byInstance[k] = append(byInstance[k], a)
	}
	for _, inst := range rows {
		if inst.rid == nil {
			continue
		}
		own := byInstance[occurrenceKey(inst.owner().ID, *inst.rid)]
		if len(own) == 0 {
			continue
		}
		gs := slices.Clone(guests[inst.owner().ID])
		for i := range gs {
			for _, a := range own {
				if a.Key().Is(gs[i]) {
					at := a.RespondedAt
					gs[i].Response, gs[i].Note, gs[i].RespondedAt, gs[i].Proposal = a.Response, a.Note, &at, a.Proposal
				}
			}
		}
		out[inst.ref] = gs
	}
	return out
}

// answerOf is what someone answered of a guest list: their row, or nil.
func answerOf(guests []models.Attendee, k models.GuestKey) *models.Attendee {
	for i := range guests {
		if k.Is(guests[i]) {
			return &guests[i]
		}
	}
	return nil
}

// Answer is a guest's answer to an event they are on, for the whole of it
// or, for an instance of a series with Scope "this", that instance alone.
func (s *service) Answer(ctx context.Context, who *model.UserInfo, id string, in AnswerInput) (*Event, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	response, ok := responseOf(in.Answer)
	if !ok {
		return nil, inputError("Answer yes, no or maybe.")
	}
	if in.noteTooLong() {
		return nil, inputError("A note with an answer can be up to 500 characters.")
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return nil, err
	}
	owner, me := inst.owner(), personOf(who)
	switch roleOf(owner, guests, me) {
	case roleOrganiser:
		return nil, inputError("You organise this event, so there's no invitation to answer.")
	case roleNone:
		return nil, ErrEventNotFound
	}
	var rid *time.Time
	switch in.Scope {
	case "", ScopeAll:
	case ScopeThis:
		rid = inst.rid // a single event has no instance: its answer is for all of it
	default:
		return nil, inputError("Say whether the answer is for this event or all events.")
	}
	key := models.GuestKey{UserID: &me}
	before := s.withAnswers(ctx, []instance{inst}, map[uuid.UUID][]models.Attendee{owner.ID: guests})[inst.ref]
	was := answerOf(before, key)
	// A note left out stays as it stands for what is answered: the series'
	// own for all of it, the instance's for one instance.
	kept := was
	if rid == nil {
		kept = answerOf(guests, key)
	}
	note := ""
	var keptProposal *models.Proposal
	if kept != nil {
		note, keptProposal = kept.Note, kept.Proposal
	}
	note = in.noteOr(note)
	at := s.now()
	proposal, err := proposalFor(in.Proposal, keptProposal, inst.Event, owner.RRule != "", rid, at)
	if err != nil {
		return nil, err
	}
	on, err := s.store.answer(ctx, owner.ID, rid, key, response, note, at, proposal)
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, ErrEventNotFound
	}
	data := map[string]string{"response": response}
	if rid != nil {
		data["instance"] = inst.ref
	}
	s.announce(ctx, who, resourceEventBusiness.CalendarEventRSVP, owner, []string{me.String()}, data)
	// Told once: the same answer again tells nobody; a time proposed anew is told.
	proposed := newlyProposed(in.Proposal, was, proposal)
	if was == nil || was.Response != response || was.Note != note || proposed != nil {
		t := telling{what: toldAnswered, event: owner, now: inst.Event, guestsAfter: guests,
			answer: &answered{key: key, response: response, note: note, proposal: proposed}}
		if rid != nil {
			t.ref = inst.ref
		}
		s.tells.tell(ctx, who, t)
	}
	got, g, err := s.resolve(ctx, who, inst.ref)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, who, got, g), nil
}

// nudgeEvery is how often a guest may be asked again for an answer.
const nudgeEvery = 24 * time.Hour

// Nudge asks again every guest of an event who hasn't answered: members in
// their inbox and by push, guests by email by their invitation again (which
// sends one to anyone whose invitation was never sent). Each guest is asked
// at most once a day. The organiser's alone; it answers how many were asked.
func (s *service) Nudge(ctx context.Context, who *model.UserInfo, id string) (int, error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return 0, err
	}
	owner := inst.owner()
	switch roleOf(owner, guests, personOf(who)) {
	case roleOrganiser:
	case roleGuest:
		return 0, ErrNotEventCreator
	default:
		return 0, ErrEventNotFound
	}
	if last := lastEnd(owner); last != nil && s.now().After(*last) {
		return 0, inputError("This event is over, so there's nobody to ask.")
	}
	now := s.now()
	var asked []models.Attendee
	var people []uuid.UUID
	var emails []string
	for _, g := range guests {
		if g.Response != responseNeedsAction || (g.NudgedAt != nil && now.Sub(*g.NudgedAt) < nudgeEvery) {
			continue
		}
		asked = append(asked, g)
		if g.UserID != nil {
			people = append(people, *g.UserID)
		} else {
			emails = append(emails, g.Email)
		}
	}
	if len(asked) == 0 {
		return 0, nil
	}
	if err := s.store.setNudged(ctx, owner.ID, people, emails, now); err != nil {
		return 0, err
	}
	s.tells.tell(ctx, who, telling{what: toldNudged, event: owner, now: owner, guestsAfter: guests, nudged: asked})
	return len(asked), nil
}

// GuestLink is the answer link of a guest invited by email, for the
// organiser to share themselves (when their invitation wasn't emailed: Cloud's
// essentials-only email, the demo). The organiser's alone.
func (s *service) GuestLink(ctx context.Context, who *model.UserInfo, id, email string) (string, error) {
	if err := s.ready(ctx); err != nil {
		return "", err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return "", err
	}
	owner := inst.owner()
	switch roleOf(owner, guests, personOf(who)) {
	case roleOrganiser:
	case roleGuest:
		return "", ErrNotEventCreator
	default:
		return "", ErrEventNotFound
	}
	clean, _, err := cleanAddress(email)
	if err != nil {
		return "", err
	}
	g := answerOf(guests, models.GuestKey{Email: clean})
	if g == nil {
		return "", inputError("That address isn't one of this event's guests.")
	}
	link, _, err := s.linkOf(ctx, owner, *g)
	return link, err
}
