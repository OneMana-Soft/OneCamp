package business

import (
	"context"
	"strings"
	"testing"
	"time"

	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("no zone data for %s", name)
	}
	return loc
}

func mustSchedule(t *testing.T, cfg scheduleConfig, now time.Time) *schedule {
	t.Helper()
	s, err := checkSchedule(cfg, now)
	if err != nil {
		t.Fatalf("%+v: %v", cfg, err)
	}
	return s
}

func nextOf(t *testing.T, s *schedule, after time.Time) time.Time {
	t.Helper()
	at, ok := s.next(after)
	if !ok {
		t.Fatalf("no next run after %s", after)
	}
	return at
}

// 09:00 stays 09:00 in the schedule's own zone when the clocks change, in
// London (25 Oct 2026) and in New York (1 Nov 2026).
func TestAScheduleKeepsItsTimeAcrossTheClockChange(t *testing.T) {
	london := zone(t, "Europe/London")
	friday := time.Date(2026, 10, 23, 12, 0, 0, 0, london)
	s := mustSchedule(t, scheduleConfig{Repeat: "FREQ=WEEKLY;BYDAY=MO", At: "09:00", TZ: "Europe/London"}, friday)
	got := nextOf(t, s, friday)
	if want := time.Date(2026, 10, 26, 9, 0, 0, 0, london); !got.Equal(want) {
		t.Errorf("London after the change: %s, want %s", got, want)
	}
	if got.UTC().Hour() != 9 {
		t.Errorf("09:00 GMT is 09:00 UTC, got %s", got.UTC())
	}

	ny := zone(t, "America/New_York")
	sat := time.Date(2026, 10, 31, 10, 0, 0, 0, ny)
	d := mustSchedule(t, scheduleConfig{Repeat: "FREQ=DAILY", At: "09:00", TZ: "America/New_York"}, sat)
	got = nextOf(t, d, sat)
	if want := time.Date(2026, 11, 1, 9, 0, 0, 0, ny); !got.Equal(want) {
		t.Errorf("New York after the change: %s, want %s", got, want)
	}
	if got.UTC().Hour() != 14 {
		t.Errorf("09:00 EST is 14:00 UTC, got %s", got.UTC())
	}
}

// The first run is the next one the rule names from when it is saved, never
// the day it is saved on when that isn't one of its days.
func TestTheFirstRunIsOneTheScheduleNames(t *testing.T) {
	tuesday := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	s := mustSchedule(t, scheduleConfig{Repeat: "FREQ=WEEKLY;BYDAY=MO", At: "09:00"}, tuesday)
	if s.cfg.From != "2026-10-20" {
		t.Errorf("first day %q, want the day it was saved", s.cfg.From)
	}
	if got, want := nextOf(t, s, tuesday), time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("first run %s, want the next Monday %s", got, want)
	}
	monday := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	m := mustSchedule(t, scheduleConfig{Repeat: "FREQ=WEEKLY;BYDAY=MO", At: "09:00"}, monday)
	if got, want := nextOf(t, m, monday), time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("saved on a Monday before 09:00: %s, want that day %s", got, want)
	}
	weekdays := mustSchedule(t, scheduleConfig{Repeat: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR", At: "17:00"}, tuesday)
	friday := time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC)
	if got, want := nextOf(t, weekdays, friday), time.Date(2026, 10, 26, 17, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("weekdays after Friday 18:00: %s, want Monday %s", got, want)
	}
}

// Every other week keeps the weeks it was saved with.
func TestEveryOtherWeekKeepsItsWeeks(t *testing.T) {
	monday := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	s := mustSchedule(t, scheduleConfig{Repeat: "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO", At: "09:00", From: "2026-10-19"}, monday)
	first := nextOf(t, s, monday)
	if want := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC); !first.Equal(want) {
		t.Errorf("after the first Monday's 09:00: %s, want two weeks on %s", first, want)
	}
	if got, want := nextOf(t, s, first), time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("then %s, want %s", got, want)
	}
}

// "Monthly on the 31st" runs on a short month's last day, and "the last day"
// is the 28th, 29th, 30th or 31st.
func TestMonthEndsAndLeapDays(t *testing.T) {
	jan31 := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)
	s := mustSchedule(t, scheduleConfig{Repeat: "FREQ=MONTHLY;BYMONTHDAY=31", At: "09:00", From: "2026-01-01"}, jan31)
	feb := nextOf(t, s, jan31)
	if want := time.Date(2026, 2, 28, 9, 0, 0, 0, time.UTC); !feb.Equal(want) {
		t.Errorf("31st in February 2026: %s, want %s", feb, want)
	}
	if got, want := nextOf(t, s, feb), time.Date(2026, 3, 31, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("then %s, want back on the 31st %s", got, want)
	}
	last := mustSchedule(t, scheduleConfig{Repeat: "FREQ=MONTHLY;BYMONTHDAY=-1", At: "18:00", From: "2028-01-01"}, jan31)
	leap := time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC)
	if got, want := nextOf(t, last, leap), time.Date(2028, 2, 29, 18, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("last day of February 2028: %s, want %s", got, want)
	}
	firstMonday := mustSchedule(t, scheduleConfig{Repeat: "FREQ=MONTHLY;BYDAY=1MO", At: "09:30"}, jan31)
	if got, want := nextOf(t, firstMonday, jan31), time.Date(2026, 2, 2, 9, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("first Monday of February 2026: %s, want %s", got, want)
	}
}

// A schedule is one the builder offers, at a time and in a zone OneCamp
// reads, or it is refused in words.
func TestSchedulesTheBuilderDoesntOfferAreRefused(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	bad := []scheduleConfig{
		{Repeat: "", At: "09:00"},
		{Repeat: "FREQ=HOURLY", At: "09:00"},
		{Repeat: "FREQ=DAILY;COUNT=3", At: "09:00"},
		{Repeat: "FREQ=DAILY;UNTIL=20261231T000000Z", At: "09:00"},
		{Repeat: "FREQ=WEEKLY", At: "09:00"},
		{Repeat: "FREQ=WEEKLY;BYDAY=2MO", At: "09:00"},
		{Repeat: "FREQ=MONTHLY;BYMONTHDAY=1,15", At: "09:00"},
		{Repeat: "FREQ=MONTHLY;BYDAY=5FR", At: "09:00"},
		{Repeat: "FREQ=YEARLY", At: "09:00"},
		{Repeat: "FREQ=DAILY", At: "9am"},
		{Repeat: "FREQ=DAILY", At: "09:00", TZ: "Mars/Olympus"},
		{Repeat: "FREQ=DAILY", At: "09:00", From: "20 Oct"},
	}
	for _, cfg := range bad {
		if _, err := checkSchedule(cfg, now); err == nil {
			t.Errorf("%+v was taken", cfg)
		}
	}
	if _, err := checkSchedule(scheduleConfig{Repeat: "RRULE:FREQ=DAILY", At: "7:05"}, now); err != nil {
		t.Errorf("a daily schedule at 7:05 was refused: %v", err)
	}
}

// scheduleHarness stands in for the queue and the rule a schedule's job
// reads.
type scheduleHarness struct {
	wf       *workflowModel.Workflow
	planned  []time.Time
	canceled int
}

func newScheduleHarness(t *testing.T, cfg string) *scheduleHarness {
	t.Helper()
	pid := uuid.New()
	h := &scheduleHarness{wf: &workflowModel.Workflow{Id: uuid.New(), Name: "Monday review", IsActive: true, CreatedBy: uuid.New(),
		TriggerType: workflowModel.TriggerSchedule, TriggerConfig: cfg, ProjectId: &pid,
		Actions: `[{"type":"create_task","project_id":"` + pid.String() + `","task_name":"Weekly review"}]`, Conditions: "{}", ElseActions: "[]"}}
	oldGet, oldPlan, oldCancel, oldClock, oldMake := getRule, planScheduleJob, cancelScheduleJob, clock, makeTask
	t.Cleanup(func() {
		getRule, planScheduleJob, cancelScheduleJob, clock, makeTask = oldGet, oldPlan, oldCancel, oldClock, oldMake
	})
	getRule = func(_ context.Context, id uuid.UUID) (*workflowModel.Workflow, error) {
		if id != h.wf.Id {
			return nil, nil
		}
		c := *h.wf
		return &c, nil
	}
	planScheduleJob = func(_ context.Context, id, _ uuid.UUID, at time.Time) error {
		h.planned = append(h.planned, at)
		return nil
	}
	cancelScheduleJob = func(context.Context, uuid.UUID) error {
		h.canceled++
		return nil
	}
	return h
}

func (h *scheduleHarness) job(at time.Time) *jobModel.ScheduledJob {
	return &jobModel.ScheduledJob{Id: uuid.New(), JobType: workflowModel.ScheduleJobType,
		Payload: `{"workflow_id":"` + h.wf.Id.String() + `"}`, RunAt: at}
}

// A schedule's job runs the rule once for the time it was due, as its
// owner, and plans the next; run twice, it runs the rule once.
func TestAScheduledRunHappensOnceAndPlansTheNext(t *testing.T) {
	f := newStore(t)
	h := newScheduleHarness(t, `{"repeat":"FREQ=WEEKLY;BYDAY=MO","at":"09:00","tz":"UTC","from":"2026-10-19"}`)
	due := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	clock = func() time.Time { return due.Add(30 * time.Second) }
	var made []string
	makeTask = func(_ context.Context, who *userModels.UserInfo, _ uuid.UUID, in taskWriter.NewTask) (uuid.UUID, error) {
		made = append(made, who.UserDgraphInfo.Uuid+" "+in.Name)
		return uuid.New(), nil
	}
	job := h.job(due)
	if err := runScheduleJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := runScheduleJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if len(made) != 1 || !strings.HasPrefix(made[0], h.wf.CreatedBy.String()+" ") {
		t.Fatalf("made %v, want one task as the rule's owner", made)
	}
	if _, ok := f.runs[scheduleKey(due)]; !ok {
		t.Errorf("no run keyed by the time it was due")
	}
	if len(h.planned) != 2 || !h.planned[0].Equal(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("planned %v, want the next Monday each time", h.planned)
	}
}

// A run more than two hours late is skipped and said, and the next is
// planned from now.
func TestALateScheduledRunIsSkippedAndSaid(t *testing.T) {
	f := newStore(t)
	h := newScheduleHarness(t, `{"repeat":"FREQ=DAILY","at":"09:00","tz":"UTC","from":"2026-10-19"}`)
	due := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	clock = func() time.Time { return due.Add(3 * time.Hour) }
	makeTask = func(context.Context, *userModels.UserInfo, uuid.UUID, taskWriter.NewTask) (uuid.UUID, error) {
		t.Error("a late run made a task")
		return uuid.Nil, nil
	}
	if err := runScheduleJob(context.Background(), h.job(due)); err != nil {
		t.Fatal(err)
	}
	r, ok := f.runs[scheduleKey(due)]
	if !ok || r.Status != workflowModel.RunSkipped || !strings.Contains(r.Error, "late") {
		t.Fatalf("run %+v, want a skipped run that says it was late", r)
	}
	if len(h.planned) != 1 || !h.planned[0].Equal(time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("planned %v, want tomorrow 09:00", h.planned)
	}
}

// A rule turned off, deleted or changed from a schedule ends its job there,
// and saving one plans or cancels to match.
func TestARulesJobFollowsTheRule(t *testing.T) {
	newStore(t)
	h := newScheduleHarness(t, `{"repeat":"FREQ=DAILY","at":"09:00","from":"2026-10-19"}`)
	clock = func() time.Time { return time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC) }
	if err := syncSchedule(context.Background(), h.wf.Id); err != nil {
		t.Fatal(err)
	}
	if len(h.planned) != 1 || !h.planned[0].Equal(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("planned %v, want tomorrow 09:00", h.planned)
	}
	h.wf.IsActive = false
	if err := syncSchedule(context.Background(), h.wf.Id); err != nil {
		t.Fatal(err)
	}
	if h.canceled != 1 {
		t.Errorf("turning it off cancelled %d jobs, want 1", h.canceled)
	}
	if err := runScheduleJob(context.Background(), h.job(clock())); err != nil {
		t.Fatal(err)
	}
	if len(h.planned) != 1 {
		t.Errorf("a job of a rule that is off planned another: %v", h.planned)
	}
}
