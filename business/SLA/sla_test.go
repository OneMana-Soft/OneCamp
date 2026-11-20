package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	weekHours "github.com/akashc777/OneCamp/business/WeekHours"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	workTaxonomy "github.com/akashc777/OneCamp/business/WorkTaxonomy"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/pgexec"
	model "github.com/akashc777/OneCamp/models/postgres/SLA"
	taskKeyModel "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// laySLA lays the tasks' SLA state as a Lay does: the layer's reads, then its laying.
func laySLA(ctx context.Context, tasks []*dgraphStruct.DgraphTask) {
	pgexec.Together(ctx, layTimers(ctx, tasks))
}

// stubTimers stands f in for the timers' read.
func stubTimers(t *testing.T, f func(context.Context, []uuid.UUID) (map[uuid.UUID]model.Timer, error)) {
	swap(t, &timersRead, func(ids []uuid.UUID) (*pgexec.Read, func() (map[uuid.UUID]model.Timer, error)) {
		return nil, func() (map[uuid.UUID]model.Timer, error) { return f(context.Background(), ids) }
	})
}

func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

var (
	project = uuid.MustParse("1c0ffee0-0000-4000-8000-000000000001")
	kolkata = weekHours.Hours{Days: []int{1, 2, 3, 4, 5}, Start: "00:00", End: "24:00", TZ: "Asia/Kolkata"}
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	loc, _ := time.LoadLocation("Asia/Kolkata")
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func rule(name string, minutes int, priorities ...string) Rule {
	return Rule{ID: uuid.New(), ProjectID: project, Name: name, Priorities: priorities, TargetMinutes: minutes, Clock: ClockBusiness}
}

func TestTheFirstMatchingRuleTimesTheTask(t *testing.T) {
	bugs := rule("Urgent bugs", 60, "urgent")
	bugs.Labels = []string{"bug"}
	urgent := rule("Urgent", 240, "urgent")
	rules := []Rule{bugs, urgent}
	if r, ok := FirstMatch(rules, Facts{Priority: "urgent", Labels: []string{"Bug", "ui"}}); !ok || r.ID != bugs.ID {
		t.Errorf("an urgent bug: %+v %v", r, ok)
	}
	if r, ok := FirstMatch(rules, Facts{Priority: "urgent"}); !ok || r.ID != urgent.ID {
		t.Errorf("an urgent task: %+v %v", r, ok)
	}
	if _, ok := FirstMatch(rules, Facts{Priority: "low"}); ok {
		t.Error("a low task matched")
	}
	form := rule("Forms", 60)
	form.Sources, form.TypeIDs = []string{"form"}, []uuid.UUID{typeOf(nil)}
	if Matches(form, Facts{Source: "app", Type: typeOf(nil)}) || !Matches(form, Facts{Source: "form", Type: typeOf(nil)}) {
		t.Error("a rule's source and type")
	}
}

func TestDefaultsAreUrgentADayAndHighAWeek(t *testing.T) {
	d := Defaults(project)
	if len(d) != 2 || d[0].Priorities[0] != "urgent" || d[0].TargetMinutes != 24*60 || d[1].Priorities[0] != "high" ||
		d[1].TargetMinutes != 5*24*60 || d[0].Clock != ClockBusiness || d[1].Clock != ClockBusiness || !d[0].Virtual {
		t.Fatalf("the defaults: %+v", d)
	}
	// Urgent made Friday 18:00 is due Monday 18:00 in the project's zone.
	due, risk := DueOf(d[0], at(t, "2026-11-13 18:00"), kolkata)
	if !due.Equal(at(t, "2026-11-16 18:00")) || !risk.Equal(at(t, "2026-11-16 12:00")) {
		t.Errorf("urgent's due %v, risk %v", due, risk)
	}
	due, risk = DueOf(d[1], at(t, "2026-11-13 18:00"), kolkata)
	if !due.Equal(at(t, "2026-11-20 18:00")) || !risk.Equal(at(t, "2026-11-19 18:00")) {
		t.Errorf("high's due %v, risk %v", due, risk)
	}
}

func TestAProjectWithNoHoursTimesOnTheBusinessClock(t *testing.T) {
	swap(t, &weekOf, func(context.Context, uuid.UUID) (model.Week, error) { return model.Week{}, nil })
	h, set, err := hoursOf(context.Background(), project)
	if err != nil || set || h.TZ != "UTC" || len(h.Days) != 5 || h.End != "24:00" {
		t.Fatalf("no hours: %+v %v %v", h, set, err)
	}
	var kept string
	swap(t, &keepZone, func(_ context.Context, _ uuid.UUID, zone string, _ uuid.UUID) error { kept = zone; return nil })
	swap(t, &zoneOfPerson, func(context.Context, uuid.UUID) string { return "Asia/Kolkata" })
	scope := policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: project.String()}
	if err := keepZoneOnSwitchOn(context.Background(), scope, json.RawMessage("true"), policyBusiness.Changer{UserID: uuid.New()}); err != nil || kept != "Asia/Kolkata" {
		t.Errorf("switching on keeps the admin's zone: %q %v", kept, err)
	}
}

func urgentTask(arrived time.Time) Task {
	return Task{ID: uuid.New(), Project: project, Facts: Facts{Priority: "urgent", Type: typeOf(nil)}, Status: "todo", Arrived: arrived}
}

func TestAReMatchKeepsTheOriginalStart(t *testing.T) {
	rules := Defaults(project)
	task := urgentTask(at(t, "2026-11-16 10:00"))
	first, ok := Next(nil, task, rules, kolkata, true, at(t, "2026-11-16 10:00"))
	if !ok || first.State != model.Running || !first.StartedAt.Equal(at(t, "2026-11-16 10:00")) {
		t.Fatalf("made urgent: %+v", first)
	}
	task.Priority = "high"
	next, _ := Next(&first, task, rules, kolkata, false, at(t, "2026-11-17 09:00"))
	if !next.StartedAt.Equal(first.StartedAt) || !next.DueAt.Equal(at(t, "2026-11-23 10:00")) {
		t.Errorf("made high a day later: started %v, due %v", next.StartedAt, next.DueAt)
	}
	task.Priority = "low"
	low, _ := Next(&next, task, rules, kolkata, false, at(t, "2026-11-17 10:00"))
	if low.State != model.Ended || low.EndedReason != EndedUnmatched || StateAt(low, at(t, "2026-11-17 10:00")) != None {
		t.Errorf("made low: %+v", low)
	}
	task.Priority = "urgent"
	back, _ := Next(&low, task, rules, kolkata, false, at(t, "2026-11-18 10:00"))
	if !back.StartedAt.Equal(first.StartedAt) || StateAt(back, at(t, "2026-11-18 10:00")) != Breached {
		t.Errorf("urgent again, past its first due: %+v", back)
	}
}

func TestADoneTaskMeetsItsTimer(t *testing.T) {
	rules := Defaults(project)
	task := urgentTask(at(t, "2026-11-16 10:00"))
	start, _ := Next(nil, task, rules, kolkata, true, task.Arrived)
	task.Status = dgraphStruct.TASK_STATUS_DONE
	in := at(t, "2026-11-16 20:00")
	task.DoneAt = &in
	met, _ := Next(&start, task, rules, kolkata, false, in)
	if met.State != model.Met || StateAt(met, at(t, "2026-11-30 10:00")) != Met {
		t.Errorf("done in time: %+v", met)
	}
	late := at(t, "2026-11-17 11:00")
	task.DoneAt = &late
	missed, _ := Next(&start, task, rules, kolkata, false, late)
	if missed.State != model.Missed || StateAt(missed, late) != Breached {
		t.Errorf("done after its due: %+v", missed)
	}
}

func TestACancelledTaskEndsItsTimer(t *testing.T) {
	rules := Defaults(project)
	task := urgentTask(at(t, "2026-11-16 10:00"))
	start, _ := Next(nil, task, rules, kolkata, true, task.Arrived)
	task.Status = dgraphStruct.TASK_STATUS_CANCELED
	gone, _ := Next(&start, task, rules, kolkata, false, at(t, "2026-11-16 11:00"))
	if gone.State != model.Ended || gone.EndedReason != EndedCancelled || StateAt(gone, at(t, "2026-11-16 11:00")) != None {
		t.Errorf("cancelled: %+v", gone)
	}
	task.Status = "todo"
	again, _ := Next(&gone, task, rules, kolkata, false, at(t, "2026-11-16 12:00"))
	if again.State != model.Running || !again.DueAt.Equal(*start.DueAt) {
		t.Errorf("reopened: %+v", again)
	}
}

// sinkWorld stands in for the stores the sink reads and writes.
type sinkWorld struct {
	on    bool
	live  []Rule
	ever  bool
	kept  map[uuid.UUID]model.Timer
	puts  int
	reads int
}

func newSinkWorld(t *testing.T, task *dgraphStruct.DgraphTask) *sinkWorld {
	w := &sinkWorld{on: true, kept: map[uuid.UUID]model.Timer{}}
	swap(t, &slaOn, func(context.Context, uuid.UUID) bool { return w.on })
	swap(t, &rulesOf, func(context.Context, uuid.UUID) ([]Rule, bool, error) { return w.live, w.ever, nil })
	swap(t, &weekOf, func(context.Context, uuid.UUID) (model.Week, error) { return model.Week{Zone: "Asia/Kolkata"}, nil })
	swap(t, &taskOf, func(context.Context, string) (*dgraphStruct.DgraphTask, error) { w.reads++; return task, nil })
	swap(t, &typesOf, func(context.Context, []uuid.UUID) (map[uuid.UUID]taskKeyModel.Facts, error) {
		return map[uuid.UUID]taskKeyModel.Facts{}, nil
	})
	swap(t, &timerOf, func(_ context.Context, id uuid.UUID) (model.Timer, bool, error) {
		x, ok := w.kept[id]
		return x, ok, nil
	})
	swap(t, &putTimer, func(_ context.Context, x model.Timer) error { w.puts++; w.kept[x.TaskID] = x; return nil })
	return w
}

func made(id uuid.UUID, kind string, arrived time.Time) workFeed.Entry {
	return workFeed.Entry{Kind: kind, ResourceKind: "task", ResourceUUID: id.String(), ProjectUUID: project.String(), Source: "app", At: arrived}
}

func TestTheSameChangeTwiceKeepsOneTimer(t *testing.T) {
	arrived := at(t, "2026-11-13 18:00")
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Status: "todo", Priority: "urgent", CreatedAt: &arrived}
	w := newSinkWorld(t, task)
	swap(t, &clock, func() time.Time { return arrived.Add(time.Minute) })
	id := uuid.MustParse(task.Uuid)
	for range 2 {
		if err := retime(context.Background(), "ev", made(id, workFeed.KindCreated, arrived)); err != nil {
			t.Fatal(err)
		}
	}
	got := w.kept[id]
	if w.puts != 1 || len(w.kept) != 1 || !got.StartedAt.Equal(arrived) || !got.DueAt.Equal(at(t, "2026-11-16 18:00")) {
		t.Errorf("%d writes, %+v", w.puts, got)
	}
}

func TestOffStartsNothingAndRemovesNothing(t *testing.T) {
	arrived := at(t, "2026-11-13 18:00")
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Status: "todo", Priority: "urgent", CreatedAt: &arrived}
	w := newSinkWorld(t, task)
	w.on = false
	if err := retime(context.Background(), "ev", made(uuid.MustParse(task.Uuid), workFeed.KindCreated, arrived)); err != nil || w.puts != 0 || w.reads != 0 {
		t.Errorf("off: %d writes, %d reads, %v", w.puts, w.reads, err)
	}
}

func TestARemovedRuleIsKeptSoftAndTimesNothingNew(t *testing.T) {
	arrived := at(t, "2026-11-13 18:00")
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Status: "todo", Priority: "urgent", CreatedAt: &arrived}
	w := newSinkWorld(t, task)
	w.ever = true // its only rule was removed: no defaults come back
	id := uuid.MustParse(task.Uuid)
	if err := retime(context.Background(), "ev", made(id, workFeed.KindCreated, arrived)); err != nil {
		t.Fatal(err)
	}
	if got := w.kept[id]; got.State != model.Ended || got.EndedReason != EndedUnmatched || got.DueAt != nil {
		t.Errorf("a task made after its rule was removed: %+v", got)
	}
}

// audit stands in for the audit log: ok false can't record.
func audit(t *testing.T, ok bool) *[]string {
	var rows []string
	swap(t, &principal, func(context.Context) auditBusiness.Principal {
		id := uuid.New()
		return auditBusiness.Principal{ID: &id}
	})
	swap(t, &recorded, func(_ context.Context, _ auditBusiness.Principal, action, _, _ string, _ map[string]interface{}, change func() error) error {
		if !ok {
			return auditBusiness.ErrNotRecorded
		}
		rows = append(rows, action)
		return change()
	})
	swap(t, &inTx, func(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) })
	swap(t, &lockRules, func(context.Context, uuid.UUID) error { return nil })
	return &rows
}

func savedRules(t *testing.T) *[]Rule {
	var saved []Rule
	swap(t, &rulesOf, func(context.Context, uuid.UUID) ([]Rule, bool, error) { return saved, len(saved) > 0, nil })
	swap(t, &saveRule, func(_ context.Context, r Rule, _ uuid.UUID, _ bool) (bool, error) {
		saved = append(saved, r)
		return true, nil
	})
	swap(t, &saveRules, func(_ context.Context, rs []Rule, _ uuid.UUID) error { saved = append(saved, rs...); return nil })
	return &saved
}

var bugsIn = RuleInput{Name: "Bugs", Priorities: []string{"urgent"}, Labels: []string{"bug"}, TargetMinutes: 120}

func TestOnlyProjectAdminsSetSLAs(t *testing.T) {
	rows := audit(t, true)
	saved := savedRules(t)
	member := workAccess.Standing{ProjectUUID: project.String(), Role: workAccess.RoleMember}
	var refused *Refused
	if _, err := AddRule(context.Background(), member, project, bugsIn); !errors.As(err, &refused) || len(*saved) != 0 || len(*rows) != 0 {
		t.Errorf("a member: %v, %d saved", err, len(*saved))
	}
	if _, err := SetHours(context.Background(), member, project, &kolkata); !errors.As(err, &refused) {
		t.Errorf("a member's hours: %v", err)
	}
	admin := workAccess.Standing{ProjectUUID: project.String(), Role: workAccess.RoleAdmin}
	r, err := AddRule(context.Background(), admin, project, bugsIn)
	// The defaults become the project's own, then the new rule follows them.
	if err != nil || len(*saved) != 3 || r.Rank != 2 || (*saved)[2].Labels[0] != "bug" || len(*rows) != 1 {
		t.Errorf("an admin: %+v %v, %d saved", r, err, len(*saved))
	}
	swap(t, &weekOf, func(context.Context, uuid.UUID) (model.Week, error) { return model.Week{}, nil })
	swap(t, &slaOn, func(context.Context, uuid.UUID) bool { return true })
	swap(t, &readerOf, func(context.Context) string { return uuid.NewString() })
	swap(t, &isGuest, func(context.Context, string) bool { return false })
	v, err := Settings(context.Background(), member, project)
	if err != nil || v.CanManage || v.Defaults || len(v.Rules) != 3 {
		t.Errorf("a member reads: %+v %v", v, err)
	}
}

func TestAnSLAChangeIsAuditedOrNotMade(t *testing.T) {
	audit(t, false)
	saved := savedRules(t)
	admin := workAccess.Standing{ProjectUUID: project.String(), Role: workAccess.RoleAdmin}
	if _, err := AddRule(context.Background(), admin, project, bugsIn); !errors.Is(err, auditBusiness.ErrNotRecorded) || len(*saved) != 0 {
		t.Errorf("unrecorded: %v, %d saved", err, len(*saved))
	}
	var weeks int
	swap(t, &setWeek, func(context.Context, uuid.UUID, []byte, string, uuid.UUID) error { weeks++; return nil })
	if _, err := SetHours(context.Background(), admin, project, &kolkata); !errors.Is(err, auditBusiness.ErrNotRecorded) || weeks != 0 {
		t.Errorf("hours unrecorded: %v, %d written", err, weeks)
	}
}

// A removed rule comes back from the trash (POST /restore/sla_rule/{id}) by
// the project's admins only, within its most rules, audited or not made.
func TestARemovedRuleComesBackOnlyByItsAdminsAndAudited(t *testing.T) {
	rows := audit(t, true)
	savedRules(t) // a restore reads the project's rules for room first
	var restored []uuid.UUID
	back := func(_ context.Context, p, id uuid.UUID) (bool, error) {
		if p != project {
			t.Errorf("restored in project %s", p)
		}
		restored = append(restored, id)
		return true, nil
	}
	swap(t, &restoreRule, back)
	id := uuid.New()
	member := workAccess.Standing{ProjectUUID: project.String(), Role: workAccess.RoleMember}
	var refused *Refused
	if err := RestoreRule(context.Background(), member, project, id); !errors.As(err, &refused) || len(restored) != 0 || len(*rows) != 0 {
		t.Errorf("a member restores: %v, %d restored, audit %v", err, len(restored), *rows)
	}
	admin := workAccess.Standing{ProjectUUID: project.String(), Role: workAccess.RoleAdmin}
	if err := RestoreRule(context.Background(), admin, project, id); err != nil || len(restored) != 1 || restored[0] != id ||
		len(*rows) != 1 || (*rows)[0] != "project.sla.rule.restored" {
		t.Errorf("an admin restores: %v, %v, audit %v", err, restored, *rows)
	}
	swap(t, &restoreRule, func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil })
	if err := RestoreRule(context.Background(), admin, project, id); !errors.Is(err, helpers.ErrNotFound) {
		t.Errorf("nothing to restore: %v", err)
	}
	audit(t, false)
	restored = nil
	swap(t, &restoreRule, back)
	if err := RestoreRule(context.Background(), admin, project, id); !errors.Is(err, auditBusiness.ErrNotRecorded) || len(restored) != 0 {
		t.Errorf("unrecorded: %v, %d restored", err, len(restored))
	}
}

// layWorld has one timer, due tomorrow, on a task in project.
func layWorld(t *testing.T, reader string, members map[string]bool, on bool) *dgraphStruct.DgraphTask {
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString()}
	due := time.Now().Add(48 * time.Hour)
	risk := due.Add(-24 * time.Hour)
	swap(t, &readerOf, func(context.Context) string { return reader })
	stubTimers(t, func(context.Context, []uuid.UUID) (map[uuid.UUID]model.Timer, error) {
		id := uuid.MustParse(task.Uuid)
		return map[uuid.UUID]model.Timer{id: {TaskID: id, ProjectID: project, State: model.Running, DueAt: &due, RiskAt: &risk}}, nil
	})
	swap(t, &membersAmong, func(_ context.Context, asked map[string][]string) (map[string]map[string]bool, error) {
		out := map[string]map[string]bool{}
		for p := range asked {
			out[p] = members
		}
		return out, nil
	})
	swap(t, &isGuest, func(context.Context, string) bool { return false })
	swap(t, &onIn, func(_ context.Context, projects []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, p := range projects {
			out[p] = on
		}
		return out, nil
	})
	return task
}

func TestSLAStateRidesOnlyOnTasksTheReaderMayRead(t *testing.T) {
	member := uuid.NewString()
	task := layWorld(t, member, map[string]bool{member: true}, true)
	laySLA(context.Background(), []*dgraphStruct.DgraphTask{task})
	if task.SLA == nil || task.SLA.State != string(OnTrack) || task.SLA.DueAt == nil {
		t.Errorf("a member reads %+v", task.SLA)
	}
	outsider := uuid.NewString()
	task = layWorld(t, outsider, map[string]bool{}, true)
	laySLA(context.Background(), []*dgraphStruct.DgraphTask{task})
	if task.SLA != nil {
		t.Errorf("someone outside the project reads %+v", task.SLA)
	}
	task = layWorld(t, member, map[string]bool{member: true}, false)
	laySLA(context.Background(), []*dgraphStruct.DgraphTask{task})
	if task.SLA != nil {
		t.Errorf("SLAs off, a member reads %+v", task.SLA)
	}
}

// A failed read of the projects' SLA switches lays no SLA state: the read
// fails closed, whatever the answer holds beside its error (a fallback to
// the workspace's value says SLAs are on here).
func TestAFailedSwitchReadLaysNoSLAState(t *testing.T) {
	member := uuid.NewString()
	task := layWorld(t, member, map[string]bool{member: true}, true)
	swap(t, &onIn, func(_ context.Context, projects []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, p := range projects {
			out[p] = true
		}
		return out, errors.New("policy down")
	})
	laySLA(context.Background(), []*dgraphStruct.DgraphTask{task})
	if task.SLA != nil {
		t.Errorf("a failed switch read laid %+v", task.SLA)
	}
}

// A guest reads no SLA state, even with a member's place in the project (a
// stale role row, an import): what they read of a task carries none.
func TestAGuestsTaskReadCarriesNoSLAState(t *testing.T) {
	guest := uuid.NewString()
	task := layWorld(t, guest, map[string]bool{guest: true}, true)
	swap(t, &isGuest, func(_ context.Context, who string) bool { return who == guest })
	laySLA(context.Background(), []*dgraphStruct.DgraphTask{task})
	raw, _ := json.Marshal(task)
	if task.SLA != nil || strings.Contains(string(raw), "task_sla") {
		t.Errorf("a guest reads %s", raw)
	}
}

// A task the reader sees only as restricted (a stub, models/taskread, or a
// task marked so with its id still on it) carries no SLA state, whatever it
// held before and however it reaches the layer, and its timer isn't asked
// for (V-1057); a task the reader reads in the same answer keeps its own.
// With no reader, every task leaves with none: the layer fails closed.
func TestARestrictedTaskCarriesNoSLAState(t *testing.T) {
	member := uuid.NewString()
	shown := layWorld(t, member, map[string]bool{member: true}, true)
	stale := func() *dgraphStruct.TaskSLA { return &dgraphStruct.TaskSLA{State: string(Breached)} }
	marked := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Hidden: true, SLA: stale()}
	stub := taskread.Stub(&dgraphStruct.DgraphTask{Uuid: uuid.NewString()})
	stub.SLA = stale()
	due := time.Now().Add(-time.Hour)
	var asked []uuid.UUID
	stubTimers(t, func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]model.Timer, error) {
		asked = append(asked, ids...)
		out := map[uuid.UUID]model.Timer{}
		for _, raw := range []string{shown.Uuid, marked.Uuid, stub.StubOf} {
			id := uuid.MustParse(raw)
			out[id] = model.Timer{TaskID: id, ProjectID: project, State: model.Running, DueAt: &due, RiskAt: &due}
		}
		return out, nil
	})
	carriesNone := func(how string, tasks ...*dgraphStruct.DgraphTask) {
		t.Helper()
		for _, task := range tasks {
			if raw, _ := json.Marshal(task); task.SLA != nil || strings.Contains(string(raw), "task_sla") {
				t.Errorf("%s: %s", how, raw)
			}
		}
	}
	laySLA(context.Background(), []*dgraphStruct.DgraphTask{marked, shown, stub})
	if shown.SLA == nil || shown.SLA.State != string(Breached) {
		t.Errorf("the task the member reads beside them: %+v", shown.SLA)
	}
	carriesNone("a restricted task, as a member who may not read it", marked, stub)
	if slices.Contains(asked, uuid.MustParse(marked.Uuid)) || slices.Contains(asked, uuid.MustParse(stub.StubOf)) {
		t.Errorf("a restricted task's timer was asked for: %v", asked)
	}
	swap(t, &readerOf, func(context.Context) string { return "" })
	marked.SLA, stub.SLA = stale(), stale()
	laySLA(context.Background(), []*dgraphStruct.DgraphTask{marked, shown, stub})
	carriesNone("no reader", marked, shown, stub)
}

func TestSLAsHaveNoPlanGate(t *testing.T) {
	for _, f := range helpers.LockedFeatures() {
		if strings.Contains(strings.ToLower(string(f)), "sla") {
			t.Errorf("SLAs are gated by %q", f)
		}
	}
	scope := policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: project.String()}
	if in, err := policyBusiness.Admin.InfoIn(context.Background(), policyBusiness.ProjectsSLA.Key, scope); err != nil || in.Value != false {
		t.Errorf("each project turns its own SLAs on, off by default: %+v %v", in, err)
	}
}

// A timer is at risk from its risk time until its due, and a member's read of
// its task carries that.
func TestATimerIsAtRiskFromItsRiskTimeUntilItsDue(t *testing.T) {
	risk, due := at(t, "2026-11-13 18:00"), at(t, "2026-11-16 18:00")
	timer := model.Timer{State: model.Running, DueAt: &due, RiskAt: &risk}
	for _, c := range []struct {
		now  time.Time
		want State
	}{{risk.Add(-time.Minute), OnTrack}, {risk, AtRisk}, {due.Add(-time.Minute), AtRisk}, {due, Breached}} {
		if got := StateAt(timer, c.now); got != c.want {
			t.Errorf("at %s: %q, want %q", c.now, got, c.want)
		}
	}
	member := uuid.NewString()
	task := layWorld(t, member, map[string]bool{member: true}, true)
	soon, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	stubTimers(t, func(context.Context, []uuid.UUID) (map[uuid.UUID]model.Timer, error) {
		id := uuid.MustParse(task.Uuid)
		return map[uuid.UUID]model.Timer{id: {TaskID: id, ProjectID: project, State: model.Running, DueAt: &soon, RiskAt: &past}}, nil
	})
	laySLA(context.Background(), []*dgraphStruct.DgraphTask{task})
	raw, _ := json.Marshal(task)
	if task.SLA == nil || task.SLA.State != string(AtRisk) || !strings.Contains(string(raw), `"task_sla":{"state":"at_risk"`) {
		t.Errorf("a member reads a task past its risk time: %s", raw)
	}
}

// A project keeps at most MaxRules: the next add or restore is refused
// before it is recorded, and saves nothing; an add that found room but finds
// the project full under its lock (a rule added meanwhile) saves nothing.
func TestAProjectKeepsAtMostFiftyRules(t *testing.T) {
	rows := audit(t, true)
	saved := savedRules(t)
	for i := range MaxRules {
		*saved = append(*saved, rule(fmt.Sprintf("Rule %d", i), 60))
	}
	admin := workAccess.Standing{ProjectUUID: project.String(), Role: workAccess.RoleAdmin}
	if _, err := AddRule(context.Background(), admin, project, bugsIn); !errors.Is(err, errFull) || len(*saved) != MaxRules || len(*rows) != 0 {
		t.Errorf("the 51st add: %v, %d saved, audit %v", err, len(*saved), *rows)
	}
	restored := 0
	swap(t, &restoreRule, func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { restored++; return true, nil })
	if err := RestoreRule(context.Background(), admin, project, uuid.New()); !errors.Is(err, errFull) || restored != 0 || len(*rows) != 0 {
		t.Errorf("a restore into a full project: %v, %d restored, audit %v", err, restored, *rows)
	}
	*saved = (*saved)[:MaxRules-1]
	reads := 0
	swap(t, &rulesOf, func(context.Context, uuid.UUID) ([]Rule, bool, error) {
		if reads++; reads > 1 {
			return append(slices.Clone(*saved), rule("Meanwhile", 60)), true, nil
		}
		return *saved, true, nil
	})
	if _, err := AddRule(context.Background(), admin, project, bugsIn); !errors.Is(err, errFull) || len(*saved) != MaxRules-1 {
		t.Errorf("full under the lock: %v, %d saved", err, len(*saved))
	}
}

// The first change to a default writes the defaults as the project's own:
// its audit row names the rule it changed or removed as written, beside the
// default's fixed id.
func TestTheAuditNamesTheRuleTheFirstChangeWrote(t *testing.T) {
	audit(t, true)
	var metas []map[string]any
	swap(t, &recorded, func(_ context.Context, _ auditBusiness.Principal, _, _, _ string, meta map[string]interface{}, change func() error) error {
		metas = append(metas, meta)
		return change()
	})
	admin := workAccess.Standing{ProjectUUID: project.String(), Role: workAccess.RoleAdmin}
	saved := savedRules(t)
	out, err := ChangeRule(context.Background(), admin, project, defaultUrgent, RuleInput{Name: "Urgent", Priorities: []string{"urgent"}, TargetMinutes: 600})
	if err != nil || len(*saved) < 2 || out.ID != (*saved)[0].ID || len(metas) != 1 ||
		metas[0]["rule_id"] != out.ID.String() || metas[0]["default_rule_id"] != defaultUrgent.String() {
		t.Errorf("the first change to Urgent: %+v %v, audit %v", out, err, metas)
	}
	saved = savedRules(t)
	var dropped uuid.UUID
	swap(t, &dropRule, func(_ context.Context, _, id, _ uuid.UUID) (bool, error) { dropped = id; return true, nil })
	if err := RemoveRule(context.Background(), admin, project, defaultHigh); err != nil || len(*saved) != 2 || dropped != (*saved)[1].ID ||
		len(metas) != 2 || metas[1]["rule_id"] != dropped.String() || metas[1]["default_rule_id"] != defaultHigh.String() {
		t.Errorf("the first removal of High: %v, dropped %s, audit %v", err, dropped, metas)
	}
}

// A rule names only the project's types (a built-in one or its own); one it
// named already stays named when it changes (a type archived since).
func TestARuleNamesOnlyTheProjectsTypes(t *testing.T) {
	audit(t, true)
	saved := savedRules(t)
	own := uuid.New()
	types := []taskKeyModel.Type{{ID: workTaxonomy.TypeBug}, {ID: own}}
	swap(t, &projectTypes, func(_ context.Context, p uuid.UUID) ([]taskKeyModel.Type, error) {
		if p != project {
			t.Errorf("the types of project %s", p)
		}
		return types, nil
	})
	admin := workAccess.Standing{ProjectUUID: project.String(), Role: workAccess.RoleAdmin}
	in := bugsIn
	in.TypeIDs = []string{uuid.NewString()}
	var problem *Problem
	if _, err := AddRule(context.Background(), admin, project, in); !errors.As(err, &problem) || len(*saved) != 0 {
		t.Errorf("a type that isn't the project's: %v, %d saved", err, len(*saved))
	}
	in.TypeIDs = []string{workTaxonomy.TypeBug.String(), own.String()}
	r, err := AddRule(context.Background(), admin, project, in)
	if err != nil || len(r.TypeIDs) != 2 {
		t.Fatalf("the project's types: %+v %v", r, err)
	}
	types = types[:1]
	in.TargetMinutes = 300
	if _, err := ChangeRule(context.Background(), admin, project, r.ID, in); err != nil {
		t.Errorf("a rule keeps its type archived since: %v", err)
	}
}

// The SLA settings are SLA state: a guest reads none of them, even with a
// member's place; a member reads them.
func TestAGuestReadsNoSLASettings(t *testing.T) {
	savedRules(t)
	swap(t, &weekOf, func(context.Context, uuid.UUID) (model.Week, error) { return model.Week{}, nil })
	swap(t, &slaOn, func(context.Context, uuid.UUID) bool { return true })
	guest, member := uuid.NewString(), uuid.NewString()
	swap(t, &isGuest, func(_ context.Context, who string) bool { return who == guest })
	standing := workAccess.Standing{ProjectUUID: project.String(), Role: workAccess.RoleMember}
	var refused *Refused
	swap(t, &readerOf, func(context.Context) string { return guest })
	if v, err := Settings(context.Background(), standing, project); !errors.As(err, &refused) || v.Rules != nil {
		t.Errorf("a guest reads %+v %v", v, err)
	}
	swap(t, &readerOf, func(context.Context) string { return member })
	if v, err := Settings(context.Background(), standing, project); err != nil || !v.On || len(v.Rules) != 2 {
		t.Errorf("a member reads %+v %v", v, err)
	}
}

// A task a visitor files through a public form is timed as from a form: a
// rule for forms takes it, and not a task made in the app.
func TestAFormsTaskIsTimedAsFromAForm(t *testing.T) {
	arrived := at(t, "2026-11-13 18:00")
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Status: "todo", Priority: "low", CreatedAt: &arrived}
	w := newSinkWorld(t, task)
	forms := rule("From forms", 120)
	forms.Sources = []string{"form"}
	w.live, w.ever = []Rule{forms}, true
	swap(t, &clock, func() time.Time { return arrived.Add(time.Minute) })
	app, form := uuid.New(), uuid.New()
	if err := retime(context.Background(), "ev", made(app, workFeed.KindCreated, arrived)); err != nil || w.kept[app].State == model.Running {
		t.Errorf("made in the app: %+v %v, want no running timer", w.kept[app], err)
	}
	if err := retime(helpers.WithPublicSubmission(context.Background()), "ev", made(form, workFeed.KindCreated, arrived)); err != nil {
		t.Fatal(err)
	}
	if got := w.kept[form]; got.State != model.Running || got.Source != "form" || got.RuleID == nil || *got.RuleID != forms.ID {
		t.Errorf("filed through a form: %+v, want running under %s from a form", got, forms.ID)
	}
}

// The sink runs on the Outbox's context, which names no reader, and a timer
// is told to nobody: it reads its task as the system, so a restricted task is
// timed as any other (V-1016).
func TestTheTimerReadsItsTaskAsTheSystem(t *testing.T) {
	arrived := at(t, "2026-11-13 18:00")
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Status: "todo", Priority: "urgent", CreatedAt: &arrived}
	w := newSinkWorld(t, task)
	var system []bool
	swap(t, &taskOf, func(ctx context.Context, _ string) (*dgraphStruct.DgraphTask, error) {
		system = append(system, taskread.System(ctx))
		return task, nil
	})
	swap(t, &clock, func() time.Time { return arrived.Add(time.Minute) })
	id := uuid.MustParse(task.Uuid)
	if err := retime(context.Background(), "ev", made(id, workFeed.KindCreated, arrived)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(system, []bool{true}) || w.kept[id].State != model.Running {
		t.Errorf("the timer's task read as the system: %v, timer %+v; want [true], running", system, w.kept[id])
	}
}

// A project's SLA week keeps no dates of their own (a booking page's, gcal-7):
// SetHours drops a "dates" key as the hours ignored it before, so After counts
// the week only.
func TestSLAHoursKeepNoDates(t *testing.T) {
	audit(t, true)
	var stored []byte
	swap(t, &setWeek, func(_ context.Context, _ uuid.UUID, raw []byte, _ string, _ uuid.UUID) error {
		stored = raw
		return nil
	})
	admin := workAccess.Standing{ProjectUUID: project.String(), Role: workAccess.RoleAdmin}
	var in weekHours.Hours
	if err := json.Unmarshal([]byte(`{"days":[1,2,3,4,5],"start":"09:00","end":"17:00","tz":"UTC","dates":{"2026-11-23":[]}}`), &in); err != nil {
		t.Fatal(err)
	}
	got, err := SetHours(context.Background(), admin, project, &in)
	if err != nil || got.Dates != nil || strings.Contains(string(stored), "dates") {
		t.Fatalf("stored %s, returned %+v, %v", stored, got, err)
	}
	// Mon 23 Nov 09:00 and an hour on is 10:00 that Monday, a weekday like any other.
	from := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	if due, ok := weekHours.After(got, from, time.Hour); !ok || !due.Equal(from.Add(time.Hour)) {
		t.Errorf("due %v, %v", due, ok)
	}
}
