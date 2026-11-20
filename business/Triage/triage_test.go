package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	triageModel "github.com/akashc777/OneCamp/models/postgres/Triage"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// world answers the seams: which projects have triage on, what waits, what
// the person may do, and what each change did.
type world struct {
	on       map[string]bool
	waiting  map[uuid.UUID]uuid.UUID
	count    int
	counts   int // countWaiting calls
	inserts  int // put calls
	puts     []triageModel.Item
	primed   []string
	role     workAccess.Role
	items    []triageModel.Item
	readable map[string]taskBusiness.ReadableTask
	tasks    map[uuid.UUID]*dgraphStruct.DgraphTask
	refuse   error
	needs    map[uuid.UUID]workAccess.Need
	applied  []string
	applyErr error
	held     bool
	asker    *uuid.UUID
	readers  map[string]bool
	// unreadable is the tasks (by uuid) no one in readers may read.
	unreadable map[string]bool
	told       []notificationBusiness.Notice
	toldTo     [][]notificationBusiness.NoticeRecipient
	kept       []triageModel.Decision
	said       map[uuid.UUID]string
	events     []resourceEventBusiness.Event
	clock      time.Time
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{on: map[string]bool{}, role: workAccess.RoleMember, tasks: map[uuid.UUID]*dgraphStruct.DgraphTask{},
		needs: map[uuid.UUID]workAccess.Need{}, held: true, said: map[uuid.UUID]string{}, clock: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	on0, prime0, put0, count0, in0, all0, keep0, project0, standing0, readable0, taskFor0, apply0, comment0, emit0, now0 :=
		triageOn, primeProjects, put, countWaiting, waitingIn, allWaiting, keep, projectFor, standingIn, readable, taskFor, apply, comment, emit, now
	t.Cleanup(func() {
		triageOn, primeProjects, put, countWaiting, waitingIn, allWaiting, keep, projectFor, standingIn, readable, taskFor, apply, comment, emit, now =
			on0, prime0, put0, count0, in0, all0, keep0, project0, standing0, readable0, taskFor0, apply0, comment0, emit0, now0
	})
	triageOn = func(_ context.Context, id string) bool { return w.on[id] }
	needs0 := needsPriority
	t.Cleanup(func() { needsPriority = needs0 })
	needsPriority = func(context.Context, string) bool { return false }
	primeProjects = func(_ context.Context, ids []string) { w.primed = append(w.primed, ids...) }
	put = func(_ context.Context, its []triageModel.Item) error {
		w.inserts++
		w.puts = append(w.puts, its...)
		return nil
	}
	countWaiting = func(context.Context, uuid.UUID) (int, error) { w.counts++; return w.count, nil }
	waitingIn = func(context.Context, uuid.UUID, int) ([]triageModel.Item, error) { return w.items, nil }
	allWaiting = func(context.Context) (map[uuid.UUID]uuid.UUID, error) { return w.waiting, nil }
	keep = func(ctx context.Context, d triageModel.Decision, change func(context.Context) error) (bool, *uuid.UUID, error) {
		if !w.held {
			return false, nil, nil
		}
		if err := change(ctx); err != nil {
			return false, nil, err
		}
		w.kept = append(w.kept, d)
		return true, w.asker, nil
	}
	readers0, tell0, named0 := readersOf, tell, named
	t.Cleanup(func() { readersOf, tell, named = readers0, tell0, named0 })
	readersOf = func(_ context.Context, task string, ids []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, id := range ids {
			out[id] = w.readers[id] && !w.unreadable[task]
		}
		return out, nil
	}
	tell = func(_ context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) []string {
		w.told, w.toldTo = append(w.told, n), append(w.toldTo, to)
		return nil
	}
	named = func(_ context.Context, _ string, name string) string { return name }
	projectFor = func(_ context.Context, id string, _ *userModels.UserInfo) (*dgraphStruct.DgraphProject, error) {
		return &dgraphStruct.DgraphProject{Uuid: id}, nil
	}
	standingIn = func(context.Context, *userModels.UserInfo, *dgraphStruct.DgraphProject) (workAccess.Standing, error) {
		return workAccess.Standing{Role: w.role}, nil
	}
	readable = func(context.Context, []string, string) (map[string]taskBusiness.ReadableTask, error) {
		return w.readable, nil
	}
	taskFor = func(_ context.Context, id uuid.UUID, _ *userModels.UserInfo, need workAccess.Need) (*dgraphStruct.DgraphTask, workAccess.Standing, error) {
		w.needs[id] = need
		if w.refuse != nil {
			return nil, workAccess.Standing{}, w.refuse
		}
		task, ok := w.tasks[id]
		if !ok {
			return nil, workAccess.Standing{}, taskBusiness.ErrTaskNotVisible
		}
		return task, workAccess.Standing{Role: w.role}, nil
	}
	apply = func(_ context.Context, _ *userModels.UserInfo, _ uuid.UUID, ops []taskWriter.Op, _ int64) (*dgraphStruct.DgraphTask, []taskWriter.Change, error) {
		var status string
		if json.Unmarshal(ops[0].Value, &status) != nil {
			var l struct {
				Kind string `json:"kind"`
			}
			_ = json.Unmarshal(ops[0].Value, &l)
			status = l.Kind
		}
		w.applied = append(w.applied, string(ops[0].Kind)+" "+status)
		return nil, nil, w.applyErr
	}
	comment = func(_ context.Context, _ *userModels.UserInfo, id uuid.UUID, body string) (string, error) {
		w.said[id] = body
		return "c1", nil
	}
	emit = func(_ context.Context, e resourceEventBusiness.Event) { w.events = append(w.events, e) }
	now = func() time.Time { return w.clock }
	arrived0 := arrivedEvent
	t.Cleanup(func() { arrivedEvent = arrived0 })
	arrivedEvent = func(context.Context, string, map[string]interface{}) {}
	return w
}

// task is a task in a project the world knows.
func (w *world) task(name string, project uuid.UUID) uuid.UUID {
	id := uuid.New()
	w.tasks[id] = &dgraphStruct.DgraphTask{Uuid: id.String(), Name: name, Project: &dgraphStruct.DgraphProject{Uuid: project.String()}}
	return id
}

func member() *userModels.UserInfo {
	who := &userModels.UserInfo{}
	who.UserPostgresInfo.Id = uuid.New()
	who.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0x9", Uuid: who.UserPostgresInfo.Id.String()}
	return who
}

func TestTheListsFilterIsWiredAtInit(t *testing.T) {
	if taskread.Waiting == nil {
		t.Fatal("business/Triage left taskread.Waiting unset: lists would show what waits in triage")
	}
}

// A door's task waits only in a project with triage on, and only while it
// has room; who asked is kept within its columns.
func TestHoldTakesWorkInOnlyWhereTriageIsOn(t *testing.T) {
	w := newWorld(t)
	ctx, project := context.Background(), uuid.New()
	if held, err := Hold(ctx, project, uuid.New(), FromForm, Requester{}); held || err != nil || len(w.puts) != 0 {
		t.Fatalf("triage off: held %v, %v, kept %d", held, err, len(w.puts))
	}
	w.on[project.String()] = true
	task, me := uuid.New(), uuid.New()
	held, err := Hold(ctx, project, task, FromAPI, Requester{UserUUID: &me, Name: strings.Repeat("é", 150), Email: "a@b.co"})
	if !held || err != nil || len(w.puts) != 1 {
		t.Fatalf("triage on: held %v, %v, kept %d", held, err, len(w.puts))
	}
	got := w.puts[0]
	if got.TaskUUID != task || got.ProjectUUID != project || got.Source != "api" || got.RequesterUUID == nil || *got.RequesterUUID != me ||
		got.RequesterName != strings.Repeat("é", 100) || got.RequesterEmail != "a@b.co" {
		t.Errorf("kept %+v", got)
	}
	w.count = MaxWaiting
	if held, err := Hold(ctx, project, uuid.New(), FromForm, Requester{}); held || err != nil || len(w.puts) != 1 {
		t.Errorf("a full triage took more: held %v, %v", held, err)
	}
}

// Lists leave out what waits only where the project has triage on: turning
// it off lets what waits back into the lists.
func TestListsLeaveOutWhatWaitsWhereTriageIsOn(t *testing.T) {
	w := newWorld(t)
	on, off := uuid.New(), uuid.New()
	inOn, inOff := uuid.New(), uuid.New()
	w.waiting = map[uuid.UUID]uuid.UUID{inOn: on, inOff: off}
	w.on[on.String()] = true
	got, err := waitingNow(context.Background())
	if err != nil || len(got) != 1 || !got[inOn.String()] {
		t.Errorf("left out %v, %v", got, err)
	}
	if len(w.primed) != 2 {
		t.Errorf("read the projects' triage one at a time: primed %v", w.primed)
	}
	w.waiting = nil
	if got, err := waitingNow(context.Background()); got != nil || err != nil {
		t.Errorf("nothing waits: %v, %v", got, err)
	}
}

// The queue shows what waits that the person may read, and nothing while
// triage is off or to someone outside the project.
func TestTheQueueShowsWhatThePersonMayRead(t *testing.T) {
	w := newWorld(t)
	ctx, project, who := context.Background(), uuid.New(), member()
	shown, kept := uuid.New(), uuid.New()
	w.items = []triageModel.Item{{TaskUUID: shown, ProjectUUID: project}, {TaskUUID: kept, ProjectUUID: project}}
	w.readable = map[string]taskBusiness.ReadableTask{shown.String(): {Name: "New logo", Status: "todo"}}
	if got, err := Triage.Waiting(ctx, who, project); got != nil || err != nil {
		t.Errorf("triage off: %v, %v", got, err)
	}
	w.on[project.String()] = true
	got, err := Triage.Waiting(ctx, who, project)
	if err != nil || len(got) != 1 || got[0].TaskUUID != shown || got[0].Name != "New logo" {
		t.Errorf("the queue: %+v, %v", got, err)
	}
	w.role = workAccess.RoleNone
	if _, err := Triage.Waiting(ctx, who, project); !errors.Is(err, taskBusiness.ErrTaskNotVisible) {
		t.Errorf("someone outside the project read it: %v", err)
	}
}

// Each decision changes the task while it waits, then is kept with its
// reason, says what it must on a task, and is told as triage.decided.
func TestADecisionChangesTheTaskThenIsKept(t *testing.T) {
	project := uuid.New()
	cases := []struct {
		name    string
		choice  func(w *world, task uuid.UUID) Choice
		applied []string
		check   func(t *testing.T, w *world, task uuid.UUID, d triageModel.Decision)
	}{
		{"accept into a status", func(*world, uuid.UUID) Choice {
			return Choice{Decision: Accept, Status: "inProgress", Reason: "  On the roadmap  "}
		},
			[]string{"status inProgress"}, func(t *testing.T, w *world, _ uuid.UUID, d triageModel.Decision) {
				if d.Reason != "On the roadmap" || len(w.said) != 0 {
					t.Errorf("reason %q, said %v", d.Reason, w.said)
				}
			}},
		{"accept as it is", func(*world, uuid.UUID) Choice { return Choice{Decision: Accept} }, nil, nil},
		{"decline", func(*world, uuid.UUID) Choice { return Choice{Decision: Decline, Reason: "Not something we make"} },
			[]string{"status canceled"}, func(t *testing.T, w *world, task uuid.UUID, _ triageModel.Decision) {
				if w.said[task] != "Declined in triage: Not something we make" {
					t.Errorf("said %v", w.said)
				}
			}},
		{"duplicate", func(w *world, _ uuid.UUID) Choice {
			return Choice{Decision: Duplicate, DuplicateOf: w.task("Original logo", project)}
		}, []string{"status canceled", "relation duplicates"}, func(t *testing.T, w *world, task uuid.UUID, d triageModel.Decision) {
			if d.DuplicateOf == nil || w.needs[*d.DuplicateOf] != workAccess.NeedComment {
				t.Fatalf("duplicate of %v, read as %v", d.DuplicateOf, w.needs)
			}
			if !strings.Contains(w.said[*d.DuplicateOf], "“New logo” was marked a duplicate of this") || w.said[task] != "" {
				t.Errorf("said %v", w.said)
			}
		}},
		{"snooze", func(w *world, _ uuid.UUID) Choice {
			return Choice{Decision: Snooze, Until: w.clock.Add(48 * time.Hour)}
		},
			nil, func(t *testing.T, w *world, _ uuid.UUID, d triageModel.Decision) {
				if d.SnoozedUntil == nil || !d.SnoozedUntil.Equal(w.clock.Add(48*time.Hour)) {
					t.Errorf("snoozed until %v", d.SnoozedUntil)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			w.on[project.String()] = true
			task := w.task("New logo", project)
			choice := c.choice(w, task)
			who := member()
			if err := Triage.Decide(context.Background(), who, task, choice); err != nil {
				t.Fatal(err)
			}
			if strings.Join(w.applied, ",") != strings.Join(c.applied, ",") {
				t.Errorf("changed %v, want %v", w.applied, c.applied)
			}
			if w.needs[task] != workAccess.NeedEditTask {
				t.Errorf("read the task as %v; deciding changes it", w.needs[task])
			}
			if len(w.kept) != 1 || w.kept[0].Decision != string(choice.Decision) || w.kept[0].ProjectUUID != project || w.kept[0].DecidedBy != who.UserPostgresInfo.Id {
				t.Fatalf("kept %+v", w.kept)
			}
			if len(w.events) != 1 || w.events[0].Kind != resourceEventBusiness.TriageDecided || w.events[0].Data["decision"] != string(choice.Decision) {
				t.Errorf("told %+v", w.events)
			}
			if c.check != nil {
				c.check(t, w, task, w.kept[0])
			}
		})
	}
}

// A decision that can't be made changes nothing, keeps nothing and tells no
// one.
func TestADecisionThatCantBeMadeChangesNothing(t *testing.T) {
	project := uuid.New()
	cases := []struct {
		name   string
		setup  func(w *world, task uuid.UUID) Choice
		wanted error
	}{
		{"an unknown decision", func(*world, uuid.UUID) Choice { return Choice{Decision: "move"} }, ErrBadChoice},
		{"a duplicate of nothing", func(*world, uuid.UUID) Choice { return Choice{Decision: Duplicate} }, ErrBadChoice},
		{"a duplicate of itself", func(_ *world, task uuid.UUID) Choice { return Choice{Decision: Duplicate, DuplicateOf: task} }, ErrBadChoice},
		{"a duplicate of a task out of sight", func(*world, uuid.UUID) Choice { return Choice{Decision: Duplicate, DuplicateOf: uuid.New()} }, ErrBadChoice},
		{"a snooze into the past", func(w *world, _ uuid.UUID) Choice { return Choice{Decision: Snooze, Until: w.clock.Add(-time.Minute)} }, ErrBadChoice},
		{"a snooze past a year", func(w *world, _ uuid.UUID) Choice {
			return Choice{Decision: Snooze, Until: w.clock.Add(366 * 24 * time.Hour)}
		}, ErrBadChoice},
		{"someone who can't change the task", func(w *world, _ uuid.UUID) Choice {
			w.refuse = workAccess.ErrNotAllowed
			return Choice{Decision: Accept}
		}, workAccess.ErrNotAllowed},
		{"triage off", func(w *world, _ uuid.UUID) Choice {
			w.on[project.String()] = false
			return Choice{Decision: Accept}
		}, ErrTriageOff},
		{"decided already", func(w *world, _ uuid.UUID) Choice {
			w.held = false
			return Choice{Decision: Decline, Reason: "late"}
		}, ErrNotWaiting},
		{"a change the task refuses", func(w *world, _ uuid.UUID) Choice {
			w.applyErr = errors.New("moving it to Canceled waits for approval")
			return Choice{Decision: Decline}
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			w.on[project.String()] = true
			task := w.task("New logo", project)
			err := Triage.Decide(context.Background(), member(), task, c.setup(w, task))
			if err == nil || (c.wanted != nil && !errors.Is(err, c.wanted)) {
				t.Fatalf("err %v, want %v", err, c.wanted)
			}
			if len(w.kept) != 0 || len(w.said) != 0 || len(w.events) != 0 {
				t.Errorf("kept %v, said %v, told %v", w.kept, w.said, w.events)
			}
			if c.name != "a change the task refuses" && len(w.applied) != 0 {
				t.Errorf("changed the task: %v", w.applied)
			}
		})
	}
}

// A project can ask for a priority before work leaves triage (linear-04 to_win
// item 2): accepting a task without one is refused in words and changes
// nothing; with one, or with the rule off, it is accepted.
func TestAcceptWaitsForAPriorityWhereTheProjectAsksForOne(t *testing.T) {
	w := newWorld(t)
	project := uuid.New()
	w.on[project.String()] = true
	asks := map[string]bool{project.String(): true}
	was := needsPriority
	t.Cleanup(func() { needsPriority = was })
	needsPriority = func(_ context.Context, id string) bool { return asks[id] }
	bare := w.task("No priority yet", project)
	err := Triage.Decide(context.Background(), member(), bare, Choice{Decision: Accept})
	if !errors.Is(err, ErrNeedsPriority) || len(w.kept) != 0 || len(w.applied) != 0 {
		t.Fatalf("accepted without a priority: %v kept=%v applied=%v", err, w.kept, w.applied)
	}
	// Declining, snoozing and marking a duplicate never ask for one.
	if err := Triage.Decide(context.Background(), member(), bare, Choice{Decision: Decline, Reason: "spam"}); err != nil {
		t.Fatalf("decline asked for a priority: %v", err)
	}
	set := w.task("Has one", project)
	w.tasks[set].Priority = "high"
	if err := Triage.Decide(context.Background(), member(), set, Choice{Decision: Accept}); err != nil {
		t.Fatalf("a task with a priority wasn't accepted: %v", err)
	}
	asks[project.String()] = false
	other := w.task("Rule off", project)
	if err := Triage.Decide(context.Background(), member(), other, Choice{Decision: Accept}); err != nil {
		t.Fatalf("the rule off still asked: %v", err)
	}
}

// The member who asked for a task is told in their Inbox how triage decided
// it, while they may read the task now: a decline with its reason as they
// wrote it (plain text), a duplicate naming the original only while they may
// read it too (else "another task", with no name or key). A form's visitor,
// one who can't read the task, one who decided it themselves, and a snooze
// tell no one.
func TestTheRequesterIsToldOfTheDecisionWhileTheyMayReadTheTask(t *testing.T) {
	project := uuid.New()
	asker := uuid.New()
	cases := []struct {
		name  string
		setup func(w *world, task uuid.UUID) (*userModels.UserInfo, Choice)
		told  bool
		title string
		body  string
	}{
		{"accept", func(*world, uuid.UUID) (*userModels.UserInfo, Choice) {
			return member(), Choice{Decision: Accept}
		}, true, "accepted your request", ""},
		{"decline with its reason", func(*world, uuid.UUID) (*userModels.UserInfo, Choice) {
			return member(), Choice{Decision: Decline, Reason: "Not <b>ours</b> & never was"}
		}, true, "declined your request", "Not <b>ours</b> & never was"},
		{"duplicate", func(w *world, _ uuid.UUID) (*userModels.UserInfo, Choice) {
			return member(), Choice{Decision: Duplicate, DuplicateOf: w.task("Original logo", project)}
		}, true, "marked your request", "Original logo"},
		{"a duplicate of a task the requester can't read", func(w *world, _ uuid.UUID) (*userModels.UserInfo, Choice) {
			original := w.task("Secret launch plan", project)
			w.unreadable = map[string]bool{original.String(): true}
			return member(), Choice{Decision: Duplicate, DuplicateOf: original}
		}, true, "marked your request", "It repeats another task."},
		{"a requester who can't read the task", func(w *world, _ uuid.UUID) (*userModels.UserInfo, Choice) {
			w.readers[asker.String()] = false
			return member(), Choice{Decision: Accept}
		}, false, "", ""},
		{"a form's visitor", func(w *world, _ uuid.UUID) (*userModels.UserInfo, Choice) {
			w.asker = nil
			return member(), Choice{Decision: Decline, Reason: "spam"}
		}, false, "", ""},
		{"the requester decided it", func(w *world, _ uuid.UUID) (*userModels.UserInfo, Choice) {
			who := member()
			who.UserPostgresInfo.Id = asker
			return who, Choice{Decision: Accept}
		}, false, "", ""},
		{"a snooze", func(w *world, _ uuid.UUID) (*userModels.UserInfo, Choice) {
			return member(), Choice{Decision: Snooze, Until: w.clock.Add(time.Hour)}
		}, false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			w.on[project.String()] = true
			w.asker = &asker
			w.readers = map[string]bool{asker.String(): true}
			task := w.task("New logo", project)
			who, choice := c.setup(w, task)
			if err := Triage.Decide(context.Background(), who, task, choice); err != nil {
				t.Fatal(err)
			}
			if !c.told {
				if len(w.told) != 0 {
					t.Errorf("told %+v", w.told)
				}
				return
			}
			if len(w.told) != 1 || len(w.toldTo[0]) != 1 || w.toldTo[0][0].UserUUID != asker.String() {
				t.Fatalf("told %+v to %+v", w.told, w.toldTo)
			}
			n := w.told[0]
			if n.Kind != "work.triage.decided" || n.DedupKey != "triage|"+task.String()+"|"+string(choice.Decision) ||
				n.Path != "/app/task/"+task.String() || n.ResourceUUID != task.String() || n.ResourceKind != "task" {
				t.Errorf("the notice %+v", n)
			}
			if !strings.Contains(n.Title, c.title) || !strings.Contains(n.Title, "New logo") || !strings.Contains(n.Body, c.body) {
				t.Errorf("title %q body %q", n.Title, n.Body)
			}
			// "Secret launch plan" is the one task its requester can't read.
			if strings.Contains(n.Title+n.Body, "Secret") {
				t.Errorf("the notice names a task its reader can't read: %q %q", n.Title, n.Body)
			}
		})
	}
}

// A duplicate is merged into its original (linear-21 to_win 2): besides being
// canceled, it is linked to the original as duplicating it, through the Task
// Writer as the person deciding. A link refused leaves the decision standing.
func TestADuplicateIsLinkedToItsOriginal(t *testing.T) {
	for _, refused := range []bool{false, true} {
		w := newWorld(t)
		project := uuid.New()
		w.on[project.String()] = true
		task, original := w.task("New logo", project), w.task("Original logo", project)
		var links []map[string]string
		inner := apply
		apply = func(ctx context.Context, who *userModels.UserInfo, id uuid.UUID, ops []taskWriter.Op, v int64) (*dgraphStruct.DgraphTask, []taskWriter.Change, error) {
			if ops[0].Kind == taskWriter.OpRelation {
				var l map[string]string
				_ = json.Unmarshal(ops[0].Value, &l)
				l["on"] = id.String()
				links = append(links, l)
				if refused {
					return nil, nil, workAccess.ErrNotAllowed
				}
				return nil, nil, nil
			}
			return inner(ctx, who, id, ops, v)
		}
		if err := Triage.Decide(context.Background(), member(), task, Choice{Decision: Duplicate, DuplicateOf: original}); err != nil {
			t.Fatalf("refused %v: %v", refused, err)
		}
		if len(links) != 1 || links[0]["kind"] != "duplicates" || links[0]["task"] != original.String() || links[0]["on"] != task.String() || links[0]["direction"] != "out" {
			t.Errorf("refused %v: linked %v", refused, links)
		}
		if len(w.kept) != 1 || len(w.events) != 1 {
			t.Errorf("refused %v: kept %v told %v", refused, w.kept, w.events)
		}
	}
}
