package Calendar

// Proposing another time (calendar head 4, gcal-4): a guest answers with a
// time that suits them better, for a single event or for one instance of a
// repeating one, as Google's "Propose a new time" does.
//
//   - A proposal rides the guest's answer (AnswerInput.Proposal), from the
//     event's panel or the guest's own page (rsvp.go). A whole series takes
//     none: a time is proposed for one of its events.
//   - It is the proposer's, and whoever may change the event sees it: its
//     organiser, and its guests when they may change it. No other guest, and
//     no guest list on a guest's page, ever shows it.
//   - It stands while the event is still at the time it was proposed
//     against. Any change of that time, by any path (the organiser's "Use
//     this time" is a change like any other, through Update), sets it aside
//     with no write; whoever may change the event can also dismiss it.

import (
	"context"
	"strings"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// ProposalInput is another time a guest proposes with their answer. Left out,
// the one they proposed stands, as a note does (the card's tap sends
// neither); empty ({}), they take it back.
type ProposalInput struct {
	Start *time.Time `json:"start"`
	End   *time.Time `json:"end"`
}

// Proposal is a time a guest proposed, as those who may see it read it.
type Proposal struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// maxProposalAhead is how far ahead a time may be proposed.
const maxProposalAhead = 2 * 366 * 24 * time.Hour

// proposalFor is the proposal an answer leaves on occ, the occurrence
// answered (an instance of a series alone, when rid is set): the one it
// sends, checked against occ at now; none when it takes it back; else the
// one kept. Pure.
func proposalFor(in *ProposalInput, kept *models.Proposal, occ *models.Event, series bool, rid *time.Time, now time.Time) (*models.Proposal, error) {
	if in == nil {
		return kept, nil
	}
	if in.Start == nil && in.End == nil {
		return nil, nil
	}
	if in.Start == nil || in.End == nil {
		return nil, inputError("Propose a time with a start and an end.")
	}
	// To the second, as it is kept: a proposal read back compares equal.
	start, end := in.Start.UTC().Truncate(time.Second), in.End.UTC().Truncate(time.Second)
	switch {
	case series && rid == nil:
		return nil, inputError("Propose a new time for one event of the series.")
	case occ.AllDay() || occ.StartAt == nil || occ.EndAt == nil:
		return nil, inputError("A new time can be proposed for an event at a time, not for a whole day.")
	case !occ.EndAt.After(now):
		return nil, inputError("This event is over, so there's no time to propose.")
	case !end.After(start):
		return nil, inputError("A proposed time ends after it starts.")
	case !start.After(now):
		return nil, inputError("Propose a time that hasn't passed.")
	case start.After(now.Add(maxProposalAhead)):
		return nil, inputError("Propose a time in the next two years.")
	case start.Equal(occ.StartAt.UTC()) && end.Equal(occ.EndAt.UTC()):
		return nil, inputError("That's the time it's at already.")
	}
	return &models.Proposal{Start: start, End: end, AgainstStart: occ.StartAt.UTC(), AgainstEnd: occ.EndAt.UTC()}, nil
}

// newlyProposed is what an answer proposes anew, for the organiser's notice:
// the proposal it sends (now), unless the guest had proposed just that
// already (was); nil when it sends none or takes one back. Pure.
func newlyProposed(in *ProposalInput, was *models.Attendee, now *models.Proposal) *models.Proposal {
	if in == nil || now == nil {
		return nil
	}
	if was != nil && was.Proposal != nil && sameProposal(*was.Proposal, *now) {
		return nil
	}
	return now
}

// sameProposal is whether a and b propose the same time against the same
// time. Pure.
func sameProposal(a, b models.Proposal) bool {
	return a.Start.Equal(b.Start) && a.End.Equal(b.End) && a.AgainstStart.Equal(b.AgainstStart) && a.AgainstEnd.Equal(b.AgainstEnd)
}

// proposalStands is whether p is still a proposal for occ: occ is still at
// the time it was proposed against. Pure.
func proposalStands(p *models.Proposal, occ *models.Event) bool {
	return p != nil && occ.StartAt != nil && occ.EndAt != nil && !occ.AllDay() &&
		p.AgainstStart.Equal(*occ.StartAt) && p.AgainstEnd.Equal(*occ.EndAt)
}

// proposalShown is guest a's proposal as reader sees it on occ: shown to
// whoever may change the event (mayChangeIt) and to the guest who proposed
// it, while it stands; nil otherwise. Pure.
func proposalShown(a models.Attendee, occ *models.Event, reader uuid.UUID, mayChangeIt bool) *Proposal {
	if !proposalStands(a.Proposal, occ) {
		return nil
	}
	if !mayChangeIt && (a.UserID == nil || *a.UserID != reader) {
		return nil
	}
	return &Proposal{Start: a.Proposal.Start, End: a.Proposal.End}
}

// DismissInput names the guest whose proposal is dismissed: a member by id,
// or a guest invited by email.
type DismissInput struct {
	UserUUID string `json:"user_uuid"`
	Email    string `json:"email"`
}

// DismissProposal sets aside a guest's proposal of another time, for an
// event or (by an instance's id) for that instance alone. It is for whoever
// may change the event; the guest keeps their answer and note. Dismissing
// one that is already gone is no error.
func (s *service) DismissProposal(ctx context.Context, who *model.UserInfo, id string, in DismissInput) (*Event, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return nil, err
	}
	owner := inst.owner()
	if err := mayChange(roleOf(owner, guests, personOf(who)), owner); err != nil {
		return nil, err
	}
	var key models.GuestKey
	if strings.TrimSpace(in.UserUUID) != "" {
		person, perr := uuid.Parse(strings.TrimSpace(in.UserUUID))
		if perr != nil {
			return nil, inputError("Say whose proposal to dismiss.")
		}
		key = models.GuestKey{UserID: &person}
	} else {
		email, _, aerr := cleanAddress(in.Email)
		if aerr != nil {
			return nil, inputError("Say whose proposal to dismiss.")
		}
		key = models.GuestKey{Email: email}
	}
	if answerOf(guests, key) == nil {
		return nil, inputError("That person isn't one of this event's guests.")
	}
	if _, err := s.store.clearProposal(ctx, owner.ID, inst.rid, key); err != nil {
		return nil, err
	}
	got, g, err := s.resolve(ctx, who, inst.ref)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, who, got, g), nil
}
