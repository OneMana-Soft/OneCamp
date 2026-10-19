package Calendar

// A guest's own page (calendar A2): someone invited by email opens the link in
// their invitation and answers yes, no or maybe, with a note, with no account.
//
//   - The link is theirs: it answers for them alone, for the whole event or
//     series, and only while they are on it. Taken off it, or the event
//     deleted, the link stops at once; a month after the event has ended it
//     stops too.
//   - The page shows the meeting as the invitation did: its title, time,
//     place and notes, the organiser's name, and how it repeats. The other
//     guests show, by name and answer, only when the organiser lets guests
//     see the guest list. No member's address is ever on it.
//   - Every "not available" reads alike (CAL9): an unknown link, a guest taken
//     off, a deleted event, one long over.

import (
	"context"
	"errors"
	"strings"
	"time"

	recurrence "github.com/akashc777/OneCamp/business/Recurrence"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// ErrInvitationGone is a link that can't be answered: unknown, or its guest
// taken off, or its event deleted or long over. Said alike for all of them.
var ErrInvitationGone = errors.New("This invitation isn't available any more. If you still need it, ask whoever invited you to send it again.")

// linkGrace is how long after an event's last end its guests' links work.
const linkGrace = 30 * 24 * time.Hour

// InvitationGuest is someone on an invitation, as its page shows them.
type InvitationGuest struct {
	Name     string `json:"name"`
	Response string `json:"response"`
}

// InvitationYou is the guest the page is for, with their answer and note.
type InvitationYou struct {
	Name     string `json:"name,omitempty"`
	Email    string `json:"email"`
	Response string `json:"response"`
	Note     string `json:"note,omitempty"`
}

// InvitationView is what a guest's page shows.
type InvitationView struct {
	Title     string            `json:"title"`
	Start     When              `json:"start"`
	End       When              `json:"end"`
	AllDay    bool              `json:"all_day"`
	Location  string            `json:"location,omitempty"`
	Notes     string            `json:"notes,omitempty"`
	Repeats   string            `json:"repeats,omitempty"`
	Organiser string            `json:"organiser"`
	Ended     bool              `json:"ended"`
	You       InvitationYou     `json:"you"`
	Guests    []InvitationGuest `json:"guests,omitempty"`
}

// lastEnd is when an event, or a series' last instance, ends; nil for a
// series with no end. Pure.
func lastEnd(e *models.Event) *time.Time {
	if e.RRule != "" {
		if e.UntilAt == nil {
			return nil
		}
		_, _, dur := seriesTime(e)
		end := e.UntilAt.Add(dur)
		return &end
	}
	_, end := spanOf(e)
	return &end
}

// byLink is the guest a link's secret names and their event, live and not long
// over at now; ErrInvitationGone otherwise.
func (s *service) byLink(ctx context.Context, secret string, now time.Time) (*models.Event, *models.Attendee, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" || len(secret) > 100 {
		return nil, nil, ErrInvitationGone
	}
	g, err := s.store.guestByLink(ctx, helpers.LinkSecretHash(secret))
	if err != nil {
		return nil, nil, err
	}
	if g == nil || g.UserID != nil {
		return nil, nil, ErrInvitationGone
	}
	e, err := s.store.get(ctx, g.EventID)
	if err != nil {
		return nil, nil, err
	}
	if e == nil || e.DeletedAt != nil || e.SeriesID != nil || e.Status == statusCancelled {
		return nil, nil, ErrInvitationGone
	}
	if end := lastEnd(e); end != nil && now.After(end.Add(linkGrace)) {
		return nil, nil, ErrInvitationGone
	}
	return e, g, nil
}

// Invitation is the page a guest's link opens.
func Invitation(ctx context.Context, secret string, now time.Time) (*InvitationView, error) {
	return stored.invitation(ctx, secret, now)
}

// AnswerInvitation is a guest's answer from their link: for the whole event
// or series.
func AnswerInvitation(ctx context.Context, secret string, in AnswerInput, now time.Time) (*InvitationView, error) {
	return stored.answerInvitation(ctx, secret, in, now)
}

// linksReady is whether a guest's page can be answered now: on a server whose
// calendar isn't in its one store there are no guests by email, so no link
// is anyone's; while it moves, the guest is asked to try again.
func (s *service) linksReady(ctx context.Context) error {
	err := s.ready(ctx)
	if errors.Is(err, ErrStoreOff) {
		return ErrInvitationGone
	}
	return err
}

func (s *service) invitation(ctx context.Context, secret string, now time.Time) (*InvitationView, error) {
	if err := s.linksReady(ctx); err != nil {
		return nil, err
	}
	e, g, err := s.byLink(ctx, secret, now)
	if err != nil {
		return nil, err
	}
	return s.invitationView(ctx, e, g, now)
}

func (s *service) answerInvitation(ctx context.Context, secret string, in AnswerInput, now time.Time) (*InvitationView, error) {
	if err := s.linksReady(ctx); err != nil {
		return nil, err
	}
	response, ok := responseOf(in.Answer)
	if !ok {
		return nil, inputError("Answer yes, no or maybe.")
	}
	if in.noteTooLong() {
		return nil, inputError("A note with your answer can be up to 500 characters.")
	}
	e, g, err := s.byLink(ctx, secret, now)
	if err != nil {
		return nil, err
	}
	note := in.noteOr(g.Note)
	key := g.Key()
	on, err := s.store.answer(ctx, e.ID, nil, key, response, note, now)
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, ErrInvitationGone
	}
	if g.Response != response || g.Note != note {
		s.emit(ctx, resourceEventBusiness.Event{
			Kind: resourceEventBusiness.CalendarEventRSVP, ResourceKind: KindEvent, ResourceUUID: e.ID.String(),
			Actor: resourceEventBusiness.Actor{}, At: now, Subjects: []string{e.CreatedBy.String()},
			Data: map[string]string{"response": response, "kind": e.Kind, "by": "guest"},
		})
		guests, err := s.store.attendees(ctx, []uuid.UUID{e.ID})
		if err != nil {
			return nil, err
		}
		s.tells.tell(ctx, nil, telling{what: toldAnswered, event: e, guestsAfter: guests[e.ID],
			answer: &answered{key: key, response: response, note: note}})
		g.Response, g.Note = response, note
	}
	return s.invitationView(ctx, e, g, now)
}

// invitationView is the page for guest g of e.
func (s *service) invitationView(ctx context.Context, e *models.Event, g *models.Attendee, now time.Time) (*InvitationView, error) {
	start, end := whenOf(e)
	v := &InvitationView{Title: e.Title, Start: start, End: end, AllDay: e.AllDay(), Location: e.Location, Notes: plainNotes(e),
		You: InvitationYou{Name: g.Name, Email: g.Email, Response: g.Response, Note: g.Note}}
	if last := lastEnd(e); last != nil && now.After(*last) {
		v.Ended = true
	}
	if e.RRule != "" {
		if r, err := ruleOf(e); err == nil {
			sStart, loc, _ := seriesTime(e)
			v.Repeats = recurrence.Rules.Describe(r, sStart, loc)
		}
	}
	all := map[uuid.UUID][]models.Attendee{}
	if e.GuestsCanSeeGuests {
		got, err := s.store.attendees(ctx, []uuid.UUID{e.ID})
		if err != nil {
			return nil, err
		}
		all = got
	}
	names := s.namesOf(ctx, []instance{single(e)}, all)
	v.Organiser = "Someone"
	if u := names[e.CreatedBy.String()]; u != nil {
		v.Organiser = u.DisplayName()
	}
	for _, a := range all[e.ID] {
		if g.Key().Is(a) {
			continue
		}
		name := a.Name
		if a.UserID != nil {
			if u := names[a.UserID.String()]; u != nil {
				name = u.DisplayName()
			}
		}
		if name == "" {
			// Someone invited by email without a name: their address's
			// first part, never the address itself.
			name, _, _ = strings.Cut(a.Email, "@")
		}
		if name == "" {
			continue
		}
		v.Guests = append(v.Guests, InvitationGuest{Name: name, Response: a.Response})
	}
	return v, nil
}
