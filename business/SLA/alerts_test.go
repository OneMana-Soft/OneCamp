package business

import (
	"context"
	"errors"
	"html"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	workSweep "github.com/akashc777/OneCamp/business/WorkSweep"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/SLA"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
)

// alertWorld is the timers kept, the tasks and switches the sweep reads, and
// what it told: Due and Claim answer as the store's do (Claim takes only what
// is still untold), and Tell tells a key once.
type alertWorld struct {
	mu      sync.Mutex
	timers  map[uuid.UUID]*model.Timer
	tasks   map[string]*dgraphStruct.DgraphTask
	off     map[string]bool
	told    []alertTelling
	keys    map[string]bool
	events  []resourceEventBusiness.Event
	asked   []taskBusiness.Guests
	stolen  bool // another sweep claims everything first
	project uuid.UUID
}

type alertTelling struct {
	n  notificationBusiness.Notice
	to notificationBusiness.NoticeRecipient
}

// untold is whether t has an alert due and untold at now, and which: as
// models/postgres/SLA dueNow reads it.
func untold(t *model.Timer, now time.Time) (breach, due bool) {
	if t.State != model.Running || t.EndedAt != nil || t.DueAt == nil {
		return false, false
	}
	if !now.Before(*t.DueAt) && t.BreachToldAt == nil {
		return true, true
	}
	return false, t.RiskAt != nil && !now.Before(*t.RiskAt) && now.Before(*t.DueAt) && t.WarnedAt == nil
}

func newAlertWorld(t *testing.T) *alertWorld {
	t.Helper()
	w := &alertWorld{timers: map[uuid.UUID]*model.Timer{}, tasks: map[string]*dgraphStruct.DgraphTask{}, off: map[string]bool{},
		keys: map[string]bool{}, project: uuid.New()}
	swap(t, &dueAlerts, func(_ context.Context, now time.Time, most int) ([]model.Alert, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		var out []model.Alert
		for _, tm := range w.timers {
			if breach, ok := untold(tm, now); ok && len(out) < most {
				out = append(out, model.Alert{TaskID: tm.TaskID, ProjectID: tm.ProjectID, DueAt: *tm.DueAt, Breach: breach})
			}
		}
		return out, nil
	})
	swap(t, &claimAlerts, func(_ context.Context, ids []uuid.UUID, now time.Time) ([]model.Alert, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		var out []model.Alert
		for _, id := range ids {
			tm := w.timers[id]
			breach, ok := untold(tm, now)
			if !ok {
				continue
			}
			at := now
			if breach {
				tm.BreachToldAt = &at
			}
			if tm.WarnedAt == nil {
				tm.WarnedAt = &at
			}
			if !w.stolen {
				out = append(out, model.Alert{TaskID: tm.TaskID, ProjectID: tm.ProjectID, DueAt: *tm.DueAt, Breach: breach})
			}
		}
		return out, nil
	})
	swap(t, &alertTasks, func(_ context.Context, ids []string) (map[string]*dgraphStruct.DgraphTask, error) {
		out := map[string]*dgraphStruct.DgraphTask{}
		for _, id := range ids {
			if task, ok := w.tasks[id]; ok {
				out[id] = task
			}
		}
		return out, nil
	})
	swap(t, &onIn, func(_ context.Context, projects []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, p := range projects {
			out[p] = !w.off[p]
		}
		return out, nil
	})
	// Everyone given a task hears of it, but a guest of members' news.
	swap(t, &hears, func(_ context.Context, task *dgraphStruct.DgraphTask, actor string, g taskBusiness.Guests) (string, bool) {
		w.asked = append(w.asked, g)
		if task.Assignee == nil || actor != "" {
			return "", false
		}
		return task.Assignee.Uuid, !(strings.HasPrefix(task.Assignee.UserName, "guest") && g == taskBusiness.MembersOnly)
	})
	swap(t, &tellAlert, func(_ context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) []string {
		w.mu.Lock()
		defer w.mu.Unlock()
		var told []string
		for _, r := range to {
			if !w.keys[n.DedupKey+"|"+r.UserUUID] {
				w.keys[n.DedupKey+"|"+r.UserUUID] = true
				w.told = append(w.told, alertTelling{n, r})
				told = append(told, r.UserUUID)
			}
		}
		return told
	})
	swap(t, &emitAlert, func(_ context.Context, e resourceEventBusiness.Event) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.events = append(w.events, e)
	})
	return w
}

// task is a task named name in the world's project, given to whom (nil for
// nobody), whose timer started at start with a target of minutes on the
// project's default work week (Monday to Friday, all day, UTC).
func (w *alertWorld) task(t *testing.T, name string, whom *dgraphStruct.DgraphUser, start time.Time, minutes int) *dgraphStruct.DgraphTask {
	t.Helper()
	id := uuid.New()
	rule := Rule{ID: uuid.New(), ProjectID: w.project, Name: "Urgent", TargetMinutes: minutes, Clock: ClockBusiness, Starts: StartsArrival}
	tm, ok := Next(nil, Task{ID: id, Project: w.project, Facts: Facts{Priority: "urgent", Type: typeOf(nil)}, Status: "todo", Arrived: start},
		[]Rule{rule}, DefaultHours("UTC"), true, start)
	if !ok || tm.State != model.Running {
		t.Fatalf("no running timer: %+v", tm)
	}
	w.timers[id] = &tm
	task := &dgraphStruct.DgraphTask{Uuid: id.String(), Name: name, Status: "todo", Assignee: whom,
		Project: &dgraphStruct.DgraphProject{Uuid: w.project.String(), Name: "Spring launch"}}
	w.tasks[id.String()] = task
	return task
}

// sweepAt runs one pass of the sweep at now.
func (w *alertWorld) sweepAt(t *testing.T, now time.Time) {
	t.Helper()
	swap(t, &clock, func() time.Time { return now })
	if err := workSweep.SLA(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (w *alertWorld) toldOf(task string) []alertTelling {
	var out []alertTelling
	for _, x := range w.told {
		if x.n.ResourceUUID == task {
			out = append(out, x)
		}
	}
	return out
}

func person(name string) *dgraphStruct.DgraphUser {
	return &dgraphStruct.DgraphUser{Uuid: uuid.NewString(), UserName: name}
}

var (
	monday   = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	week     = 5 * 24 * 60 // five business days, in minutes
	friday9  = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	saturday = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
)

func TestAtRiskTellsTheAssigneeOnce(t *testing.T) {
	w := newAlertWorld(t)
	ben := person("ben")
	odd := `<b>Ship</b> & "launch"`
	task := w.task(t, odd, ben, monday, week)

	// Due Monday 23rd at 10:00, five business days on; at risk a business day
	// before, Friday 20th at 10:00 (the calendar's day before is Sunday).
	w.sweepAt(t, friday9)
	if got := w.toldOf(task.Uuid); len(got) != 0 {
		t.Fatalf("told before its risk: %+v", got)
	}
	w.sweepAt(t, saturday)
	w.sweepAt(t, saturday.Add(5*time.Minute))
	got := w.toldOf(task.Uuid)
	if len(got) != 1 {
		t.Fatalf("Saturday, a business day from due: told %d times, want once", len(got))
	}
	n, to := got[0].n, got[0].to
	due := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	if to.UserUUID != ben.Uuid || to.Reason != "assignee" || to.InboxOnly || n.Kind != NoticeAtRisk ||
		n.DedupKey != NoticeAtRisk+"|"+task.Uuid+"|"+due.Format(time.RFC3339Nano) || n.Email != notificationBusiness.EventTaskSLA ||
		n.Path != "/app/task/"+task.Uuid || n.Body != "Spring launch" || n.Push == nil || n.Push.ID != task.Uuid {
		t.Errorf("told %+v as %+v", to, n)
	}
	if len(w.events) != 1 || w.events[0].Kind != resourceEventBusiness.SLAAtRisk || w.events[0].ResourceUUID != task.Uuid ||
		!w.events[0].Actor.System || w.events[0].Data["project_uuid"] != w.project.String() {
		t.Errorf("events: %+v", w.events)
	}
	// The email shows the name as written: services/Email escapes it once.
	page, text, err := emailService.Render(emailService.TemplateData{Title: n.Title, Body: n.Body})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "<b>Ship</b>") || !strings.Contains(html.UnescapeString(page), odd) || !strings.Contains(text, odd) {
		t.Errorf("the email doesn't show %q as written", odd)
	}
}

func TestABreachTellsOnceAcrossTwoSweeps(t *testing.T) {
	w := newAlertWorld(t)
	ben, tom := person("ben"), person("tom")
	warned := w.task(t, "Warned", ben, monday, week)
	never := w.task(t, "Never warned", tom, monday, week)
	w.sweepAt(t, saturday)
	w.told = nil
	past := time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC)
	w.timers[uuid.MustParse(never.Uuid)].WarnedAt = nil
	w.sweepAt(t, past)
	w.sweepAt(t, past.Add(time.Minute))
	for _, task := range []*dgraphStruct.DgraphTask{warned, never} {
		got := w.toldOf(task.Uuid)
		if len(got) != 1 || got[0].n.Kind != NoticeBreached || got[0].n.Title != task.Name+" breached its SLA" {
			t.Errorf("%s past due, two sweeps: told %+v, want one breach", task.Name, got)
		}
	}
	// A sweep tells only what it claimed: another took this one first.
	late := w.task(t, "Taken", ben, monday, week)
	w.stolen = true
	w.sweepAt(t, past.Add(2*time.Minute))
	if got := w.toldOf(late.Uuid); len(got) != 0 {
		t.Errorf("told what another sweep claimed: %+v", got)
	}
}

func TestAMovedDueTellsAgain(t *testing.T) {
	w := newAlertWorld(t)
	ben := person("ben")
	task := w.task(t, "Moved", ben, monday, week)
	w.sweepAt(t, saturday)
	// Its priority changes and a new rule's due moves it a day on: PutTimer
	// clears what was told.
	tm := w.timers[uuid.MustParse(task.Uuid)]
	due, risk := tm.DueAt.Add(24*time.Hour), tm.RiskAt.Add(24*time.Hour)
	tm.DueAt, tm.RiskAt, tm.WarnedAt = &due, &risk, nil
	w.sweepAt(t, risk.Add(time.Hour))
	if got := w.toldOf(task.Uuid); len(got) != 2 || got[0].n.DedupKey == got[1].n.DedupKey {
		t.Fatalf("a moved due: told %d times, want twice under two keys", len(got))
	}
}

func TestNobodyWhoCantReadTheTaskIsTold(t *testing.T) {
	w := newAlertWorld(t)
	locked := w.task(t, "Restricted", person("lee"), monday, week)
	swap(t, &hears, func(context.Context, *dgraphStruct.DgraphTask, string, taskBusiness.Guests) (string, bool) {
		return "", false
	})
	gone := w.task(t, "Deleted", person("ben"), monday, week)
	when := time.Now()
	gone.DeletedAt = &when
	done := w.task(t, "Done", person("ben"), monday, week)
	done.Status = dgraphStruct.TASK_STATUS_DONE
	w.sweepAt(t, saturday)
	if len(w.told) != 0 {
		t.Fatalf("told: %+v", w.told)
	}
	if len(w.events) != 1 || w.events[0].ResourceUUID != locked.Uuid {
		t.Errorf("events: %+v, want the live task's alone", w.events)
	}
}

func TestAnUnassignedTaskTellsNobody(t *testing.T) {
	w := newAlertWorld(t)
	swap(t, &hears, taskBusiness.AssigneeHears) // the real check: nobody to ask
	w.task(t, "Nobody's", nil, monday, week)
	w.sweepAt(t, saturday)
	if len(w.told) != 0 {
		t.Fatalf("told: %+v", w.told)
	}
}

func TestUrgentAndSLAShareOneHearerCheck(t *testing.T) {
	if reflect.ValueOf(hears).Pointer() != reflect.ValueOf(taskBusiness.AssigneeHears).Pointer() {
		t.Fatal("SLA alerts ask someone other than business/Task AssigneeHears who hears")
	}
	w := newAlertWorld(t)
	w.task(t, "Asked", person("ben"), monday, week)
	w.sweepAt(t, saturday)
	if len(w.asked) != 1 || w.asked[0] != taskBusiness.MembersOnly {
		t.Errorf("asked %v, want once, members only", w.asked)
	}
}

func TestAGuestIsNeverToldOfAnSLA(t *testing.T) {
	w := newAlertWorld(t)
	gus := w.task(t, "Gus's", person("guest-gus"), monday, week)
	ben := w.task(t, "Ben's", person("ben"), monday, week)
	w.sweepAt(t, saturday)
	if len(w.toldOf(gus.Uuid)) != 0 || len(w.toldOf(ben.Uuid)) != 1 {
		t.Errorf("told %+v: a guest is never told, a member is", w.told)
	}
}

// A failed read of the projects' switches claims nothing and tells nobody:
// the sweep fails, and the next one finds the alert still due (CR-305 F2).
func TestAFailedSwitchReadClaimsNothing(t *testing.T) {
	w := newAlertWorld(t)
	task := w.task(t, "Switches", person("ben"), monday, week)
	id := uuid.MustParse(task.Uuid)
	read := onIn
	swap(t, &onIn, func(context.Context, []string) (map[string]bool, error) { return nil, errors.New("policy down") })
	swap(t, &clock, func() time.Time { return saturday })
	if err := workSweep.SLA(context.Background()); err == nil {
		t.Error("a sweep whose switches couldn't be read answered nil")
	}
	if tm := w.timers[id]; tm.WarnedAt != nil || tm.BreachToldAt != nil || len(w.told) != 0 || len(w.events) != 0 {
		t.Fatalf("a failed switch read: claimed %+v, told %+v, events %+v; want nothing", tm, w.told, w.events)
	}
	onIn = read
	w.sweepAt(t, saturday.Add(time.Minute))
	if len(w.told) != 1 {
		t.Errorf("the next sweep: told %+v, want the alert once", w.told)
	}
}

func TestSLAsOffTellNobody(t *testing.T) {
	w := newAlertWorld(t)
	w.off[w.project.String()] = true
	task := w.task(t, "Off", person("ben"), monday, week)
	w.sweepAt(t, saturday)
	if len(w.told) != 0 || len(w.events) != 0 {
		t.Fatalf("SLAs off: told %+v, events %+v", w.told, w.events)
	}
	// Claimed all the same, so the page moves on; on again, it isn't replayed.
	if w.timers[uuid.MustParse(task.Uuid)].WarnedAt == nil {
		t.Error("an alert due while SLAs are off wasn't claimed: it would hold the page")
	}
	w.off = map[string]bool{}
	w.sweepAt(t, saturday.Add(time.Minute))
	if len(w.told) != 0 {
		t.Errorf("switching SLAs on replayed: %+v", w.told)
	}
}
