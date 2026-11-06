package Calendar

// Out of office that declines (calendar head 5b, gcal-9b). An out-of-office
// event may carry a rule: "new" declines invitations, to events that start
// while it runs, that arrive after it is set; "all" also declines the ones
// already there when it is set. Each decline carries the event's message as
// its note, and the organiser is told as for any answer.
//
// Permission model: only a person's own out-of-office declines for them, and
// only events they are a guest of, never ones they organise; the rule is
// written by whoever may edit the out-of-office event (its owner), read back
// only by them, and a guest sees only the decline and its note, as any answer.

import (
	"context"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// maxOOOMessage is the longest message a decline carries (the table's CHECK).
const maxOOOMessage = 500

// OOOInput is an out-of-office event's decline rule: Decline is "" or
// "none" for none, "new" or "all"; Message goes with each decline.
type OOOInput struct {
	Decline string `json:"decline"`
	Message string `json:"message"`
}

// OOOView is the rule as its owner reads it back.
type OOOView struct {
	Decline string `json:"decline"`
	Message string `json:"message"`
}

// oooRuleOf checks an input for an event of kind: a rule only on an
// out-of-office event.
func oooRuleOf(in *OOOInput, kind EventKind) (models.OOORule, error) {
	r := models.OOORule{Message: strings.TrimSpace(in.Message)}
	switch in.Decline {
	case "", "none":
		r.Decline = models.OOODeclineNone
	case models.OOODeclineNew, models.OOODeclineAll:
		r.Decline = in.Decline
	default:
		return r, inputError("Decline none, new invitations, or new and existing ones.")
	}
	if r.Decline != models.OOODeclineNone && kind != EventAway {
		return r, inputError("Only out of office declines invitations.")
	}
	if len([]rune(r.Message)) > maxOOOMessage {
		return r, inputError("A message with a decline can be up to 500 characters.")
	}
	return r, nil
}

// keepRule writes an out-of-office event's rule and, for "all", declines the
// invitations already in its time. Who may write it is the caller's check.
func (s *service) keepRule(ctx context.Context, e *models.Event, r models.OOORule) error {
	if err := s.store.setOOORule(ctx, e.ID, r); err != nil {
		return err
	}
	if r.Decline == models.OOODeclineAll {
		s.declineExisting(ctx, e, r)
	}
	return nil
}

// declineForAway declines, for each of these guests of e who is out of office
// with a rule when e starts, e on their behalf with their message. Called for
// guests newly on e. Never the organiser, never a guest by email, never an
// answer already given. A failure declines nobody and is logged: the event
// itself is written.
func (s *service) declineForAway(ctx context.Context, e *models.Event, guests []models.Attendee) {
	if e == nil || EventKind(e.Kind) == EventAway {
		return
	}
	var people []uuid.UUID
	for _, g := range guests {
		if g.UserID != nil && *g.UserID != e.CreatedBy && g.Response == responseNeedsAction {
			people = append(people, *g.UserID)
		}
	}
	if len(people) == 0 {
		return
	}
	if e.RRule != "" {
		// A series is never declined whole (CR-68 MF1): a week away would
		// leave every occurrence. Its occurrences inside the away are.
		s.declineOccurrencesForAway(ctx, e, people, guests)
		return
	}
	start, end := spanOf(e)
	w, err := window(start, end.Add(time.Minute))
	if err != nil {
		return
	}
	aways, rules := s.awaysWithRules(ctx, people, w)
	declined := map[uuid.UUID]bool{}
	var out []oooDecline
	for _, a := range aways {
		r, ok := rules[a.ID]
		if !ok || declined[a.CreatedBy] {
			continue
		}
		as, ae := spanOf(a)
		// Out of office when it starts: an event that starts while away.
		if start.Before(as) || !start.Before(ae) {
			continue
		}
		declined[a.CreatedBy] = true
		out = append(out, oooDecline{event: e, now: e, ref: e.ID.String(), guests: guests, person: a.CreatedBy, note: r.Message})
	}
	s.declineAll(ctx, out)
}

// awayHorizon is how far ahead of a series' start (or now, when later) its
// occurrences are matched against its guests' aways: a year, as far ahead as
// anyone sets time off.
const awayHorizon = 366 * 24 * time.Hour

// declineOccurrencesForAway declines, for each of people away with a rule
// that declines, the occurrences of the series e that start while they are
// away, each alone with the away's message: never the series whole, and
// never an occurrence they already answered. One read of their aways and
// rules, one expansion of the series over the aways' span, and one read of
// its answers for single instances. A failure declines nobody and is logged.
func (s *service) declineOccurrencesForAway(ctx context.Context, e *models.Event, people []uuid.UUID, guests []models.Attendee) {
	from, _ := spanOf(e)
	if now := s.now(); now.After(from) {
		from = now
	}
	aways, rules := s.awaysWithRules(ctx, people, spanWindow(from, from.Add(awayHorizon)))
	type away struct {
		person   uuid.UUID
		from, to time.Time
		note     string
	}
	var spans []away
	var first, last time.Time
	for _, a := range aways {
		r, ok := rules[a.ID]
		if !ok {
			continue
		}
		as, ae := spanOf(a)
		spans = append(spans, away{a.CreatedBy, as, ae, r.Message})
		if first.IsZero() || as.Before(first) {
			first = as
		}
		if ae.After(last) {
			last = ae
		}
	}
	if len(spans) == 0 {
		return
	}
	// Never the past: an away that began before now declines from now on.
	if first.Before(from) {
		first = from
	}
	insts, err := s.instancesOf(ctx, []*models.Event{e}, spanWindow(first, last), maxInstances)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: a series' occurrences couldn't be read to decline for an away: %v", err)
		return
	}
	occ := s.withAnswers(ctx, insts, map[uuid.UUID][]models.Attendee{e.ID: guests})
	seen := map[string]bool{}
	var out []oooDecline
	for _, inst := range insts {
		if inst.rid == nil {
			continue
		}
		is, _ := spanOf(inst.Event)
		if is.Before(from) {
			continue
		}
		for _, sp := range spans {
			key := sp.person.String() + " " + inst.ref
			if seen[key] || is.Before(sp.from) || !is.Before(sp.to) {
				continue
			}
			if mine := answerOf(occ[inst.ref], models.GuestKey{UserID: &sp.person}); mine == nil || mine.Response != responseNeedsAction {
				continue
			}
			seen[key] = true
			// A guest newly on the series holds no answer for a single
			// instance (ForgetInstanceAnswers drops them when a guest is taken
			// off), so this guards only against a change in that rule.
			out = append(out, oooDecline{event: e, now: inst.Event, ref: inst.ref, rid: inst.rid, guests: guests, person: sp.person, note: sp.note})
		}
	}
	s.declineAll(ctx, out)
}

// declineExisting declines, for the owner of away, the invitations already
// on their calendar that start while away runs and they haven't answered.
func (s *service) declineExisting(ctx context.Context, away *models.Event, r models.OOORule) {
	me := away.CreatedBy
	start, end := spanOf(away)
	rows, guests, _, err := s.rangeRows(ctx, me, start, end)
	if err != nil {
		return
	}
	occ := s.withAnswers(ctx, rows, guests)
	seen := map[string]bool{}
	var out []oooDecline
	for _, inst := range rows {
		owner := inst.owner()
		if owner.CreatedBy == me || seen[inst.ref] || EventKind(owner.Kind) == EventAway {
			continue
		}
		is, _ := spanOf(inst.Event)
		if is.Before(start) || !is.Before(end) {
			continue
		}
		mine := answerOf(occ[inst.ref], models.GuestKey{UserID: &me})
		if mine == nil || mine.Response != responseNeedsAction {
			continue
		}
		// The occurrence alone: an instance of a series answers for itself (CR-68 MF1).
		seen[inst.ref] = true
		out = append(out, oooDecline{event: owner, now: inst.Event, ref: inst.ref, rid: inst.rid, guests: guests[owner.ID], person: me, note: r.Message})
	}
	s.declineAll(ctx, out)
}

// newlyOn is the guests in after who weren't in before.
func newlyOn(before, after []models.Attendee) []models.Attendee {
	was := map[uuid.UUID]bool{}
	for _, g := range before {
		if g.UserID != nil {
			was[*g.UserID] = true
		}
	}
	var out []models.Attendee
	for _, g := range after {
		if g.UserID != nil && !was[*g.UserID] {
			out = append(out, g)
		}
	}
	return out
}

// oooDecline is one decline an out-of-office makes.
type oooDecline struct {
	// event is the row answered (a series' own for an instance), now the
	// occurrence, ref its id, and rid its start in its series (nil for a
	// single event): an instance is declined alone.
	event  *models.Event
	now    *models.Event
	ref    string
	rid    *time.Time
	guests []models.Attendee
	person uuid.UUID
	note   string
}

// declineAll stores each decline (the whole event, or one instance) with its
// note, recorded as the away's (by_away), and tells each organiser as for
// any answer, the people named in one read. One person's declines of one
// series are told once, for the first of its days declined (CR-B1 M1): a
// fortnight away on a daily standup is one notice, not ten. Each day's
// answer and its mark are on the event for anyone who opens it.
func (s *service) declineAll(ctx context.Context, all []oooDecline) {
	if len(all) == 0 {
		return
	}
	named := map[string]*dgraphStruct.DgraphUser{}
	if s.people != nil {
		ids := make([]string, 0, len(all))
		for _, d := range all {
			ids = append(ids, d.person.String())
		}
		if people, err := s.people(ctx, ids); err == nil {
			for _, p := range people {
				if p != nil {
					named[p.Uuid] = p
				}
			}
		}
	}
	at := s.now()
	told := map[string]bool{}
	for _, d := range all {
		key := models.GuestKey{UserID: &d.person}
		on, err := s.store.answer(ctx, d.event.ID, d.rid, key, responseDeclined, d.note, at, nil, true)
		if err != nil || !on {
			continue
		}
		if d.rid != nil {
			once := d.person.String() + " " + d.event.ID.String()
			if told[once] {
				continue
			}
			told[once] = true
		}
		actor := &model.UserInfo{UserPostgresInfo: model.User{Id: d.person}}
		actor.UserDgraphInfo.Uuid = d.person.String()
		if p := named[d.person.String()]; p != nil {
			actor.UserDgraphInfo = *p
		}
		after := make([]models.Attendee, len(d.guests))
		copy(after, d.guests)
		for i := range after {
			if after[i].UserID != nil && *after[i].UserID == d.person {
				after[i].Response, after[i].Note = responseDeclined, d.note
			}
		}
		t := telling{what: toldAnswered, event: d.event, now: d.now, guestsAfter: after,
			answer: &answered{key: key, response: responseDeclined, note: d.note}}
		if d.rid != nil {
			t.ref = d.ref
		}
		s.tells.tell(ctx, actor, t)
	}
}

// awayDecliners is, for each occurrence by its ref, those who declined it
// while out of office: the "Out of office" beside their answer (head 5c,
// gcal-9b), the one rule by which every read of guests' answers marks it, a
// list's as one event's. The mark is the record of the decline their away
// made (by_away, head C), never inferred from the times: a decline made by
// hand inside an away, or before it was set, is a plain one, and an answer
// given by hand clears the record. It tells when someone is away, so reader
// is told it only of people whose busy times they may see
// (seesWhenOthersAreBusy, read once and only when someone else's mark is
// there), and the public invitation pages never carry it. A failed read
// marks nobody.
func (s *service) awayDecliners(ctx context.Context, reader uuid.UUID, rows []instance, occ map[string][]models.Attendee) map[string]map[string]bool {
	type mark struct {
		ref    string
		person uuid.UUID
	}
	var found []mark
	others := false
	for _, inst := range rows {
		if EventKind(inst.owner().Kind) == EventAway {
			continue
		}
		for _, a := range occ[inst.ref] {
			if a.Response == responseDeclined && a.ByAway && a.UserID != nil {
				found = append(found, mark{inst.ref, *a.UserID})
				others = others || *a.UserID != reader
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	sees := false
	if others {
		ok, err := s.seesWhenOthersAreBusy(ctx, reader)
		if err != nil {
			return nil
		}
		sees = ok
	}
	marks := map[string]map[string]bool{}
	for _, m := range found {
		if m.person != reader && !sees {
			continue
		}
		if marks[m.ref] == nil {
			marks[m.ref] = map[string]bool{}
		}
		marks[m.ref][m.person.String()] = true
	}
	return marks
}

// awaysWithRules is these people's out-of-office events in w and their rules:
// one read of each. A failed read is none (nobody is declined or marked).
func (s *service) awaysWithRules(ctx context.Context, people []uuid.UUID, w models.Window) ([]*models.Event, map[uuid.UUID]models.OOORule) {
	aways, err := s.store.awayOf(ctx, people, w, maxInstances)
	if err != nil || len(aways) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, 0, len(aways))
	for _, a := range aways {
		ids = append(ids, a.ID)
	}
	rules, err := s.store.oooRules(ctx, ids)
	if err != nil {
		return nil, nil
	}
	return aways, rules
}
