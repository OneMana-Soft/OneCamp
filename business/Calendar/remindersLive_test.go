package Calendar

// Head A (gcal-5): a reminder by email, defaults per calendar, and the grid
// told live of a change.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// emailing swaps the email seams: sent says how many an email reaches (0:
// none), full whether the day's email limit is reached.
func emailing(t *testing.T, sent int, full bool) *[]notificationBusiness.Event {
	t.Helper()
	var got []notificationBusiness.Event
	prevSend, prevFull := emailReminder, emailLimitReached
	emailReminder = func(_ context.Context, evt notificationBusiness.Event) int {
		got = append(got, evt)
		return sent
	}
	emailLimitReached = func(context.Context) bool { return full }
	t.Cleanup(func() { emailReminder, emailLimitReached = prevSend, prevFull })
	return &got
}

func TestAnEmailReminderIsEmailedOrSaysWhyItCameAsAPopUp(t *testing.T) {
	h, rem := reminding(t)
	ctx := context.Background()
	sam := h.member("Sam")
	e := h.create(sam, "Design review")
	if _, err := h.s.SetReminders(ctx, sam, e.ID, []int{10}, []int{20}); err == nil {
		t.Error("an email reminder that isn't one of the reminders was kept")
	}
	got, err := h.s.SetReminders(ctx, sam, e.ID, []int{60, 10}, []int{60})
	if err != nil || !slices.Equal(got.Minutes, []int{10, 60}) || !slices.Equal(got.Email, []int{60}) || got.Default {
		t.Fatalf("set: %+v %v", got, err)
	}
	if again, _ := h.s.Reminders(ctx, sam, e.ID); !slices.Equal(again.Email, []int{60}) {
		t.Errorf("read back: %+v", again)
	}
	told := captureNotices(t)
	emailed := emailing(t, 1, false)
	j, ok := rem.next(sam, e.ID, models.ReminderNext)
	if !ok || j.p.Minutes != 60 || !j.p.Email {
		t.Fatalf("the hour's reminder is planned by email: %+v %v", j, ok)
	}
	h.clock = j.at.Add(30 * time.Second)
	if err := h.s.fireReminder(ctx, j.person, j.p); err != nil {
		t.Fatal(err)
	}
	if len(*emailed) != 1 || len(*told) != 0 {
		t.Fatalf("emailed %d, told %d", len(*emailed), len(*told))
	}
	m := (*emailed)[0]
	if m.Type != notificationBusiness.EventCalendarReminder || !m.Requested || !m.SkipOnlineCheck || len(m.Recipients) != 1 ||
		m.Recipients[0].UserUUID != personOf(sam) || m.Title == "" || !strings.HasSuffix(m.CTAURL, "/app/calendar/event/"+e.ID) {
		t.Errorf("the email: %+v", m)
	}
	// It has no unsubscribe link (an unsubscribe never stops it), so it says where to.
	if !strings.Contains(m.Why, "change or remove the reminder on the event") {
		t.Errorf("the email doesn't say where to stop it: %q", m.Why)
	}
	// The ten minutes' is a pop-up, as before.
	j, _ = rem.next(sam, e.ID, models.ReminderNext)
	if j.p.Minutes != 10 || j.p.Email {
		t.Fatalf("next: %+v", j)
	}
	h.clock = j.at.Add(30 * time.Second)
	if err := h.s.fireReminder(ctx, j.person, j.p); err != nil {
		t.Fatal(err)
	}
	if len(*emailed) != 1 || len(*told) != 1 || strings.Contains((*told)[0].n.Body, "sent as a pop-up") {
		t.Errorf("the pop-up: emailed %d, told %+v", len(*emailed), *told)
	}
	// The day's email limit reached, or the email not sent: a pop-up that says so.
	for _, c := range []struct {
		sent int
		full bool
		says string
	}{{1, true, "sent as a pop-up: email limit reached"}, {0, false, "sent as a pop-up: email couldn't be sent"}} {
		*told = (*told)[:0]
		emailed = emailing(t, c.sent, c.full)
		h.clock = time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
		if _, err := h.s.SetReminders(ctx, sam, e.ID, []int{60}, []int{60}); err != nil {
			t.Fatal(err)
		}
		j, _ = rem.next(sam, e.ID, models.ReminderNext)
		h.clock = j.at.Add(30 * time.Second)
		if err := h.s.fireReminder(ctx, j.person, j.p); err != nil {
			t.Fatal(err)
		}
		if len(*told) != 1 || !strings.Contains((*told)[0].n.Body, c.says) || (*told)[0].n.Data["email_fallback"] == "" {
			t.Errorf("%q: told %+v", c.says, *told)
		}
		if c.full && len(*emailed) != 0 {
			t.Errorf("an email was tried past the day's limit")
		}
	}
}

func TestADefaultPerCalendarRemindsWhenNoneIsSet(t *testing.T) {
	h, rem := reminding(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	project := uuid.NewString()
	projectsDecide(t, map[string]map[string]accessBusiness.Level{personOf(sam).String(): {project: accessBusiness.LevelEdit}}, nil)
	cal, err := h.s.ProjectCalendarFor(ctx, sam, project)
	if err != nil {
		t.Fatal(err)
	}
	rem.defaults[personOf(sam)] = map[string]ReminderDefaults{
		DefaultsMine:     {Timed: &ReminderSet{Minutes: []int{30}, Email: []int{30}}},
		cal.String():     {Timed: &ReminderSet{Minutes: []int{5}}},
		uuid.NewString(): {Timed: &ReminderSet{Minutes: []int{90}}},
	}
	mine := h.create(sam, "Design review", maya)
	onProject := h.onCalendar(cal, sam, "Sprint planning", time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC), "")
	for _, id := range []string{mine.ID, onProject.ID.String()} {
		if err := h.s.planReminders(ctx, uuid.MustParse(id), nil); err != nil {
			t.Fatal(err)
		}
	}
	if j, ok := rem.next(sam, mine.ID, models.ReminderNext); !ok || j.p.Minutes != 30 || !j.p.Email {
		t.Errorf("my calendar's default, by email: %+v %v", j, ok)
	}
	if j, ok := rem.next(sam, onProject.ID.String(), models.ReminderNext); !ok || j.p.Minutes != 5 || j.p.Email {
		t.Errorf("the project calendar's own default: %+v %v", j, ok)
	}
	if v, _ := h.s.Reminders(ctx, sam, mine.ID); !v.Default || !slices.Equal(v.Minutes, []int{30}) || !slices.Equal(v.Email, []int{30}) {
		t.Errorf("the view says the default: %+v", v)
	}
	// Maya set no default: Google's ten minutes once she says yes.
	if _, err := h.s.Answer(ctx, maya, mine.ID, AnswerInput{Answer: Accepted}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.planReminders(ctx, uuid.MustParse(mine.ID), nil); err != nil {
		t.Fatal(err)
	}
	if j, ok := rem.next(maya, mine.ID, models.ReminderNext); !ok || j.p.Minutes != 10 || j.p.Email {
		t.Errorf("Maya's: %+v %v", j, ok)
	}
	// An all-day event takes the all-day default; none set there, D-CAL-4's.
	day := blank("UTC")
	day.CalendarID = uuid.New()
	day.StartDate, day.EndDate = "2026-10-20", "2026-10-21"
	if m, em := defaultsFor(rem.defaults[personOf(sam)], day); !slices.Equal(m, defaultAllDayReminders) || em != nil {
		t.Errorf("all-day: %v %v", m, em)
	}
	// Changing a default plans that person's coming events again.
	if err := h.s.defaultsChanged(ctx, personOf(sam)); err != nil {
		t.Fatal(err)
	}
	if len(rem.replanned) != 1 || rem.replanned[0] != personOf(sam) {
		t.Errorf("replanned %v", rem.replanned)
	}
}

// V-524: a reminder email has no unsubscribe link, so its footer says where
// to stop it: on the event for one set there; in the calendar's Default
// reminders for one that came from a default.
func TestAReminderEmailSaysWhereToStopIt(t *testing.T) {
	h, rem := reminding(t)
	ctx := context.Background()
	sam := h.member("Sam")
	own, byDefault := h.create(sam, "Design review"), h.create(sam, "Standup")
	if _, err := h.s.SetReminders(ctx, sam, own.ID, []int{60}, []int{60}); err != nil {
		t.Fatal(err)
	}
	rem.defaults[personOf(sam)] = map[string]ReminderDefaults{
		DefaultsMine: {Timed: &ReminderSet{Minutes: []int{30}, Email: []int{30}}},
	}
	if err := h.s.planReminders(ctx, uuid.MustParse(byDefault.ID), nil); err != nil {
		t.Fatal(err)
	}
	why := func(event string, minutes int) string {
		t.Helper()
		emailed := emailing(t, 1, false)
		j, ok := rem.next(sam, event, models.ReminderNext)
		if !ok || j.p.Minutes != minutes || !j.p.Email {
			t.Fatalf("%s: planned %+v %v, want %d minutes by email", event, j, ok, minutes)
		}
		h.clock = j.at.Add(30 * time.Second)
		if err := h.s.fireReminder(ctx, j.person, j.p); err != nil {
			t.Fatal(err)
		}
		if len(*emailed) != 1 {
			t.Fatalf("%s: emailed %d", event, len(*emailed))
		}
		return (*emailed)[0].Why
	}
	if w := why(own.ID, 60); !strings.Contains(w, "change or remove the reminder on the event") || strings.Contains(w, "Default reminders") {
		t.Errorf("a reminder set on the event: %q", w)
	}
	if w := why(byDefault.ID, 30); !strings.Contains(w, "default reminders") || !strings.Contains(w, "change Default reminders") ||
		strings.Contains(w, "on the event") {
		t.Errorf("a reminder from the calendar's default: %q, want where to change the default", w)
	}
}

func TestDefaultRemindersAreCheckedAsWritten(t *testing.T) {
	for _, bad := range []string{
		`[]`, `{"mine": {"timed": {"minutes": [99999]}}}`, `{"mine": {"timed": {"minutes": [10], "email": [20]}}}`,
		`{"someone": {"timed": {"minutes": [10]}}}`, `{"mine": {"timed": {"minutes": [1,2,3,4,5,6]}}}`,
	} {
		if defaultRemindersProblem(json.RawMessage(bad)) == nil {
			t.Errorf("kept %s", bad)
		}
	}
	good := `{"mine": {"timed": {"minutes": [10, 60], "email": [60]}, "all_day": {"minutes": [-540]}}, "` + uuid.NewString() + `": {"timed": {"minutes": []}}}`
	if err := defaultRemindersProblem(json.RawMessage(good)); err != nil {
		t.Errorf("refused %s: %v", good, err)
	}
}

func TestALiveChangeTellsOnlyWhoMayReadItAndOnlyWhenItIs(t *testing.T) {
	h, _ := reminding(t)
	ctx := context.Background()
	sam, maya, lee, jonas := h.member("Sam"), h.member("Maya"), h.member("Lee"), h.member("Jonas")
	e := h.create(sam, "Salary review", maya)
	ev, _ := h.store.get(ctx, uuid.MustParse(e.ID))
	h.store.shares = append(h.store.shares, models.CalendarShare{CalendarID: ev.CalendarID, PersonID: personOf(lee), Level: models.ShareDetails})
	topics, change, err := h.s.liveOf(ctx, uuid.MustParse(e.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{personOf(sam).String(), personOf(maya).String(), personOf(lee).String()} {
		if !slices.Contains(topics, helpers.GetMqttTopicForUserActivity(who)) {
			t.Errorf("%s isn't told: %v", who, topics)
		}
	}
	// Jonas may see only that Sam is busy: he isn't told, and nobody is told more than a busy view shows.
	if slices.Contains(topics, helpers.GetMqttTopicForUserActivity(personOf(jonas).String())) {
		t.Error("a member who sees Sam as busy alone was told")
	}
	raw, _ := json.Marshal(change)
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	if len(keys) != 3 || keys["event_id"] != e.ID || keys["from"] != "2026-10-16T09:15:00Z" || keys["to"] != "2026-10-16T09:30:00Z" {
		t.Errorf("the message says more than a busy view: %s", raw)
	}
	// A project calendar's event is told on the project's topic too.
	project := uuid.NewString()
	projectsDecide(t, map[string]map[string]accessBusiness.Level{personOf(sam).String(): {project: accessBusiness.LevelEdit}}, nil)
	cal, err := h.s.ProjectCalendarFor(ctx, sam, project)
	if err != nil {
		t.Fatal(err)
	}
	onProject := h.onCalendar(cal, sam, "Sprint planning", time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC), "FREQ=WEEKLY")
	topics, change, err = h.s.liveOf(ctx, onProject.ID)
	if err != nil || !slices.Contains(topics, helpers.GetMqttTopicForProjectMessage(project)) || change.To != "" {
		t.Errorf("project: %v %+v %v", topics, change, err)
	}
	// Who is told, by the sink, on one publish.
	var published [][]string
	prev := publishLive
	publishLive = func(topics []string, _ int8, _ interface{}) { published = append(published, topics) }
	t.Cleanup(func() { publishLive = prev })
	h.s.tellLive(ctx, uuid.MustParse(e.ID))
	if len(published) != 1 || len(published[0]) < 3 {
		t.Errorf("published %v", published)
	}
}

// The list shows each reader's reminders as they will come: their own, else
// their default for the event's calendar, and which of them come by email,
// so the card and the form say what fires (withReminders).
func TestTheListShowsEachReadersDefaultAndEmail(t *testing.T) {
	h, rem := reminding(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	rem.defaults[personOf(sam)] = map[string]ReminderDefaults{DefaultsMine: {Timed: &ReminderSet{Minutes: []int{30}, Email: []int{30}}}}
	e := h.create(sam, "Design review", maya)
	if _, err := h.s.SetReminders(ctx, maya, e.ID, []int{15, 60}, []int{60}); err != nil {
		t.Fatal(err)
	}
	from, to := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	for who, want := range map[*model.UserInfo]struct {
		minutes, email []int
		def            bool
	}{sam: {[]int{30}, []int{30}, true}, maya: {[]int{15, 60}, []int{60}, false}} {
		list, err := h.s.legacyRange(ctx, who, from, to)
		if err != nil || len(list) != 1 {
			t.Fatalf("%s: %+v %v", who.UserDgraphInfo.UserName, list, err)
		}
		got, shown := list[0], func(p *[]int) any {
			if p == nil {
				return nil
			}
			return *p
		}
		if got.Reminders == nil || !slices.Equal(*got.Reminders, want.minutes) || got.RemindersDefault != want.def ||
			got.RemindersEmail == nil || !slices.Equal(*got.RemindersEmail, want.email) {
			t.Errorf("%s: %v (default %v), email %v; want %v (default %v), email %v", who.UserDgraphInfo.UserName,
				shown(got.Reminders), got.RemindersDefault, shown(got.RemindersEmail), want.minutes, want.def, want.email)
		}
	}
}
