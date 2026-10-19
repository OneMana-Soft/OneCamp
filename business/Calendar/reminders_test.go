package Calendar

// Calendar A5 over the stores in memory: whose reminders an event gives and
// when each fires, across a series, its overrides and each person's answers;
// a reminder sent through or within someone's holds; one late or moved never
// sent; snoozes; and each person's own reminders kept and planned again.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// memReminders keeps reminders and their waiting jobs in memory.
type memReminders struct {
	mu   sync.Mutex
	own  map[[2]uuid.UUID][]int
	jobs map[string]memJob
}

type memJob struct {
	person uuid.UUID
	p      models.ReminderPayload
	at     time.Time
}

func newMemReminders() *memReminders {
	return &memReminders{own: map[[2]uuid.UUID][]int{}, jobs: map[string]memJob{}}
}

func jobKey(person uuid.UUID, event, kind string) string {
	return person.String() + "|" + event + "|" + kind
}

func (m *memReminders) remindersOf(_ context.Context, event uuid.UUID, people []uuid.UUID) (map[uuid.UUID][]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID][]int{}
	for _, p := range people {
		if set, ok := m.own[[2]uuid.UUID{event, p}]; ok {
			out[p] = set
		}
	}
	return out, nil
}

func (m *memReminders) remindersFor(_ context.Context, person uuid.UUID, events []uuid.UUID) (map[uuid.UUID][]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[uuid.UUID][]int{}
	for _, e := range events {
		if set, ok := m.own[[2]uuid.UUID{e, person}]; ok {
			out[e] = set
		}
	}
	return out, nil
}

func (m *memReminders) setReminders(_ context.Context, event, person uuid.UUID, minutes []int, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if minutes == nil {
		delete(m.own, [2]uuid.UUID{event, person})
	} else {
		m.own[[2]uuid.UUID{event, person}] = minutes
	}
	return nil
}

func (m *memReminders) planReminder(_ context.Context, person uuid.UUID, p models.ReminderPayload, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[jobKey(person, p.EventID, p.Kind)] = memJob{person, p, at}
	return nil
}

func (m *memReminders) cancelReminders(_ context.Context, event uuid.UUID, people []uuid.UUID, kind string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, j := range m.jobs {
		if j.p.EventID == event.String() && (people == nil || slices.Contains(people, j.person)) && (kind == "" || j.p.Kind == kind) {
			delete(m.jobs, k)
		}
	}
	return nil
}

func (m *memReminders) cancelRemindersExcept(_ context.Context, event uuid.UUID, keep []uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, j := range m.jobs {
		if j.p.EventID == event.String() && !slices.Contains(keep, j.person) {
			delete(m.jobs, k)
		}
	}
	return nil
}

func (m *memReminders) unplanned(context.Context, time.Time, time.Time, int) ([]uuid.UUID, error) {
	return nil, nil
}

// next is who's waiting job of a kind for an event, and whether there is one.
func (m *memReminders) next(who *model.UserInfo, event, kind string) (memJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[jobKey(personOf(who), event, kind)]
	return j, ok
}

// reminding is the harness with its reminders in memory and the one store on.
func reminding(t *testing.T) (*harness, *memReminders) {
	t.Helper()
	t.Setenv("CALENDAR_STORE", "postgres")
	h := newHarness(t)
	rem := newMemReminders()
	h.s.rem = rem
	return h, rem
}

func TestRemindersAreTheirOwnOrTheDefault(t *testing.T) {
	cases := []struct {
		name                   string
		own                    []int
		set, allDay, organiser bool
		response               string
		want                   []int
	}{
		{"the organiser's default", nil, false, false, true, "", []int{10}},
		{"yes", nil, false, false, false, responseAccepted, []int{10}},
		{"maybe", nil, false, false, false, responseTentative, []int{10}},
		{"no answer yet", nil, false, false, false, responseNeedsAction, nil},
		{"no", nil, false, false, false, responseDeclined, nil},
		{"an all-day event's", nil, false, true, false, responseAccepted, []int{900}},
		{"their own", []int{0, 30}, true, false, false, responseNeedsAction, []int{0, 30}},
		{"none, chosen", []int{}, true, false, true, "", []int{}},
		{"their own, but they said no", []int{30}, true, false, false, responseDeclined, nil},
	}
	for _, c := range cases {
		if got := remindersIn(c.own, c.set, c.allDay, c.organiser, c.response); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAReminderIsCheckedAsWritten(t *testing.T) {
	if got, err := checkReminders([]int{30, 10, 30, 0}, false); err != nil || !slices.Equal(got, []int{0, 10, 30}) {
		t.Errorf("sorted and once each: %v %v", got, err)
	}
	if got, err := checkReminders([]int{-540}, true); err != nil || !slices.Equal(got, []int{-540}) {
		t.Errorf("09:00 on an all-day event's day: %v %v", got, err)
	}
	var input *EventInputError
	for name, c := range map[string]struct {
		minutes []int
		allDay  bool
	}{
		"six":                        {[]int{1, 2, 3, 4, 5, 6}, false},
		"after a timed event starts": {[]int{-5}, false},
		"five weeks before":          {[]int{5 * 7 * 24 * 60}, false},
	} {
		if _, err := checkReminders(c.minutes, c.allDay); !errors.As(err, &input) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAReminderFiresBeforeItsStartOrItsDayInTheirZone(t *testing.T) {
	start := time.Date(2026, 10, 20, 9, 15, 0, 0, time.UTC)
	timedEvent := &models.Event{StartAt: &start}
	if got := reminderAt(timedEvent, 10, time.UTC); !got.Equal(start.Add(-10 * time.Minute)) {
		t.Errorf("timed: %v", got)
	}
	day := &models.Event{StartDate: "2026-10-21", EndDate: "2026-10-22"}
	kolkata := zone(t, "Asia/Kolkata")
	// 09:00 the day before, in Kolkata: 03:30 UTC on the 20th.
	if got := reminderAt(day, 900, kolkata); !got.Equal(time.Date(2026, 10, 20, 3, 30, 0, 0, time.UTC)) {
		t.Errorf("all-day: %v", got)
	}
}

// Sam organises the review, Maya says yes, Jonas hasn't answered: Sam and Maya
// are reminded 10 minutes before by default, Jonas isn't.
func TestAnEventRemindsWhoOrganisesItOrSaidYes(t *testing.T) {
	h, rem := reminding(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	e := h.create(sam, "Design review", maya, jonas)
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Accepted}); err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(e.ID)
	if err := h.s.planReminders(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 16, 9, 15, 0, 0, time.UTC)
	for _, who := range []*model.UserInfo{sam, maya} {
		j, ok := rem.next(who, e.ID, models.ReminderNext)
		if !ok || !j.at.Equal(start.Add(-10*time.Minute)) || j.p.Minutes != 10 {
			t.Errorf("%s: %+v %v", who.UserDgraphInfo.UserName, j, ok)
		}
	}
	if _, ok := rem.next(jonas, e.ID, models.ReminderNext); ok {
		t.Error("Jonas hasn't answered, so the default doesn't remind him")
	}
	// Jonas sets his own: he is reminded, at the start and half an hour before.
	got, err := h.s.SetReminders(ctx, jonas, e.ID, []int{0, 30})
	if err != nil || got.Default || !slices.Equal(got.Minutes, []int{0, 30}) {
		t.Fatalf("Jonas's reminders: %+v %v", got, err)
	}
	if j, ok := rem.next(jonas, e.ID, models.ReminderNext); !ok || !j.at.Equal(start.Add(-30*time.Minute)) {
		t.Errorf("Jonas's next: %+v %v", j, ok)
	}
	// Maya chooses none; Sam goes back to the default from his own.
	if _, err := h.s.SetReminders(ctx, maya, e.ID, []int{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := rem.next(maya, e.ID, models.ReminderNext); ok {
		t.Error("Maya chose none")
	}
	if got, _ := h.s.Reminders(ctx, maya, e.ID); got.Default || len(got.Minutes) != 0 {
		t.Errorf("Maya's: %+v", got)
	}
	if got, _ := h.s.Reminders(ctx, sam, e.ID); !got.Default || !slices.Equal(got.Minutes, []int{10}) {
		t.Errorf("Sam's: %+v", got)
	}
	// Taken off the event, Jonas is reminded of nothing.
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(e.Sequence), Attendees: &[]GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.planReminders(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := rem.next(jonas, e.ID, models.ReminderNext); ok {
		t.Error("Jonas was taken off and is still reminded")
	}
}

// A weekly stand-up reminds each week; the week Maya declines, she isn't
// reminded, and her next reminder is the week after.
func TestASeriesRemindsEachInstanceButOneDeclined(t *testing.T) {
	h, rem := reminding(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya := h.member("Sam"), h.member("Maya")
	h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	in := h.between(maya, time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon))
	series := in[0].SeriesID
	if _, err := h.s.Answer(ctx, maya, in[0].ID, AnswerInput{Answer: Accepted}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Answer(ctx, maya, in[0].ID, AnswerInput{Answer: Declined, Scope: ScopeThis}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.planReminders(ctx, uuid.MustParse(series), nil); err != nil {
		t.Fatal(err)
	}
	j, ok := rem.next(maya, series, models.ReminderNext)
	if !ok || !j.at.Equal(time.Date(2026, 10, 27, 9, 5, 0, 0, lon)) {
		t.Errorf("Maya's next skips the week she declined: %+v %v", j, ok)
	}
	if j, ok := rem.next(sam, series, models.ReminderNext); !ok || !j.at.Equal(time.Date(2026, 10, 20, 9, 5, 0, 0, lon)) {
		t.Errorf("Sam's next: %+v %v", j, ok)
	}
}

// A reminder fires once, on time, to the person's devices and never their
// inbox, through their holds when they organise it or said yes; then their
// next is planned. One late, or for an event moved since, isn't sent.
func TestAReminderIsSentOnTimeThroughTheirHolds(t *testing.T) {
	h, rem := reminding(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya := h.member("Sam"), h.member("Maya")
	h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	in := h.between(sam, time.Date(2026, 10, 19, 0, 0, 0, 0, lon), time.Date(2026, 11, 4, 0, 0, 0, 0, lon))
	series := in[0].SeriesID
	if _, err := h.s.SetReminders(ctx, maya, series, []int{10}); err != nil {
		t.Fatal(err)
	}
	if err := h.s.planReminders(ctx, uuid.MustParse(series), nil); err != nil {
		t.Fatal(err)
	}
	got := captureNotices(t)
	j, _ := rem.next(sam, series, models.ReminderNext)
	h.clock = j.at.Add(30 * time.Second)
	if err := h.s.fireReminder(ctx, j.person, j.p); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("told %d times", len(*got))
	}
	c := (*got)[0]
	if c.n.Kind != noticeReminder || c.n.Title != "Stand-up in 10 minutes" || c.n.Push == nil || c.n.Push.ID != in[0].ID ||
		len(c.r) != 1 || !c.r[0].Listed || !c.r[0].ThroughHolds || c.n.Data["through_holds"] != "true" || c.n.Path != "/app/calendar/event/"+in[0].ID {
		t.Errorf("reminded %+v to %+v", c.n, c.r)
	}
	if n, ok := rem.next(sam, series, models.ReminderNext); !ok || !n.at.Equal(time.Date(2026, 10, 27, 9, 5, 0, 0, lon)) {
		t.Errorf("Sam's next is next week's: %+v %v", n, ok)
	}
	// Maya hasn't answered: her own reminder keeps to her holds.
	mj, _ := rem.next(maya, series, models.ReminderNext)
	if err := h.s.fireReminder(ctx, mj.person, mj.p); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 || (*got)[1].r[0].ThroughHolds {
		t.Errorf("Maya's reminder: %+v", *got)
	}
	// Found 20 minutes late (the server was down), next week's isn't sent.
	j, _ = rem.next(sam, series, models.ReminderNext)
	h.clock = j.at.Add(20 * time.Minute)
	if err := h.s.fireReminder(ctx, j.person, j.p); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 {
		t.Errorf("a late reminder was sent: %+v", (*got)[2:])
	}
}

// Snoozed, a reminder comes back in five minutes, or when the event starts;
// the next one stays as it was.
func TestASnoozedReminderComesBack(t *testing.T) {
	h, rem := reminding(t)
	ctx := context.Background()
	sam := h.member("Sam")
	e := h.create(sam, "Design review")
	h.clock = time.Date(2026, 10, 16, 9, 5, 0, 0, time.UTC)
	at, err := h.s.Snooze(ctx, sam, e.ID, SnoozeFiveMinutes)
	if err != nil || !at.Equal(time.Date(2026, 10, 16, 9, 10, 0, 0, time.UTC)) {
		t.Fatalf("snoozed until %v %v", at, err)
	}
	j, ok := rem.next(sam, e.ID, models.ReminderSnooze)
	if !ok || !j.at.Equal(at) {
		t.Fatalf("the snooze: %+v %v", j, ok)
	}
	got := captureNotices(t)
	h.clock = at
	if err := h.s.fireReminder(ctx, j.person, j.p); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0].n.Title != "Design review in 5 minutes" {
		t.Errorf("snoozed reminder: %+v", *got)
	}
	if at, err := h.s.Snooze(ctx, sam, e.ID, SnoozeUntilStart); err != nil || !at.Equal(time.Date(2026, 10, 16, 9, 15, 0, 0, time.UTC)) {
		t.Errorf("until it starts: %v %v", at, err)
	}
	var input *EventInputError
	h.clock = time.Date(2026, 10, 16, 9, 20, 0, 0, time.UTC)
	if _, err := h.s.Snooze(ctx, sam, e.ID, SnoozeUntilStart); !errors.As(err, &input) {
		t.Errorf("a start that's passed: %v", err)
	}
}

// A deleted event reminds nobody.
func TestADeletedEventRemindsNobody(t *testing.T) {
	h, rem := reminding(t)
	ctx := context.Background()
	sam := h.member("Sam")
	e := h.create(sam, "Design review")
	id := uuid.MustParse(e.ID)
	if err := h.s.planReminders(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := rem.next(sam, e.ID, models.ReminderNext); !ok {
		t.Fatal("not planned")
	}
	if err := h.s.Delete(ctx, sam, e.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := h.s.planReminders(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := rem.next(sam, e.ID, models.ReminderNext); ok {
		t.Error("a deleted event still reminds")
	}
}

func TestAReminderSaysWhenInWords(t *testing.T) {
	start := time.Date(2026, 10, 16, 9, 15, 0, 0, time.UTC)
	e := &models.Event{Title: "Design review", StartAt: &start, EndAt: func() *time.Time { t := start.Add(time.Hour); return &t }()}
	for _, c := range []struct {
		before time.Duration
		want   string
	}{
		{10 * time.Minute, "Design review in 10 minutes"},
		{time.Minute, "Design review in 1 minute"},
		{0, "Design review is starting"},
		{2 * time.Hour, "Design review in 2 hours"},
		{48 * time.Hour, "Design review in 2 days"},
	} {
		if got, _ := reminderWords(e, 0, false, start.Add(-c.before), time.UTC); got != c.want {
			t.Errorf("%v before: %q, want %q", c.before, got, c.want)
		}
	}
	day := &models.Event{Title: "Offsite", StartDate: "2026-10-21", EndDate: "2026-10-22"}
	if got, _ := reminderWords(day, 900, false, time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), time.UTC); got != "Offsite is tomorrow" {
		t.Errorf("all-day: %q", got)
	}
}

// The calendar's list carries each event's reminders for its reader: their
// own, or the default for what they organise or said yes or maybe to.
func TestTheListCarriesTheReadersReminders(t *testing.T) {
	h, _ := reminding(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.create(sam, "Design review", maya)
	if _, err := h.s.SetReminders(ctx, maya, e.ID, []int{30}); err != nil {
		t.Fatal(err)
	}
	from, to := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	for who, want := range map[*model.UserInfo]struct {
		minutes []int
		def     bool
	}{sam: {[]int{10}, true}, maya: {[]int{30}, false}} {
		list, err := h.s.legacyRange(ctx, who, from, to)
		if err != nil || len(list) != 1 || list[0].Reminders == nil || !slices.Equal(*list[0].Reminders, want.minutes) || list[0].RemindersDefault != want.def {
			t.Errorf("%s: %+v %v", who.UserDgraphInfo.UserName, list, err)
		}
	}
}
