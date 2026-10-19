package Calendar

// Reminders (calendar programme A5): an event reminds the people on it, the
// way Google's pop-up does, but on every device.
//
//   - Each person's reminders are their own, for an event or a whole series:
//     up to five, minutes before it starts (an all-day event's, before 00:00
//     of its first day in their zone). With none set, the default reminds
//     whoever organises it or said yes or maybe: 10 minutes before, or 09:00
//     the day before an all-day event (D-CAL-4). Nobody is reminded of what
//     they declined, for the whole series or that one instance.
//   - When each person's next reminder fires is one waiting job in the queue
//     every delayed piece of work uses (business/Scheduler, scheduled_jobs):
//     it fires, then names the next. A change to the event, its guests, an
//     answer or a person's reminders plans again, from its Resource Event;
//     an hourly pass plans what a write made with nobody told (an import,
//     the move), but never the demo's seeded events: its visitor is shared.
//   - A reminder of an event someone organises or said yes to reaches them
//     through a pause, quiet hours and focus time (D-CAL-4); anyone else's
//     keeps to their holds. It goes to their devices and open apps, never
//     their inbox, and a reminder found late (the server was down) isn't
//     sent at all.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const (
	// maxReminders is how many reminders a person may set for one event.
	maxReminders = 5
	// The furthest a reminder may come before an event (four weeks), and,
	// for an all-day event, the latest on its day (23:59).
	maxReminderMinutes = 4 * 7 * 24 * 60
	minReminderMinutes = -(24*60 - 1)
	// reminderHorizon is how far ahead a plan looks for someone's next
	// reminder; a series with none that soon is planned again from its edge.
	reminderHorizon = 35 * 24 * time.Hour
	// reminderLate is how late a reminder may be and still be sent: one
	// found later (the server was down) is skipped, never sent stale.
	reminderLate = 15 * time.Minute
	// reminderPlanLimit bounds the instances one plan reads.
	reminderPlanLimit = 400
)

// The defaults (D-CAL-4): 10 minutes before a timed event, 09:00 the day
// before an all-day one.
var (
	defaultTimedReminders  = []int{10}
	defaultAllDayReminders = []int{15 * 60}
)

// reminderStore is where reminders and their jobs are kept (models
// reminderModel.go): a seam of its own, so a test keeps them in memory.
type reminderStore interface {
	remindersOf(ctx context.Context, event uuid.UUID, people []uuid.UUID) (map[uuid.UUID][]int, error)
	remindersFor(ctx context.Context, person uuid.UUID, events []uuid.UUID) (map[uuid.UUID][]int, error)
	setReminders(ctx context.Context, event, person uuid.UUID, minutes []int, at time.Time) error
	planReminder(ctx context.Context, person uuid.UUID, p models.ReminderPayload, at time.Time) error
	cancelReminders(ctx context.Context, event uuid.UUID, people []uuid.UUID, kind string) error
	cancelRemindersExcept(ctx context.Context, event uuid.UUID, keep []uuid.UUID) error
	unplanned(ctx context.Context, from, to time.Time, limit int) ([]uuid.UUID, error)
}

type postgresReminders struct{}

func (postgresReminders) remindersOf(ctx context.Context, event uuid.UUID, people []uuid.UUID) (map[uuid.UUID][]int, error) {
	return models.RemindersOf(ctx, models.Pool(), event, people)
}
func (postgresReminders) remindersFor(ctx context.Context, person uuid.UUID, events []uuid.UUID) (map[uuid.UUID][]int, error) {
	return models.RemindersForPerson(ctx, models.Pool(), person, events)
}
func (postgresReminders) setReminders(ctx context.Context, event, person uuid.UUID, minutes []int, at time.Time) error {
	return models.SetReminders(ctx, models.Pool(), event, person, minutes, at)
}
func (postgresReminders) planReminder(ctx context.Context, person uuid.UUID, p models.ReminderPayload, at time.Time) error {
	return models.PlanReminderJob(ctx, models.Pool(), person, p, at)
}
func (postgresReminders) cancelReminders(ctx context.Context, event uuid.UUID, people []uuid.UUID, kind string) error {
	return models.CancelReminderJobs(ctx, models.Pool(), event, people, kind)
}
func (postgresReminders) cancelRemindersExcept(ctx context.Context, event uuid.UUID, keep []uuid.UUID) error {
	return models.CancelReminderJobsExcept(ctx, models.Pool(), event, keep)
}
func (postgresReminders) unplanned(ctx context.Context, from, to time.Time, limit int) ([]uuid.UUID, error) {
	return models.UnplannedEvents(ctx, models.Pool(), from, to, limit)
}

// checkReminders is a person's reminders as written: at most five, each in
// reach (an all-day event's may fall on its day), sorted, once each.
func checkReminders(minutes []int, allDay bool) ([]int, error) {
	if minutes == nil {
		return nil, nil
	}
	out := make([]int, 0, len(minutes))
	for _, m := range minutes {
		least := 0
		if allDay {
			least = minReminderMinutes
		}
		if m < least || m > maxReminderMinutes {
			return nil, inputError("A reminder can come up to four weeks before an event.")
		}
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	if len(out) > maxReminders {
		return nil, inputError("An event can have up to 5 reminders.")
	}
	slices.Sort(out)
	return out, nil
}

// remindersIn is what reminds someone of one occurrence: their own for the
// event (set says they chose, own may be none), else the default when they
// organise it or said yes or maybe. Never an occurrence they declined. Pure.
func remindersIn(own []int, set, allDay, organiser bool, response string) []int {
	if !organiser && response == responseDeclined {
		return nil
	}
	if set {
		return own
	}
	if organiser || response == responseAccepted || response == responseTentative {
		if allDay {
			return defaultAllDayReminders
		}
		return defaultTimedReminders
	}
	return nil
}

// reminderAt is when a reminder minutes before an occurrence fires: before
// its start, or before 00:00 of an all-day one's first day in zone. Pure.
func reminderAt(e *models.Event, minutes int, zone *time.Location) time.Time {
	if e.AllDay() {
		return midnightOf(e.StartDate, zone).Add(-time.Duration(minutes) * time.Minute)
	}
	return e.StartAt.Add(-time.Duration(minutes) * time.Minute)
}

// due is one reminder to fire: which occurrence, which reminder, when.
type due struct {
	inst    instance
	minutes int
	at      time.Time
}

// firstReminder is the earliest reminder after after, over insts (earliest
// first), each occurrence's reminders from remindersOf. Pure.
func firstReminder(insts []instance, after time.Time, zone *time.Location, remindersOf func(instance) []int) (due, bool) {
	var best due
	found := false
	for _, inst := range insts {
		start, _ := spanOf(inst.Event)
		// Every later occurrence starts later still: none of its reminders
		// can come before best once its start, less the furthest reminder,
		// is past it.
		if found && start.Add(-maxReminderMinutes*time.Minute).After(best.at) {
			break
		}
		for _, m := range remindersOf(inst) {
			at := reminderAt(inst.Event, m, zone)
			if at.After(after) && (!found || at.Before(best.at)) {
				best, found = due{inst: inst, minutes: m, at: at}, true
			}
		}
	}
	return best, found
}

// peopleOn is who an event reminds: its organiser and its guests in the
// workspace.
func peopleOn(e *models.Event, guests []models.Attendee) []uuid.UUID {
	out := []uuid.UUID{e.CreatedBy}
	for _, g := range guests {
		if g.UserID != nil && *g.UserID != e.CreatedBy && !slices.Contains(out, *g.UserID) {
			out = append(out, *g.UserID)
		}
	}
	return out
}

// planReminders plans the next reminder of each person on an event or a
// series (of only these people, when named): moved when something changed,
// cancelled for anyone it no longer reminds. An instance's override plans
// its series.
func (s *service) planReminders(ctx context.Context, id uuid.UUID, only []uuid.UUID) error {
	return s.planAfter(ctx, id, only, s.now())
}

// planAfter is planReminders for reminders after after: after one has
// fired, its own time, so a clock a moment behind the queue's never plans
// the same reminder again.
func (s *service) planAfter(ctx context.Context, id uuid.UUID, only []uuid.UUID, after time.Time) error {
	if !helpers.CalendarInPostgres() {
		return nil
	}
	e, err := s.store.get(ctx, id)
	if err != nil {
		return err
	}
	if e != nil && e.SeriesID != nil {
		return s.planAfter(ctx, *e.SeriesID, only, after)
	}
	if e == nil || e.DeletedAt != nil || e.Status == statusCancelled {
		return s.rem.cancelReminders(ctx, id, only, "")
	}
	guests, err := s.store.attendees(ctx, []uuid.UUID{id})
	if err != nil {
		return err
	}
	on := peopleOn(e, guests[id])
	people := on
	if only != nil {
		people = nil
		var gone []uuid.UUID
		for _, p := range only {
			if slices.Contains(on, p) {
				people = append(people, p)
			} else {
				gone = append(gone, p)
			}
		}
		if len(gone) > 0 {
			if err := s.rem.cancelReminders(ctx, id, gone, ""); err != nil {
				return err
			}
		}
	} else if err := s.rem.cancelRemindersExcept(ctx, id, on); err != nil {
		return err
	}
	if len(people) == 0 {
		return nil
	}
	own, err := s.rem.remindersOf(ctx, id, people)
	if err != nil {
		return err
	}
	zones, err := s.zonesOf(ctx, people)
	if err != nil {
		zones = map[uuid.UUID]string{}
	}
	now := after
	w, err := window(now, now.Add(reminderHorizon))
	if err != nil {
		return err
	}
	insts, err := s.instancesOf(ctx, []*models.Event{e}, w, reminderPlanLimit)
	if err != nil {
		return err
	}
	answers := s.withAnswers(ctx, insts, map[uuid.UUID][]models.Attendee{id: guests[id]})
	for _, p := range people {
		zone, zerr := loadZone(zoneOrUTC(zones[p]))
		if zerr != nil {
			zone = time.UTC
		}
		mine, set := own[p]
		person := p
		next, ok := firstReminder(insts, now, zone, func(inst instance) []int {
			response := ""
			if a := answerOf(answers[inst.ref], models.GuestKey{UserID: &person}); a != nil {
				response = a.Response
			}
			return remindersIn(mine, set, inst.Event.AllDay(), person == e.CreatedBy, response)
		})
		switch {
		case ok:
			err = s.rem.planReminder(ctx, p, reminderPayload(id, next), next.at)
		case e.RRule != "" && (e.UntilAt == nil || e.UntilAt.After(w.To)):
			// Nothing in reach of a series that goes on: planned again at the edge.
			wake := w.To.Add(-24 * time.Hour)
			err = s.rem.planReminder(ctx, p, models.ReminderPayload{EventID: id.String(), Kind: models.ReminderNext, Replan: true, FireAt: wake.Format(time.RFC3339)}, wake)
		default:
			err = s.rem.cancelReminders(ctx, id, []uuid.UUID{p}, models.ReminderNext)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// reminderPayload is the job of a reminder due to fire.
func reminderPayload(event uuid.UUID, d due) models.ReminderPayload {
	start, _ := spanOf(d.inst.Event)
	p := models.ReminderPayload{EventID: event.String(), Kind: models.ReminderNext, Minutes: d.minutes,
		Start: start.Format(time.RFC3339), FireAt: d.at.Format(time.RFC3339Nano)}
	if d.inst.rid != nil {
		p.RecurrenceID = d.inst.rid.Format(time.RFC3339Nano)
	}
	return p
}

// occurrence is the instance a reminder names, as it is now: nil when it was
// moved away, cancelled or deleted.
func (s *service) occurrence(ctx context.Context, e *models.Event, recurrenceID string) (*instance, error) {
	if recurrenceID == "" {
		if e.RRule != "" {
			return nil, nil
		}
		inst := single(e)
		return &inst, nil
	}
	rid, err := time.Parse(time.RFC3339Nano, recurrenceID)
	if err != nil || e.RRule == "" {
		return nil, nil
	}
	o, err := s.store.overrideAt(ctx, e.ID, rid)
	if err != nil {
		return nil, err
	}
	if o != nil {
		if !live(o) {
			return nil, nil
		}
		inst := instance{Event: o, ref: instanceRef(e.ID, rid, e.AllDay()), series: e, rid: o.RecurrenceID, override: true}
		return &inst, nil
	}
	if !isInstance(e, rid) {
		return nil, nil
	}
	inst := madeInstance(e, rid)
	return &inst, nil
}

// fireReminder sends one reminder a job names, when it still stands, then
// plans the person's next. A wake-up only plans.
func (s *service) fireReminder(ctx context.Context, person uuid.UUID, p models.ReminderPayload) error {
	if !helpers.CalendarInPostgres() {
		return nil
	}
	id, err := uuid.Parse(p.EventID)
	if err != nil {
		return nil
	}
	if p.Replan {
		return s.planReminders(ctx, id, []uuid.UUID{person})
	}
	e, err := s.store.get(ctx, id)
	if err != nil {
		return err
	}
	if e == nil || e.DeletedAt != nil || e.Status == statusCancelled {
		return nil
	}
	guests, err := s.store.attendees(ctx, []uuid.UUID{id})
	if err != nil {
		return err
	}
	if !slices.Contains(peopleOn(e, guests[id]), person) {
		return nil
	}
	inst, err := s.occurrence(ctx, e, p.RecurrenceID)
	if err != nil {
		return err
	}
	if inst != nil {
		if err := s.sendReminder(ctx, person, e, guests[id], *inst, p); err != nil {
			return err
		}
	}
	if p.Kind == models.ReminderSnooze {
		return nil
	}
	after := s.now()
	if fired, err := time.Parse(time.RFC3339Nano, p.FireAt); err == nil && fired.After(after) {
		after = fired
	}
	return s.planAfter(ctx, id, []uuid.UUID{person}, after)
}

// sendReminder tells person of inst, when the reminder still stands as it
// was planned: they haven't declined it, it hasn't moved, and it isn't late.
func (s *service) sendReminder(ctx context.Context, person uuid.UUID, e *models.Event, guests []models.Attendee, inst instance, p models.ReminderPayload) error {
	answers := s.withAnswers(ctx, []instance{inst}, map[uuid.UUID][]models.Attendee{e.ID: guests})
	response := ""
	if a := answerOf(answers[inst.ref], models.GuestKey{UserID: &person}); a != nil {
		response = a.Response
	}
	organiser := person == e.CreatedBy
	if !organiser && response == responseDeclined {
		return nil
	}
	zone, err := loadZone(zoneOrUTC(s.zoneOf(ctx, person)))
	if err != nil {
		zone = time.UTC
	}
	now := s.now()
	at := reminderAt(inst.Event, p.Minutes, zone)
	if p.Kind == models.ReminderSnooze {
		if planned, err := time.Parse(time.RFC3339Nano, p.FireAt); err == nil {
			at = planned
		}
	}
	// Moved since it was planned: the plan after this one has its new time.
	if planned, err := time.Parse(time.RFC3339Nano, p.FireAt); err == nil && absDuration(planned.Sub(at)) > time.Minute {
		return nil
	}
	_, end := spanOf(inst.Event)
	if now.Sub(at) > reminderLate || !now.Before(end) {
		helpers.LogInfoWithContext(ctx, "business/Calendar: a reminder for %s came %s late and wasn't sent", inst.ref, now.Sub(at).Round(time.Minute))
		return nil
	}
	title, body := reminderWords(inst.Event, p.Minutes, p.Kind == models.ReminderSnooze, now, zone)
	data := whenData(inst.Event)
	data["minutes"] = strconv.Itoa(p.Minutes)
	if inst.ref != e.ID.String() {
		data["instance"] = inst.ref
	}
	// D-CAL-4: through their holds for what they organise or said yes to,
	// and the notice says so, for "why did this reach me in focus time?".
	through := organiser || response == responseAccepted
	if through {
		data["through_holds"] = "true"
	}
	recipient := notificationBusiness.NoticeRecipient{UserUUID: person.String(), Reason: "reminder", Listed: true, ThroughHolds: through}
	tellNotice(ctx, notificationBusiness.Notice{
		Kind: noticeReminder, ResourceKind: KindEvent, ResourceUUID: e.ID.String(), ResourceTitle: inst.Event.Title,
		Data: data, DedupKey: noticeReminder + "|" + inst.ref + "|" + strconv.Itoa(p.Minutes) + "|" + p.Kind + "|" + at.UTC().Format(time.RFC3339),
		Title: title, Body: body, Path: eventPath(inst.ref),
		Push: &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR, ID: inst.ref, Tag: "reminder:" + inst.ref},
	}, []notificationBusiness.NoticeRecipient{recipient})
	return nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// reminderWords is a reminder's title and line, on a clock in loc: "Launch
// review in 10 minutes", "Launch review is starting", "Offsite is
// tomorrow", with when it is under it. Pure.
func reminderWords(e *models.Event, minutes int, snoozed bool, now time.Time, loc *time.Location) (string, string) {
	when := whenWords(e, loc)
	if e.AllDay() {
		first, _ := parseDate(e.StartDate)
		today := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, time.UTC)
		switch days := int(first.Sub(today).Hours() / 24); {
		case days <= 0:
			return e.Title + " is today", when
		case days == 1:
			return e.Title + " is tomorrow", when
		default:
			return fmt.Sprintf("%s is in %d days", e.Title, days), when
		}
	}
	left := e.StartAt.Sub(now).Round(time.Minute)
	switch {
	case left <= 0:
		return e.Title + " is starting", when
	case left < time.Hour:
		return fmt.Sprintf("%s in %s", e.Title, plural(int(left.Minutes()), "minute")), when
	case left < 24*time.Hour:
		return fmt.Sprintf("%s in %s", e.Title, plural(int(left.Round(time.Hour).Hours()), "hour")), when
	default:
		return fmt.Sprintf("%s in %s", e.Title, plural(int(left.Hours()/24), "day")), when
	}
}

// plural is a count with its noun: "1 minute", "10 minutes". Pure.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// SnoozeFor is how long a snoozed reminder waits: five minutes, until it
// starts, or until a minute before.
type SnoozeFor string

const (
	SnoozeFiveMinutes SnoozeFor = "5m"
	SnoozeUntilStart  SnoozeFor = "start"
	SnoozeMinuteAhead SnoozeFor = "1m"
)

// Reminders is who's own reminders for an event or series (an instance's
// are its series'): minutes before, and whether they are the default.
func (s *service) Reminders(ctx context.Context, who *model.UserInfo, id string) (Reminders, error) {
	if err := s.ready(ctx); err != nil {
		return Reminders{}, err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return Reminders{}, err
	}
	return s.remindersView(ctx, inst.owner(), guests, personOf(who))
}

// remindersView is person's reminders for an event as the web shows them.
func (s *service) remindersView(ctx context.Context, e *models.Event, guests []models.Attendee, person uuid.UUID) (Reminders, error) {
	own, err := s.rem.remindersOf(ctx, e.ID, []uuid.UUID{person})
	if err != nil {
		return Reminders{}, err
	}
	if mine, set := own[person]; set {
		return Reminders{Minutes: mine}, nil
	}
	response := ""
	if a := answerOf(guests, models.GuestKey{UserID: &person}); a != nil {
		response = a.Response
	}
	return Reminders{Minutes: remindersIn(nil, false, e.AllDay(), person == e.CreatedBy, response), Default: true}, nil
}

// SetReminders sets who's own reminders for an event or series (nil goes
// back to the default; empty is none), and plans their next. Saved without
// a word to anyone: they are theirs alone.
func (s *service) SetReminders(ctx context.Context, who *model.UserInfo, id string, minutes []int) (Reminders, error) {
	if err := s.ready(ctx); err != nil {
		return Reminders{}, err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return Reminders{}, err
	}
	owner := inst.owner()
	minutes, err = checkReminders(minutes, owner.AllDay())
	if err != nil {
		return Reminders{}, err
	}
	me := personOf(who)
	if err := s.rem.setReminders(ctx, owner.ID, me, minutes, s.now()); err != nil {
		return Reminders{}, err
	}
	if err := s.planReminders(ctx, owner.ID, []uuid.UUID{me}); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: %s's reminders for %s were saved and not planned: %v", me, owner.ID, err)
	}
	return s.remindersView(ctx, owner, guests, me)
}

// Snooze brings a reminder of an occurrence back to who: in five minutes,
// when it starts, or a minute before.
func (s *service) Snooze(ctx context.Context, who *model.UserInfo, id string, until SnoozeFor) (time.Time, error) {
	if err := s.ready(ctx); err != nil {
		return time.Time{}, err
	}
	inst, _, err := s.resolve(ctx, who, id)
	if err != nil {
		return time.Time{}, err
	}
	start, end := spanOf(inst.Event)
	now := s.now()
	var at time.Time
	switch until {
	case SnoozeFiveMinutes:
		at = now.Add(5 * time.Minute)
	case SnoozeUntilStart:
		at = start
	case SnoozeMinuteAhead:
		at = start.Add(-time.Minute)
	default:
		return time.Time{}, inputError("Snooze for 5 minutes, until it starts, or until a minute before.")
	}
	if !at.After(now) || !end.After(at) {
		return time.Time{}, inputError("That's already passed. Snooze for 5 minutes instead.")
	}
	owner := inst.owner()
	p := models.ReminderPayload{EventID: owner.ID.String(), Kind: models.ReminderSnooze, Start: start.Format(time.RFC3339),
		Minutes: int(start.Sub(at).Minutes()), FireAt: at.Format(time.RFC3339Nano)}
	if inst.rid != nil {
		p.RecurrenceID = inst.rid.Format(time.RFC3339Nano)
	}
	if err := s.rem.planReminder(ctx, personOf(who), p, at); err != nil {
		return time.Time{}, err
	}
	return at, nil
}

// withReminders puts reader's reminders on each event of a list (one read,
// whatever its length): their own, or the default for what they organise or
// said yes or maybe to.
func (s *service) withReminders(ctx context.Context, rows []instance, occ map[string][]models.Attendee, out []*dgraphStruct.DgraphEvent, reader uuid.UUID) {
	var owners []uuid.UUID
	for _, inst := range rows {
		if id := inst.owner().ID; !slices.Contains(owners, id) {
			owners = append(owners, id)
		}
	}
	own, err := s.rem.remindersFor(ctx, reader, owners)
	if err != nil {
		// The list is shown without them, rather than not at all.
		return
	}
	for i, inst := range rows {
		owner := inst.owner()
		mine, set := own[owner.ID]
		response := ""
		if a := answerOf(occ[inst.ref], models.GuestKey{UserID: &reader}); a != nil {
			response = a.Response
		}
		minutes := slices.Clone(remindersIn(mine, set, owner.AllDay(), reader == owner.CreatedBy, response))
		if minutes == nil {
			minutes = []int{}
		}
		out[i].Reminders, out[i].RemindersDefault = &minutes, !set
	}
}

// Reminders is a person's reminders for an event.
type Reminders struct {
	// Minutes are minutes before it (an all-day event's, before 00:00 of its
	// first day): empty for none.
	Minutes []int `json:"minutes"`
	// Default says they set none, and these are the default's.
	Default bool `json:"default"`
}

// plansReminders is whether a Resource Event can change who an event
// reminds, or when.
func plansReminders(e resourceEventBusiness.Event) bool {
	if e.ResourceKind != KindEvent {
		return false
	}
	switch e.Kind {
	case resourceEventBusiness.CalendarEventCreated, resourceEventBusiness.CalendarEventUpdated, resourceEventBusiness.CalendarEventCancelled,
		resourceEventBusiness.CalendarEventDeleted, resourceEventBusiness.CalendarEventRestored, resourceEventBusiness.CalendarEventRSVP:
		return true
	}
	return false
}

func init() {
	// A change to an event, its guests or an answer plans its reminders.
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "calendar.reminders", Wants: plansReminders,
		Run: func(ctx context.Context, e resourceEventBusiness.Event) {
			id, err := uuid.Parse(e.ResourceUUID)
			if err != nil {
				return
			}
			if err := stored.planReminders(ctx, id, nil); err != nil {
				helpers.LogErrorWithContext(ctx, "business/Calendar: reminders for %s couldn't be planned: %v", id, err)
			}
		}})
	// A reminder due fires, and names the next.
	schedulerBusiness.RegisterJobHandler(models.ReminderJobType, func(ctx context.Context, job *jobModel.ScheduledJob) error {
		var p models.ReminderPayload
		if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a reminder job %s can't be read: %v", job.Id, err)
			return nil
		}
		return stored.fireReminder(ctx, job.UserUuid, p)
	})
	// What a write made with nobody told (an import, the move, the demo's
	// seeding) is planned within the hour.
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "calendar.reminders.plan", Every: time.Hour, Run: func(ctx context.Context) error {
		return stored.planUnplanned(ctx)
	}})
}

// planUnplanned plans the reminders of events coming up that nobody has a
// reminder waiting for. Not on the demo: its seeded events stay quiet (the
// visitor is shared, and is reminded of what they make themselves).
func (s *service) planUnplanned(ctx context.Context) error {
	if !helpers.CalendarInPostgres() || !helpers.CalendarReadsFromStore() || helpers.DemoMode() {
		return nil
	}
	now := s.now()
	ids, err := s.rem.unplanned(ctx, now, now.Add(reminderHorizon), 500)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.planReminders(ctx, id, nil); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: reminders for %s couldn't be planned: %v", id, err)
		}
	}
	return nil
}
