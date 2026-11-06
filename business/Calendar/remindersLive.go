package Calendar

// Head A (gcal-5), "Live and reminded":
//
//   - A reminder can come by email rather than as a pop-up, one by one, as
//     Google's do. The email is one the person asked for, so it goes out
//     whatever their email settings, but never past the day's email cap
//     (EMAIL_DAILY_CAP, the shared free tier of a workspace on OneCamp
//     Cloud): then, or when it can't be sent, the reminder
//     comes as a pop-up that says so ("sent as a pop-up: email limit
//     reached"), never silently.
//   - A person's defaults per calendar: for its timed events and its all-day
//     ones, with "mine" for their own calendar and what they're invited to.
//     They remind whenever the person set none for an event; with no
//     default, D-CAL-4's. A change plans their coming events again.
//   - A change to an event is told live to who may read it: its organiser,
//     its member guests, its calendar's owner and the people it's shared
//     with, and a project calendar's project. The message carries the
//     event's id and window alone, which a busy view already shows, so a
//     reader who sees it as busy learns nothing more.
//
// Permissions: reminders and defaults are each person's own (only they read
// or set them); the live message goes only to who may read the calendar.

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	preferencesBusiness "github.com/akashc777/OneCamp/business/Preferences"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

const (
	// PrefDefaultReminders is a person's default reminders per calendar:
	// {"mine" | calendar id: ReminderDefaults}.
	PrefDefaultReminders = "calendar.default_reminders"
	// DefaultsMine names the person's own calendar, and what they're invited
	// to on a calendar they have no defaults for.
	DefaultsMine = "mine"
	// maxDefaultCalendars is how many calendars one person keeps defaults for.
	maxDefaultCalendars = 50
)

// ReminderSet is a set of reminders, minutes before, and which of them come
// by email.
type ReminderSet struct {
	Minutes []int `json:"minutes"`
	Email   []int `json:"email,omitempty"`
}

// ReminderDefaults are a person's defaults for one calendar: for its timed
// events and its all-day ones. One left out keeps D-CAL-4's.
type ReminderDefaults struct {
	Timed  *ReminderSet `json:"timed,omitempty"`
	AllDay *ReminderSet `json:"all_day,omitempty"`
}

// Seams: how an email reminder is sent and whether the day's limit is
// reached, and how a change is told live.
var (
	emailReminder     = notificationBusiness.Dispatch
	emailLimitReached = notificationBusiness.EmailLimitReached
	publishLive       = mqttBusiness.PublishToTopics
)

// checkEmail is which of minutes come by email, as written: each one of
// minutes, once each, sorted. Pure.
func checkEmail(minutes, email []int) ([]int, error) {
	out := []int{}
	for _, m := range email {
		if !slices.Contains(minutes, m) {
			return nil, inputError("An email reminder has to be one of the event's reminders.")
		}
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	slices.Sort(out)
	return out, nil
}

// defaultsFor is what reminds a person of e when they set none, and which of
// those come by email: their default for e's calendar, else for "mine", for
// a timed or an all-day event; else D-CAL-4's (none for a working
// location, whatever they chose). Pure.
func defaultsFor(prefs map[string]ReminderDefaults, e *models.Event) ([]int, []int) {
	base := defaultsOf(e)
	if base == nil {
		return nil, nil
	}
	d, ok := prefs[e.CalendarID.String()]
	if !ok {
		d, ok = prefs[DefaultsMine]
	}
	set := d.Timed
	if e.AllDay() {
		set = d.AllDay
	}
	if !ok || set == nil {
		return base, nil
	}
	return set.Minutes, set.Email
}

// defaultRemindersProblem is why a person's defaults can't be kept, or nil:
// up to maxDefaultCalendars calendars, each "mine" or a calendar's id, each
// set as an event's reminders are checked. Pure.
func defaultRemindersProblem(v json.RawMessage) error {
	var all map[string]ReminderDefaults
	if json.Unmarshal(v, &all) != nil {
		return &preferencesBusiness.Refused{Msg: "Default reminders are kept per calendar."}
	}
	if len(all) > maxDefaultCalendars {
		return &preferencesBusiness.Refused{Msg: "Keep default reminders for up to 50 calendars."}
	}
	for cal, d := range all {
		if _, err := uuid.Parse(cal); err != nil && cal != DefaultsMine {
			return &preferencesBusiness.Refused{Msg: "Default reminders belong to a calendar."}
		}
		for _, c := range []struct {
			set    *ReminderSet
			allDay bool
		}{{d.Timed, false}, {d.AllDay, true}} {
			if c.set == nil {
				continue
			}
			minutes, err := checkReminders(c.set.Minutes, c.allDay)
			if err == nil {
				_, err = checkEmail(minutes, c.set.Email)
			}
			if err != nil {
				return &preferencesBusiness.Refused{Msg: err.Error()}
			}
		}
	}
	return nil
}

// defaultsChanged plans again, now, every event that may remind person in
// the planner's reach, so their new defaults reach what was planned under
// the old ones.
func (s *service) defaultsChanged(ctx context.Context, person uuid.UUID) error {
	if !helpers.CalendarInPostgres() {
		return nil
	}
	now := s.now()
	return s.rem.replanPerson(ctx, person, now, now.Add(reminderHorizon), reminderPlanLimit)
}

// fallbackWords are why an email reminder came as a pop-up, as it says.
var fallbackWords = map[string]string{
	"limit":  "sent as a pop-up: email limit reached",
	"unsent": "sent as a pop-up: email couldn't be sent",
}

// reminderEmailWhy ends a reminder email. It has no unsubscribe link (an
// unsubscribe never stops a reminder the person asked for), so it says where.
const reminderEmailWhy = "You asked for this reminder by email. To stop it, change or remove the reminder on the event."

// sendByEmail emails person a reminder, and is whether it went: "" when it
// did, else why not ("limit" or "unsent").
func sendByEmail(ctx context.Context, person uuid.UUID, title, body, path, dedup string) string {
	if emailLimitReached(ctx) {
		return "limit"
	}
	n := emailReminder(ctx, notificationBusiness.Event{
		Type: notificationBusiness.EventCalendarReminder, SubjectLine: title, Title: title, Body: body,
		CTAURL: notificationBusiness.AppURL(path), CTAText: "Open in OneCamp",
		DedupKeyParts: []string{dedup}, Recipients: []notificationBusiness.Recipient{{UserUUID: person}},
		Requested: true, SkipOnlineCheck: true, Why: reminderEmailWhy,
	})
	if n > 0 {
		return ""
	}
	return "unsent"
}

// LiveChange is what a live message says of a changed event: its id and
// window ("to" empty for a series with no end), as a busy view shows them.
type LiveChange struct {
	EventID string `json:"event_id"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// liveOf is who is told of a change to event id, as topics, and what: the
// activity topics of its organiser, member guests, its calendar's owner and
// the people it's shared with, and a project calendar's project topic.
func (s *service) liveOf(ctx context.Context, id uuid.UUID) ([]string, *LiveChange, error) {
	e, err := s.store.get(ctx, id)
	if err != nil || e == nil {
		return nil, nil, err
	}
	guests, err := s.store.attendees(ctx, []uuid.UUID{id})
	if err != nil {
		return nil, nil, err
	}
	people := peopleOn(e, guests[id])
	cals, err := s.store.calendarsByID(ctx, []uuid.UUID{e.CalendarID})
	if err != nil {
		return nil, nil, err
	}
	var topics []string
	for _, c := range cals {
		if c.ContainerKind == containerProject {
			topics = append(topics, helpers.GetMqttTopicForProjectMessage(c.ContainerID))
		}
		if c.OwnerID != nil && c.ContainerKind == "" && c.Kind != kindSubscribed {
			people = append(people, *c.OwnerID)
		}
	}
	if len(cals) == 1 && cals[0].OwnerID != nil && cals[0].ContainerKind == "" {
		shares, err := s.store.sharesOf(ctx, e.CalendarID)
		if err != nil {
			return nil, nil, err
		}
		for _, sh := range shares {
			people = append(people, sh.PersonID)
		}
	}
	for _, p := range people {
		if t := helpers.GetMqttTopicForUserActivity(p.String()); p != uuid.Nil && !slices.Contains(topics, t) {
			topics = append(topics, t)
		}
	}
	start, end := spanOf(e)
	change := &LiveChange{EventID: id.String(), From: start.UTC().Format(time.RFC3339), To: end.UTC().Format(time.RFC3339)}
	if e.RRule != "" {
		change.To = ""
		if e.UntilAt != nil {
			change.To = e.UntilAt.UTC().Format(time.RFC3339)
		}
	}
	return topics, change, nil
}

// tellLive tells who may read event id that it changed, on one publish.
func (s *service) tellLive(ctx context.Context, id uuid.UUID) {
	topics, change, err := s.liveOf(ctx, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: a change to %s couldn't be told live: %v", id, err)
		return
	}
	if len(topics) > 0 {
		publishLive(topics, mqttStruct.MESSAGE_CALENDAR_LIVE, change)
	}
}

func init() {
	// A change that can move an event, its guests or an answer is told live.
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "calendar.live", Wants: plansReminders,
		Run: func(ctx context.Context, e resourceEventBusiness.Event) {
			if id, err := uuid.Parse(e.ResourceUUID); err == nil && helpers.CalendarInPostgres() {
				stored.tellLive(ctx, id)
			}
		}})
	preferencesBusiness.Register(PrefDefaultReminders, preferencesBusiness.Key{Default: json.RawMessage("{}"), Check: defaultRemindersProblem})
	// A change of them plans the person's coming events again.
	preferencesBusiness.OnChange(PrefDefaultReminders, func(ctx context.Context, user uuid.UUID, _ json.RawMessage) {
		if err := stored.defaultsChanged(ctx, user); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: %s's default reminders were kept and their events not planned again: %v", user, err)
		}
	})
}

// dedupOf is one firing of a reminder, the same for its email and its pop-up.
func dedupOf(ref string, p models.ReminderPayload, at time.Time) string {
	return noticeReminder + "|" + ref + "|" + strconv.Itoa(p.Minutes) + "|" + p.Kind + "|" + at.UTC().Format(time.RFC3339)
}
