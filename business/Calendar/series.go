package Calendar

// Repeating events (calendar A7, on A1's one store and the Recurrence Engine
// #39). A series is one events row with an RFC 5545 rule; its instances are
// worked out on every read, in the series' own zone, so a 09:15 stand-up stays
// at 09:15 across a clock change. An instance changed by hand is an override
// row, keyed by the series and the start the instance has in it (its
// recurrence id), holding the whole instance and the names of the fields
// changed by hand; a deleted instance is a cancelled override in the trash.
//
// A change to an instance says its scope (PROGRAM.md D-CAL-6): this event,
// this and following, or all events. Only what the change changes goes
// further than the instance: all events moves every instance not moved by
// hand and keeps each override's own changes (EV-21: Google can lose them);
// this and following ends the series before the instance and starts a new
// series there, which takes the later overrides along. Deleting this and
// following does the same and puts the new series in the trash, so it can
// come back.
//
// A write to a series is checked against the series as read (its sequence and
// how many overrides it has), in the transaction, so two people changing one
// series at once never leave an override behind (models.LockSeries).

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	recurrence "github.com/akashc777/OneCamp/business/Recurrence"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Scopes of a change to an instance.
const (
	ScopeThis      = "this"
	ScopeFollowing = "following"
	ScopeAll       = "all"
)

// The kinds that may repeat. Repeating focus time and time off are B3's: the
// push gate and the workload read rows, not series.
var repeatingKinds = map[EventKind]bool{EventPlain: true, EventWorkingLocation: true, EventTaskBlock: true}

// Instance ids, as Google writes them: "<series id>_20261016T091500Z" for a
// timed series, "<series id>_20261016" for an all-day one.
const (
	ridTimed  = "20060102T150405Z"
	ridAllDay = "20060102"
)

// instanceRef is an instance's id.
func instanceRef(series uuid.UUID, rid time.Time, allDay bool) string {
	if allDay {
		return series.String() + "_" + rid.UTC().Format(ridAllDay)
	}
	return series.String() + "_" + rid.UTC().Format(ridTimed)
}

// parseRef reads an event's id or an instance's.
func parseRef(ref string) (uuid.UUID, *time.Time, error) {
	ref = strings.TrimSpace(ref)
	base, rest, isInstance := strings.Cut(ref, "_")
	id, err := uuid.Parse(base)
	if err != nil {
		return uuid.Nil, nil, inputError("That isn't an event id.")
	}
	if !isInstance {
		return id, nil, nil
	}
	for _, layout := range []string{ridTimed, ridAllDay} {
		if t, err := time.Parse(layout, rest); err == nil {
			u := t.UTC()
			return id, &u, nil
		}
	}
	return uuid.Nil, nil, inputError("That isn't an event id.")
}

// IsInstanceID is whether id names one instance of a repeating event.
func IsInstanceID(id string) bool {
	_, rid, err := parseRef(id)
	return err == nil && rid != nil
}

// instance is one occurrence as a reader sees it: a single event, or one
// instance of a series, from its override when it has one, else made from
// the series.
type instance struct {
	*models.Event
	// ref is the id it is known by.
	ref string
	// series is its series, nil for a single event; rid its start in it.
	series *models.Event
	rid    *time.Time
	// override is whether its fields are an override row's.
	override bool
}

func single(e *models.Event) instance { return instance{Event: e, ref: e.ID.String()} }

// owner is the row whose guests and access an instance has: its series', or
// its own.
func (i instance) owner() *models.Event {
	if i.series != nil {
		return i.series
	}
	return i.Event
}

// cloneEvent is a copy of a row that shares nothing with it.
func cloneEvent(e *models.Event) *models.Event {
	c := *e
	c.Changed = slices.Clone(e.Changed)
	c.Exdates = slices.Clone(e.Exdates)
	c.Rdates = slices.Clone(e.Rdates)
	return &c
}

// zoneOfRow is the zone an event's times are said in: UTC for an all-day one,
// whose dates are UTC midnights.
func zoneOfRow(e *models.Event) *time.Location {
	if e.AllDay() {
		return time.UTC
	}
	loc, err := loadZone(e.TZ)
	if err != nil {
		return time.UTC
	}
	return loc
}

// seriesTime is a series' first start, its zone, and how long each instance
// is: for an all-day series a date's midnight in UTC, in UTC, and its days.
func seriesTime(e *models.Event) (time.Time, *time.Location, time.Duration) {
	if e.AllDay() {
		start, end := utcMidnightOf(e.StartDate), utcMidnightOf(e.EndDate)
		return start, time.UTC, end.Sub(start)
	}
	return *e.StartAt, zoneOfRow(e), e.EndAt.Sub(*e.StartAt)
}

// ruleOf is a series' rule, read.
func ruleOf(e *models.Event) (recurrence.Rule, error) {
	return recurrence.Rules.Parse(e.RRule)
}

// expandOptions are a series' own exceptions, for the engine.
func expandOptions(e *models.Event, limit int) recurrence.Options {
	return recurrence.Options{Exdates: e.Exdates, Rdates: e.Rdates, Limit: limit}
}

// isInstance is whether rid is one of a series' instances.
func isInstance(e *models.Event, rid time.Time) bool {
	if e.RRule == "" {
		return false
	}
	r, err := ruleOf(e)
	if err != nil {
		return false
	}
	start, loc, _ := seriesTime(e)
	got := recurrence.Rules.Expand(r, start, loc, rid, rid, expandOptions(e, 1))
	return len(got) == 1 && got[0].Equal(rid)
}

// madeInstance is a series' instance at rid, as the series says it.
func madeInstance(series *models.Event, rid time.Time) instance {
	c := cloneEvent(series)
	_, _, dur := seriesTime(series)
	placeAt(c, rid, dur, series.AllDay())
	c.RRule, c.UntilAt, c.Exdates, c.Rdates, c.Changed = "", nil, []time.Time{}, []time.Time{}, []string{}
	r := rid
	return instance{Event: c, ref: instanceRef(series.ID, rid, series.AllDay()), series: series, rid: &r}
}

// placeAt puts an instance at start, lasting dur (days for an all-day one).
func placeAt(e *models.Event, start time.Time, dur time.Duration, allDay bool) {
	if allDay {
		e.StartAt, e.EndAt = nil, nil
		e.StartDate, e.EndDate = start.UTC().Format(dateLayout), start.UTC().Add(dur).Format(dateLayout)
		return
	}
	s, en := start.UTC(), start.UTC().Add(dur)
	e.StartAt, e.EndAt = &s, &en
	e.StartDate, e.EndDate = "", ""
}

// sameWhen is whether two rows are at the same times.
func sameWhen(a, b *models.Event) bool {
	return sameTime(a.StartAt, b.StartAt) && sameTime(a.EndAt, b.EndAt) && a.StartDate == b.StartDate && a.EndDate == b.EndDate
}

// inWindow is whether an event's own time touches w, its bounds counted in.
func inWindow(e *models.Event, w models.Window) bool {
	if e.AllDay() {
		return e.StartDate <= w.ToDate && e.EndDate >= w.FromDate
	}
	return !e.StartAt.After(w.To) && !e.EndAt.Before(w.From)
}

// live is whether an override shows: not deleted, nor cancelled.
func live(o *models.Event) bool { return o.DeletedAt == nil && o.Status != statusCancelled }

// instancesOf is what rows give in w: single events as they are, and each
// series' instances, from its overrides where it has them. At most limit,
// earliest first.
func (s *service) instancesOf(ctx context.Context, rows []*models.Event, w models.Window, limit int) ([]instance, error) {
	overrides, err := s.overridesFor(ctx, rows, w)
	if err != nil {
		return nil, err
	}
	return expandInstances(ctx, rows, overrides, w, limit), nil
}

// seriesOverrides are series' overrides, by series and then by the
// instance's recurrence id (UnixNano).
type seriesOverrides map[uuid.UUID]map[int64]*models.Event

// overridesFor reads, in one read, the overrides of rows' series that can
// show in w: back as far as the longest instance, so one that started before
// w and runs into it is found too.
func (s *service) overridesFor(ctx context.Context, rows []*models.Event, w models.Window) (seriesOverrides, error) {
	var seriesIDs []uuid.UUID
	reach := w
	for _, e := range rows {
		if e.RRule != "" {
			seriesIDs = append(seriesIDs, e.ID)
			if _, _, dur := seriesTime(e); w.From.Add(-dur).Before(reach.From) {
				reach.From = w.From.Add(-dur)
			}
		}
	}
	overrides := seriesOverrides{}
	if len(seriesIDs) == 0 {
		return overrides, nil
	}
	ovs, err := s.store.overridesOf(ctx, seriesIDs, reach)
	if err != nil {
		return nil, err
	}
	for _, o := range ovs {
		if overrides[*o.SeriesID] == nil {
			overrides[*o.SeriesID] = map[int64]*models.Event{}
		}
		overrides[*o.SeriesID][o.RecurrenceID.UnixNano()] = o
	}
	return overrides, nil
}

// expandInstances is instancesOf once the overrides are read: no store, so
// a caller can read many calendars' rows and overrides once, then expand
// each calendar's rows apart.
func expandInstances(ctx context.Context, rows []*models.Event, overrides seriesOverrides, w models.Window, limit int) []instance {
	var out []instance
	for _, e := range rows {
		if e.RRule == "" {
			out = append(out, single(e))
			continue
		}
		r, err := ruleOf(e)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: series %s's rule can't be read: %v", e.ID, err)
			continue
		}
		start, loc, dur := seriesTime(e)
		from, to := w.From.Add(-dur), w.To
		if e.AllDay() {
			from, to = utcMidnightOf(w.FromDate).Add(-dur), utcMidnightOf(w.ToDate)
		}
		used := map[int64]bool{}
		for _, rid := range recurrence.Rules.Expand(r, start, loc, from, to, expandOptions(e, limit)) {
			if o := overrides[e.ID][rid.UnixNano()]; o != nil {
				used[rid.UnixNano()] = true
				if live(o) && inWindow(o, w) {
					out = append(out, instance{Event: o, ref: instanceRef(e.ID, rid, e.AllDay()), series: e, rid: o.RecurrenceID, override: true})
				}
				continue
			}
			if made := madeInstance(e, rid); inWindow(made.Event, w) {
				out = append(out, made)
			}
		}
		// An instance moved into the window from outside it.
		for key, o := range overrides[e.ID] {
			if used[key] || !live(o) || !inWindow(o, w) || !isInstance(e, *o.RecurrenceID) {
				continue
			}
			out = append(out, instance{Event: o, ref: instanceRef(e.ID, *o.RecurrenceID, e.AllDay()), series: e, rid: o.RecurrenceID, override: true})
		}
	}
	slices.SortStableFunc(out, func(a, b instance) int {
		as, _ := spanOf(a.Event)
		bs, _ := spanOf(b.Event)
		return as.Compare(bs)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// resolve is the occurrence an id names, and its owner's guests, for someone
// on it: an event, a series (by its own id) or one of its instances.
func (s *service) resolve(ctx context.Context, who *model.UserInfo, ref string) (instance, []models.Attendee, error) {
	id, rid, err := parseRef(ref)
	if err != nil {
		return instance{}, nil, err
	}
	e, guests, err := s.load(ctx, id)
	if err != nil {
		return instance{}, nil, err
	}
	if rid == nil && e.SeriesID != nil {
		// An override's own id: the instance it is.
		return s.resolve(ctx, who, instanceRef(*e.SeriesID, *e.RecurrenceID, false))
	}
	if roleOf(e, guests, personOf(who)) == roleNone {
		return instance{}, nil, ErrEventNotFound
	}
	if rid == nil {
		return single(e), guests, nil
	}
	if e.RRule == "" {
		return instance{}, nil, ErrEventNotFound
	}
	o, err := s.store.overrideAt(ctx, e.ID, *rid)
	if err != nil {
		return instance{}, nil, err
	}
	if o != nil {
		if !live(o) {
			return instance{}, nil, ErrEventNotFound
		}
		return instance{Event: o, ref: instanceRef(e.ID, *rid, e.AllDay()), series: e, rid: o.RecurrenceID, override: true}, guests, nil
	}
	if !isInstance(e, *rid) {
		return instance{}, nil, ErrEventNotFound
	}
	return madeInstance(e, *rid), guests, nil
}

// setRule makes e repeat by text ("" for not at all), checked, with the
// bound a read of a range needs (until_at).
func setRule(e *models.Event, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		e.RRule, e.UntilAt = "", nil
		return nil
	}
	if !repeatingKinds[EventKind(e.Kind)] {
		return inputError("Focus time, time off, bookings and reminders don't repeat yet.")
	}
	if e.SeriesID != nil {
		return inputError("One instance of a repeating event doesn't repeat on its own.")
	}
	r, err := recurrence.Rules.Parse(text)
	if err != nil {
		return inputError(err.Error())
	}
	start, loc, _ := seriesTime(e)
	// An end written as the other kind is read as people mean it, and kept
	// as the RFC writes it: a date for an all-day series, the end of that day
	// in its zone, in UTC, for a timed one.
	switch {
	case r.Until != nil && r.UntilDate && !e.AllDay():
		d := r.Until.UTC()
		end := time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc).UTC()
		r.Until, r.UntilDate = &end, false
	case r.Until != nil && !r.UntilDate && e.AllDay():
		d := r.Until.UTC()
		day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		r.Until, r.UntilDate = &day, true
	}
	if r.Until != nil && r.Until.Before(start) {
		return inputError("A repeat can't end before the event starts.")
	}
	e.RRule = r.String()
	if last, ends := recurrence.Last(r, start, loc, expandOptions(e, 0)); ends {
		e.UntilAt = &last
	} else {
		e.UntilAt = nil
	}
	return nil
}

// sameRule is whether text says the rule a series has.
func sameRule(text, rrule string) bool {
	text = strings.TrimSpace(text)
	if text == "" || rrule == "" {
		return text == rrule
	}
	r, err := recurrence.Rules.Parse(text)
	return err == nil && r.String() == rrule
}

// fieldsOf are an instance's fields a reader can change, by the names an
// override records in changed.
var fieldsOf = []struct {
	name string
	same func(a, b *models.Event) bool
	copy func(dst, src *models.Event)
}{
	{"title", func(a, b *models.Event) bool { return a.Title == b.Title }, func(d, s *models.Event) { d.Title = s.Title }},
	{"notes", func(a, b *models.Event) bool { return a.Notes == b.Notes && a.NotesFormat == b.NotesFormat },
		func(d, s *models.Event) { d.Notes, d.NotesFormat = s.Notes, s.NotesFormat }},
	{"location", func(a, b *models.Event) bool { return a.Location == b.Location }, func(d, s *models.Event) { d.Location = s.Location }},
	{"show_as", func(a, b *models.Event) bool { return a.Transparency == b.Transparency }, func(d, s *models.Event) { d.Transparency = s.Transparency }},
	{"visibility", func(a, b *models.Event) bool { return a.Visibility == b.Visibility }, func(d, s *models.Event) { d.Visibility = s.Visibility }},
	{"colour", func(a, b *models.Event) bool { return a.Hue == b.Hue }, func(d, s *models.Event) { d.Hue = s.Hue }},
}

// changesOf names what next says differently from was: fieldsOf's names, and
// "time". Pure.
func changesOf(was, next *models.Event) []string {
	var out []string
	for _, f := range fieldsOf {
		if !f.same(was, next) {
			out = append(out, f.name)
		}
	}
	if !sameWhen(was, next) || (!next.AllDay() && was.TZ != next.TZ) {
		out = append(out, "time")
	}
	return out
}

// takeChanges sets on dst what src says of the fields named. Pure.
func takeChanges(dst, src *models.Event, names []string) {
	for _, f := range fieldsOf {
		if slices.Contains(names, f.name) {
			f.copy(dst, src)
		}
	}
	if slices.Contains(names, "time") {
		dst.StartAt, dst.EndAt, dst.StartDate, dst.EndDate = src.StartAt, src.EndAt, src.StartDate, src.EndDate
		dst.TZ, dst.EndTZ = src.TZ, src.EndTZ
	}
}

// permissionsDiffer is whether two rows let guests do different things.
func permissionsDiffer(a, b *models.Event) bool {
	return a.GuestsCanModify != b.GuestsCanModify || a.GuestsCanInvite != b.GuestsCanInvite || a.GuestsCanSeeGuests != b.GuestsCanSeeGuests
}

// following brings an override up to date with its series: every field it
// didn't change by hand takes the series', and, unless its time was changed
// by hand, it sits at its instance's start for the series' length.
func following(o, series *models.Event, rid time.Time) {
	for _, f := range fieldsOf {
		if !slices.Contains(o.Changed, f.name) {
			f.copy(o, series)
		}
	}
	o.Kind, o.ICalUID = series.Kind, series.ICalUID
	o.GuestsCanModify, o.GuestsCanInvite, o.GuestsCanSeeGuests = series.GuestsCanModify, series.GuestsCanInvite, series.GuestsCanSeeGuests
	if !slices.Contains(o.Changed, "time") {
		_, _, dur := seriesTime(series)
		placeAt(o, rid, dur, series.AllDay())
		o.TZ, o.EndTZ = series.TZ, ""
	}
	r := rid
	o.RecurrenceID = &r
}

// dayOf is the date t falls on in loc, as that date's midnight in UTC.
func dayOf(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
}

// daysBetween is how many days on b's date (in bLoc) is from a's (in aLoc).
func daysBetween(a time.Time, aLoc *time.Location, b time.Time, bLoc *time.Location) int {
	return int(dayOf(b, bLoc).Sub(dayOf(a, aLoc)).Hours() / 24)
}

// rekey is where an instance at rid goes when its series' start moves from
// oldStart (said in oldLoc) to newStart (in newLoc): as many days on as the
// start moved, at the new start's time of day. Pure.
func rekey(rid, oldStart time.Time, oldLoc *time.Location, newStart time.Time, newLoc *time.Location) time.Time {
	d := dayOf(rid, oldLoc).AddDate(0, 0, daysBetween(oldStart, oldLoc, newStart, newLoc))
	h, mi, sec := newStart.In(newLoc).Clock()
	return time.Date(d.Year(), d.Month(), d.Day(), h, mi, sec, 0, newLoc).UTC()
}

// moveRule moves the weekdays and month days of e's rule by as many days as
// its first start moved, from oldStart (in oldLoc) to newStart (in newLoc):
// a weekly Tuesday series moved to a Wednesday repeats on Wednesdays.
func moveRule(e *models.Event, oldStart time.Time, oldLoc *time.Location, newStart time.Time, newLoc *time.Location) error {
	days := daysBetween(oldStart, oldLoc, newStart, newLoc)
	if e.RRule == "" || days == 0 {
		return nil
	}
	r, err := ruleOf(e)
	if err != nil {
		return inputError(err.Error())
	}
	return setRule(e, recurrence.Shift(r, days).String())
}

// splitTimes cuts a series' own dates at at: those before it, and those from
// it on, moved by move.
func splitTimes(ts []time.Time, at time.Time, move func(time.Time) time.Time) (before, from []time.Time) {
	before, from = []time.Time{}, []time.Time{}
	for _, t := range ts {
		if t.Before(at) {
			before = append(before, t)
		} else {
			from = append(from, move(t))
		}
	}
	return before, from
}

// seriesEdit is the instance a change to a whole series was made from, by
// its key before the change, and the fields the change set on it: they
// follow the series from now on, whatever it changed by hand before.
type seriesEdit struct {
	rid     time.Time
	release []string
}

// carry is a series' overrides keyed from at on, brought along a change to
// next from a series starting at oldStart (in oldLoc): re-keyed where the
// start moved, refreshed where they weren't changed by hand, and to the
// trash when next has no such instance.
func carry(ovs []*models.Event, next *models.Event, oldStart time.Time, oldLoc *time.Location, at time.Time, edit *seriesEdit, now time.Time) ([]rowUpdate, []uuid.UUID) {
	newStart, newLoc, _ := seriesTime(next)
	var updates []rowUpdate
	var trash []uuid.UUID
	for _, o := range ovs {
		if o.RecurrenceID.Before(at) {
			continue
		}
		rid := *o.RecurrenceID
		if !oldStart.Equal(newStart) || oldLoc.String() != newLoc.String() {
			rid = rekey(rid, oldStart, oldLoc, newStart, newLoc)
		}
		if !isInstance(next, rid) {
			if o.DeletedAt == nil {
				trash = append(trash, o.ID)
			}
			continue
		}
		moved := cloneEvent(o)
		moved.SeriesID = &next.ID
		if edit != nil && o.RecurrenceID.Equal(edit.rid) {
			moved.Changed = slices.DeleteFunc(moved.Changed, func(name string) bool { return slices.Contains(edit.release, name) })
		}
		following(moved, next, rid)
		moved.UpdatedAt = now
		updates = append(updates, rowUpdate{e: moved})
	}
	return updates, trash
}

// overridesToCarry is a series' overrides, for a change to all of it, or
// words when it has too many to carry in one write.
func (s *service) overridesToCarry(ctx context.Context, series uuid.UUID) ([]*models.Event, error) {
	ovs, err := s.store.overridesOfSeries(ctx, series)
	if err != nil {
		return nil, err
	}
	if len(ovs) >= maxOverrides {
		return nil, inputError("This repeating event has too many changed instances to change all at once. Change it from a later instance on.")
	}
	return ovs, nil
}

// updateSeries changes a whole series to next (from cur): its overrides come
// along, and its guests change when wanted says so. A series that stops
// repeating is its first instance: that one's own changes stay on it (but
// for what this change sets), and the other overrides go to the trash.
func (s *service) updateSeries(ctx context.Context, who *model.UserInfo, cur, next *models.Event, guests []models.Attendee, wanted *[]GuestInput, expect *int, edit *seriesEdit) (*models.Event, []models.Attendee, error) {
	if err := mayEdit(roleOf(cur, guests, personOf(who)), cur, next, guests, wanted); err != nil {
		return nil, nil, err
	}
	if err := check(next); err != nil {
		return nil, nil, err
	}
	ovs, err := s.overridesToCarry(ctx, cur.ID)
	if err != nil {
		return nil, nil, err
	}
	now, me := s.now(), personOf(who)
	next.ID, next.UpdatedAt = cur.ID, now
	var updates []rowUpdate
	var trash []uuid.UUID
	var folded []uuid.UUID
	if next.RRule == "" {
		first, _, _ := seriesTime(cur)
		for _, o := range ovs {
			if o.DeletedAt != nil {
				continue
			}
			if o.RecurrenceID.Equal(first) && live(o) {
				// The event it becomes is this instance: its changes move onto
				// the event's row, and the override, empty now, goes.
				keep := slices.DeleteFunc(slices.Clone(o.Changed), func(name string) bool {
					return edit != nil && edit.rid.Equal(first) && slices.Contains(edit.release, name)
				})
				takeChanges(next, o, keep)
				folded = append(folded, o.ID)
				continue
			}
			trash = append(trash, o.ID)
		}
	} else {
		oldStart, oldLoc, _ := seriesTime(cur)
		updates, trash = carry(ovs, next, oldStart, oldLoc, time.Time{}, edit, now)
	}
	g, err := s.guestsFor(ctx, cur, guests, wanted, now)
	if err != nil {
		return nil, nil, err
	}
	read := cur.Sequence
	b := batch{updates: append([]rowUpdate{{e: next, expect: expect}}, updates...), guests: g.add, trash: trash, folded: folded, now: now, by: &me,
		guard: &seriesGuard{id: cur.ID, sequence: &read, overrides: len(ovs)}}
	if len(g.remove) > 0 {
		b.drop = map[uuid.UUID][]uuid.UUID{cur.ID: g.remove}
	}
	if len(g.removeEmails) > 0 {
		b.dropEmails = map[uuid.UUID][]string{cur.ID: g.removeEmails}
	}
	b.roles = g.roles
	if err := s.applySeries(ctx, who, cur.ID, b); err != nil {
		return nil, nil, err
	}
	updated, after, err := s.load(ctx, cur.ID)
	if err != nil {
		return nil, nil, err
	}
	removed := helpers.UUIDStrings(g.remove)
	changed := changedFields(cur, updated, g.changed())
	if cur.RRule != updated.RRule {
		changed = append(changed, "recurrence")
	}
	s.announce(ctx, who, resourceEventBusiness.CalendarEventUpdated, updated, append(guestIDs(after), removed...),
		map[string]string{"changed": strings.Join(changed, ","), "scope": ScopeAll})
	s.effects.updated(ctx, who, cur, updated, guests, after, wanted != nil, removed)
	s.tells.tell(ctx, who, telling{what: toldChanged, event: updated, was: cur, now: updated, guestsBefore: guests, guestsAfter: after, changed: changed})
	return updated, after, nil
}

// applySeries writes a batch for a series, saying why it couldn't.
func (s *service) applySeries(ctx context.Context, who *model.UserInfo, series uuid.UUID, b batch) error {
	err := s.store.apply(ctx, b)
	switch {
	case errors.Is(err, models.ErrStale):
		return s.stale(ctx, who, series)
	case errors.Is(err, models.ErrNotFound):
		return ErrEventNotFound
	}
	return err
}

// newOverride makes row the first override of an instance made from its
// series. Its sequence goes on from the one the instance had (the series'),
// so a writer who read the instance before it was changed is refused.
func (s *service) newOverride(row *models.Event, inst instance, now time.Time) {
	series := inst.series
	rid := *inst.rid
	row.ID, row.SeriesID, row.RecurrenceID = s.newID(), &series.ID, &rid
	row.CalendarID, row.CreatedBy, row.ICalUID = series.CalendarID, series.CreatedBy, series.ICalUID
	row.RRule, row.UntilAt, row.Exdates, row.Rdates = "", nil, []time.Time{}, []time.Time{}
	row.Source, row.Sequence, row.CreatedAt, row.GoogleEventID = sourceOneCamp, series.Sequence+1, now, ""
}

// instanceWrite is a write of row for one instance: its override's update,
// at the sequence the writer read (the override's own, when the writer named
// none, so a change to the whole series meanwhile is never undone), or its
// first override, written while the series is as the writer read it.
func instanceWrite(inst instance, row *models.Event, expect *int) (batch, error) {
	if inst.override {
		if expect == nil {
			read := inst.Sequence
			expect = &read
		}
		return batch{updates: []rowUpdate{{e: row, expect: expect}}}, nil
	}
	if expect != nil && inst.series.Sequence != *expect {
		return batch{}, models.ErrStale
	}
	seq := inst.series.Sequence
	return batch{inserts: []*models.Event{row}, guard: &seriesGuard{id: inst.series.ID, sequence: &seq, overrides: -1}}, nil
}

// applyInstance writes a batch for one instance, saying why it couldn't: an
// instance someone else changed first is stale, as is one whose first
// override someone else wrote at the same moment.
func (s *service) applyInstance(ctx context.Context, who *model.UserInfo, inst instance, b batch, err error) error {
	if err == nil {
		err = s.store.apply(ctx, b)
	}
	if err == nil {
		return nil
	}
	if !errors.Is(err, models.ErrStale) && !inst.override {
		if o, oerr := s.store.overrideAt(ctx, inst.series.ID, *inst.rid); oerr == nil && o != nil {
			err = models.ErrStale
		}
	}
	switch {
	case errors.Is(err, models.ErrStale):
		return s.staleAt(ctx, who, inst.ref)
	case errors.Is(err, models.ErrNotFound):
		return ErrEventNotFound
	}
	return err
}

// updateThis changes one instance of a series: its override, made the first
// time. What it says differently from its series is what it changed by
// hand.
func (s *service) updateThis(ctx context.Context, who *model.UserInfo, inst instance, guests []models.Attendee, next *models.Event, expect *int) (*models.Event, error) {
	series := inst.series
	if err := mayChange(roleOf(series, guests, personOf(who)), series); err != nil {
		return nil, err
	}
	if next.Kind != series.Kind {
		return nil, inputError("Change what a repeating event is for this and following events, or for all of them.")
	}
	if permissionsDiffer(next, series) {
		return nil, inputError("Change what guests may do for all events.")
	}
	if err := check(next); err != nil {
		return nil, err
	}
	now := s.now()
	next.Changed = changesOf(madeInstance(series, *inst.rid).Event, next)
	next.UpdatedAt = now
	if !inst.override {
		s.newOverride(next, inst, now)
	}
	b, err := instanceWrite(inst, next, expect)
	b.now = now
	if err := s.applyInstance(ctx, who, inst, b, err); err != nil {
		return nil, err
	}
	changed := changedFields(inst.Event, next, false)
	s.announce(ctx, who, resourceEventBusiness.CalendarEventUpdated, series, guestIDs(guests), map[string]string{
		"changed": strings.Join(changed, ","), "scope": ScopeThis, "instance": inst.ref})
	s.tells.tell(ctx, who, telling{what: toldChanged, event: series, was: inst.Event, now: next, ref: inst.ref,
		guestsBefore: guests, guestsAfter: guests, changed: changed})
	return next, nil
}

// split is "this and following" from inst, as one batch: the series ends
// before the instance, and fresh, a new series made from next, starts there,
// repeating by rule (nil: as the series would have from there on; "": not at
// all) with guests, taking the later overrides along. A fresh event that
// doesn't repeat takes the instance's own override, if it has one, as its
// row.
func (s *service) split(ctx context.Context, who *model.UserInfo, inst instance, next *models.Event, rule *string, guests []models.Attendee, edit *seriesEdit, expect *int) (batch, *models.Event, error) {
	series := inst.series
	start, loc, _ := seriesTime(series)
	at := *inst.rid
	r, err := ruleOf(series)
	if err != nil {
		return batch{}, nil, err
	}
	ovs, err := s.overridesToCarry(ctx, series.ID)
	if err != nil {
		return batch{}, nil, err
	}
	now, me := s.now(), personOf(who)

	ended := cloneEvent(series)
	ended.UpdatedAt = now
	if err := setRule(ended, recurrence.Before(r, start, loc, at, series.AllDay()).String()); err != nil {
		return batch{}, nil, err
	}

	fresh := cloneEvent(next)
	fresh.ID, fresh.SeriesID, fresh.RecurrenceID = s.newID(), nil, nil
	fresh.ICalUID = fresh.ID.String() + "@onecamp"
	fresh.CalendarID, fresh.CreatedBy = series.CalendarID, series.CreatedBy
	fresh.Source, fresh.Sequence, fresh.CreatedAt, fresh.UpdatedAt, fresh.Changed = sourceOneCamp, 0, now, now, []string{}
	fresh.DeletedAt, fresh.DeletedBy, fresh.GoogleEventID = nil, nil, ""
	freshStart, freshLoc, _ := seriesTime(fresh)
	move := func(t time.Time) time.Time { return rekey(t, at, loc, freshStart, freshLoc) }
	ended.Exdates, fresh.Exdates = splitTimes(series.Exdates, at, move)
	ended.Rdates, fresh.Rdates = splitTimes(series.Rdates, at, move)
	if rule == nil {
		fresh.RRule = recurrence.From(r, start, loc, at).String()
		if err := moveRule(fresh, at, loc, freshStart, freshLoc); err != nil {
			return batch{}, nil, err
		}
		if err := setRule(fresh, fresh.RRule); err != nil {
			return batch{}, nil, err
		}
	} else if err := setRule(fresh, *rule); err != nil {
		return batch{}, nil, err
	}
	if err := check(fresh); err != nil {
		return batch{}, nil, err
	}
	read := series.Sequence
	b := batch{updates: []rowUpdate{{e: ended, expect: expect}}, now: now, by: &me,
		guard: &seriesGuard{id: series.ID, sequence: &read, overrides: len(ovs)}}
	if fresh.RRule == "" {
		// Not repeating from here: the instance on its own. Its override, if
		// it has one, becomes it, keeping what it changed by hand that this
		// change doesn't set; the later ones have no instance left.
		var own *models.Event
		for _, o := range ovs {
			if o.RecurrenceID.Equal(at) && o.DeletedAt == nil {
				own = o
			}
		}
		if own != nil {
			keep := slices.DeleteFunc(slices.Clone(own.Changed), func(name string) bool { return edit != nil && slices.Contains(edit.release, name) })
			takeChanges(fresh, own, keep)
			fresh.ID, fresh.CreatedAt = own.ID, own.CreatedAt
			fresh.ICalUID = fresh.ID.String() + "@onecamp"
			b.updates = append(b.updates, rowUpdate{e: fresh})
		} else {
			b.inserts = []*models.Event{fresh}
		}
		for _, o := range ovs {
			if !o.RecurrenceID.Before(at) && o.DeletedAt == nil && (own == nil || o.ID != own.ID) {
				b.trash = append(b.trash, o.ID)
			}
		}
	} else {
		// The new series is written before its overrides move onto it (apply
		// writes new rows first).
		b.inserts = []*models.Event{fresh}
		updates, trash := carry(ovs, fresh, at, loc, at, edit, now)
		b.updates, b.trash = append(b.updates, updates...), trash
	}
	copied, err := copyGuests(guests, fresh.ID)
	if err != nil {
		return batch{}, nil, err
	}
	b.guests = append(b.guests, copied...)
	return b, fresh, nil
}

// splitAt is "this and following" from inst, changed to next: the series
// ends before it, and a new series starts there with the guests wanted (the
// series' own when nil).
func (s *service) splitAt(ctx context.Context, who *model.UserInfo, inst instance, guests []models.Attendee, next *models.Event, rule *string, wanted *[]GuestInput, expect *int, edit *seriesEdit) (*models.Event, error) {
	series := inst.series
	if err := mayChange(roleOf(series, guests, personOf(who)), series); err != nil {
		return nil, err
	}
	g, err := s.guestsFor(ctx, series, guests, wanted, s.now())
	if err != nil {
		return nil, err
	}
	b, fresh, err := s.split(ctx, who, inst, next, rule, g.after, edit, expect)
	if err != nil {
		return nil, err
	}
	if err := s.applySeries(ctx, who, series.ID, b); err != nil {
		return nil, err
	}
	removed := helpers.UUIDStrings(g.remove)
	s.announce(ctx, who, resourceEventBusiness.CalendarEventUpdated, series, append(guestIDs(guests), removed...),
		map[string]string{"changed": "recurrence", "scope": ScopeFollowing, "instance": inst.ref})
	s.announce(ctx, who, resourceEventBusiness.CalendarEventCreated, fresh, guestIDs(g.after), map[string]string{"from": series.ID.String()})
	if wanted != nil {
		s.effects.updated(ctx, who, series, fresh, guests, g.after, true, removed)
	}
	s.tellSplit(ctx, who, inst, series, fresh, guests, g.after)
	return fresh, nil
}

// deleteThis puts one instance in the trash: a cancelled override.
func (s *service) deleteThis(ctx context.Context, who *model.UserInfo, inst instance, guests []models.Attendee, expect *int) error {
	switch roleOf(inst.series, guests, personOf(who)) {
	case roleOrganiser:
	case roleGuest:
		return ErrNotEventCreator
	case roleReader:
		return ErrSubscribedEvent
	default:
		return ErrEventNotFound
	}
	now, me := s.now(), personOf(who)
	gone := cloneEvent(inst.Event)
	gone.Status, gone.UpdatedAt = statusCancelled, now
	if !inst.override {
		s.newOverride(gone, inst, now)
		gone.DeletedAt, gone.DeletedBy = &now, &me
	}
	b, err := instanceWrite(inst, gone, expect)
	if inst.override {
		b.trash = []uuid.UUID{gone.ID}
	}
	b.now, b.by = now, &me
	if err := s.applyInstance(ctx, who, inst, b, err); err != nil {
		return err
	}
	s.announce(ctx, who, resourceEventBusiness.CalendarEventCancelled, inst.series, guestIDs(guests), map[string]string{"scope": ScopeThis, "instance": inst.ref})
	if len(guests) > 0 {
		s.tells.tell(ctx, who, telling{what: toldCancelled, event: inst.series, now: inst.Event, ref: inst.ref, guestsBefore: guests, guestsAfter: guests})
	}
	return nil
}

// deleteFollowing ends a series before an instance: the instances from it on
// become a series of their own, in the trash, to come back from there. From
// the first instance, the whole series goes to the trash.
func (s *service) deleteFollowing(ctx context.Context, who *model.UserInfo, inst instance, guests []models.Attendee, expect *int) error {
	series := inst.series
	if start, _, _ := seriesTime(series); inst.rid.Equal(start) {
		return s.remove(ctx, who, series, guests, expect)
	}
	switch roleOf(series, guests, personOf(who)) {
	case roleOrganiser:
	case roleGuest:
		return ErrNotEventCreator
	case roleReader:
		return ErrSubscribedEvent
	default:
		return ErrEventNotFound
	}
	b, fresh, err := s.split(ctx, who, inst, madeInstance(series, *inst.rid).Event, nil, guests, nil, expect)
	if err != nil {
		return err
	}
	b.trash = append(b.trash, fresh.ID)
	if err := s.applySeries(ctx, who, series.ID, b); err != nil {
		return err
	}
	s.announce(ctx, who, resourceEventBusiness.CalendarEventCancelled, series, guestIDs(guests), map[string]string{"scope": ScopeFollowing, "instance": inst.ref})
	if len(guests) > 0 {
		// The series as it is now, ending before the instance.
		if ended, _, err := s.load(ctx, series.ID); err == nil {
			s.tells.tell(ctx, who, telling{what: toldCancelled, event: ended, now: inst.Event, ref: inst.ref, guestsBefore: guests, guestsAfter: guests})
		}
	}
	return nil
}

// restoreThis brings a deleted instance back from the trash: as the instance
// it was, or, when its series no longer has it, as an event on its own with
// the series' guests.
func (s *service) restoreThis(ctx context.Context, who *model.UserInfo, series *models.Event, guests []models.Attendee, rid time.Time) (*models.Event, error) {
	if series.CreatedBy != personOf(who) {
		return nil, ErrEventNotFound
	}
	o, err := s.store.overrideAt(ctx, series.ID, rid)
	if err != nil {
		return nil, err
	}
	if o == nil || o.DeletedAt == nil {
		return nil, ErrEventNotFound
	}
	now := s.now()
	back := cloneEvent(o)
	back.Status, back.UpdatedAt = statusConfirmed, now
	seq := series.Sequence
	b := batch{updates: []rowUpdate{{e: back}}, restore: []uuid.UUID{o.ID}, now: now,
		guard: &seriesGuard{id: series.ID, sequence: &seq, overrides: -1}}
	if !isInstance(series, rid) {
		back.SeriesID, back.RecurrenceID, back.Changed = nil, nil, []string{}
		back.ICalUID = back.ID.String() + "@onecamp"
		copied, err := copyGuests(guests, back.ID)
		if err != nil {
			return nil, err
		}
		b.guests = copied
	}
	if err := s.applySeries(ctx, who, series.ID, b); err != nil {
		return nil, err
	}
	back.DeletedAt, back.DeletedBy = nil, nil
	return back, nil
}

// tellSplit tells of "this and following": the series now ends before the
// instance, and a new one carries on from it with the guests asked for.
// Members hear once, of the new one; guests by email are sent both, so their
// calendars end the old one and carry the new one.
func (s *service) tellSplit(ctx context.Context, who *model.UserInfo, inst instance, series, fresh *models.Event, before, after []models.Attendee) {
	if len(before)+len(after) == 0 {
		return
	}
	if ended, endedGuests, err := s.load(ctx, series.ID); err == nil {
		s.tells.tell(ctx, who, telling{what: toldChanged, event: ended, was: series, now: ended, guestsBefore: endedGuests,
			guestsAfter: endedGuests, changed: []string{"recurrence"}, outsidersOnly: true})
	}
	got, freshGuests, err := s.load(ctx, fresh.ID)
	if err != nil {
		return
	}
	changed := append(changedFields(inst.Event, got, false), "recurrence")
	s.tells.tell(ctx, who, telling{what: toldChanged, event: got, was: inst.Event, now: got, guestsBefore: before, guestsAfter: freshGuests,
		changed: changed, splitTail: true})
}
