package business

import (
	"context"
	"errors"
	"reflect"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

var (
	syncDoc  = uuid.MustParse("0d0c0000-0000-4000-8000-000000000001")
	syncTask = uuid.MustParse("7a5c0000-0000-4000-8000-000000000002")
	syncProj = uuid.MustParse("9e0c0000-0000-4000-8000-000000000003")
	syncDoc2 = uuid.MustParse("0d0c0000-0000-4000-8000-000000000005")
)

// world stands in for the stores the sync reads and writes, and puts them back.
type world struct {
	level     accessBusiness.Level
	admin     bool
	outside   bool
	tied      bool
	written   []string
	ticks     []map[string]bool
	tiedNew   []uuid.UUID
	untied    []uuid.UUID
	answers   map[uuid.UUID]collab.TaskChecks
	failures  map[uuid.UUID]error
	made      *adapter.CreateOrUpdateTaskInput
	published []mqttStruct.MqttTaskChanged
}

func newWorld(t *testing.T) *world {
	w := &world{level: accessBusiness.LevelEdit, admin: true, tied: true}
	saved := []any{tieDocTask, docHoldsTask, docsHolding, setChecks, docLevel, readProject, readTask, writeStatus, makeTask, publishTaskChanged, untieDocTask}
	db := postgresInit.DBConn
	t.Cleanup(func() {
		postgresInit.DBConn = db
		tieDocTask = saved[0].(func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error)
		docHoldsTask = saved[1].(func(context.Context, uuid.UUID, uuid.UUID) (bool, error))
		docsHolding = saved[2].(func(context.Context, uuid.UUID) ([]uuid.UUID, error))
		setChecks = saved[3].(func(context.Context, string, map[string]bool, collab.Options) (collab.TaskChecks, error))
		docLevel = saved[4].(func(context.Context, *model.UserInfo, string) (accessBusiness.Level, error))
		readProject = saved[5].(func(context.Context, string, string) (*dgraphStruct.DgraphProject, error))
		readTask = saved[6].(func(context.Context, string, string) (*dgraphStruct.DgraphTask, error))
		writeStatus = saved[7].(func(context.Context, uuid.UUID, string, *dgraphStruct.DgraphTask, *dgraphStruct.DgraphUser) error)
		makeTask = saved[8].(func(context.Context, uuid.UUID, *model.UserInfo, *dgraphStruct.DgraphProject, *dgraphStruct.DgraphUser, adapter.CreateOrUpdateTaskInput, []*dgraphStruct.DgraphUser) (uuid.UUID, error))
		publishTaskChanged = saved[9].(func(*mqttStruct.MqttTaskChanged))
		untieDocTask = saved[10].(func(context.Context, uuid.UUID, uuid.UUID) error)
	})
	tieDocTask = func(_ context.Context, doc, task, _ uuid.UUID) error {
		w.tiedNew = append(w.tiedNew, doc, task)
		return nil
	}
	docHoldsTask = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return w.tied, nil }
	docsHolding = func(context.Context, uuid.UUID) ([]uuid.UUID, error) { return []uuid.UUID{syncDoc, syncDoc2}, nil }
	setChecks = func(_ context.Context, doc string, c map[string]bool, _ collab.Options) (collab.TaskChecks, error) {
		w.ticks = append(w.ticks, c)
		id := uuid.MustParse(doc)
		if err := w.failures[id]; err != nil {
			return collab.TaskChecks{}, err
		}
		if a, ok := w.answers[id]; ok {
			return a, nil
		}
		return collab.TaskChecks{Changed: 1, Held: 1}, nil
	}
	untieDocTask = func(_ context.Context, doc, task uuid.UUID) error {
		w.untied = append(w.untied, doc, task)
		return nil
	}
	docLevel = func(context.Context, *model.UserInfo, string) (accessBusiness.Level, error) { return w.level, nil }
	// The role read: Work Access (workAccess.Of, OfTask) reads the person's
	// role row and the project's "Who can change tasks" from Postgres, and
	// both read through this handle. Stubbed as no database, as the server
	// stands before it connects, they answer without one: the role is the
	// person's place below, and the policy its default (members change tasks).
	// Left as it was, it is whatever the package's integration tests left
	// behind, their stopped container.
	postgresInit.DBConn = &postgresInit.DB{}
	// The person's place in the project, as the graph counts it: its admin, a
	// member, or outside it.
	place := func() (admin, member uint8) {
		switch {
		case w.outside:
			return 0, 0
		case w.admin:
			return 1, 1
		}
		return 0, 1
	}
	readProject = func(context.Context, string, string) (*dgraphStruct.DgraphProject, error) {
		admin, member := place()
		return &dgraphStruct.DgraphProject{Uuid: syncProj.String(), IsProjectAdmin: admin, IsProjectMember: member}, nil
	}
	readTask = func(context.Context, string, string) (*dgraphStruct.DgraphTask, error) {
		admin, member := place()
		return &dgraphStruct.DgraphTask{Uuid: syncTask.String(), Project: &dgraphStruct.DgraphProject{Uuid: syncProj.String(), IsProjectMember: member, IsProjectAdmin: admin}}, nil
	}
	writeStatus = func(_ context.Context, _ uuid.UUID, status string, _ *dgraphStruct.DgraphTask, _ *dgraphStruct.DgraphUser) error {
		w.written = append(w.written, status)
		return nil
	}
	makeTask = func(_ context.Context, _ uuid.UUID, _ *model.UserInfo, _ *dgraphStruct.DgraphProject, _ *dgraphStruct.DgraphUser, in adapter.CreateOrUpdateTaskInput, _ []*dgraphStruct.DgraphUser) (uuid.UUID, error) {
		w.made = &in
		return syncTask, nil
	}
	publishTaskChanged = func(m *mqttStruct.MqttTaskChanged) { w.published = append(w.published, *m) }
	return w
}

var syncUser = &model.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "5a110000-0000-4000-8000-000000000004"}}

func TestATickInTheDocChangesTheTaskThroughOnePath(t *testing.T) {
	w := newWorld(t)
	if err := SetTodoDone(context.Background(), syncUser, syncDoc.String(), syncTask.String(), true); err != nil {
		t.Fatal(err)
	}
	// A member, who changes tasks since roles (Work Access), ticks it back.
	w.admin = false
	if err := SetTodoDone(context.Background(), syncUser, syncDoc.String(), syncTask.String(), false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w.written, []string{dgraphStruct.TASK_STATUS_DONE, dgraphStruct.TASK_STATUS_TODO}) {
		t.Fatalf("wrote %v", w.written)
	}
}

func TestNobodyChangesATaskFromADocThatTheyCouldNotChangeInItsPanel(t *testing.T) {
	cases := []struct {
		name string
		set  func(*world)
		want error
	}{
		{"a to-do that isn't the task's", func(w *world) { w.tied = false }, ErrTodoNotTied},
		{"a reader of the doc", func(w *world) { w.level = accessBusiness.LevelComment }, ErrDocNotEditable},
		{"someone outside the task's project", func(w *world) { w.outside = true }, ErrTaskNotVisible},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			c.set(w)
			err := SetTodoDone(context.Background(), syncUser, syncDoc.String(), syncTask.String(), true)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if len(w.written) != 0 {
				t.Fatalf("the task was changed: %v", w.written)
			}
		})
	}
}

func TestATodoBecomesATaskTiedToItsDoc(t *testing.T) {
	w := newWorld(t)
	got, err := TodoToTask(context.Background(), syncUser, syncDoc.String(), syncProj.String(), "  Write the launch post ")
	if err != nil || got != syncTask {
		t.Fatalf("got %v %v", got, err)
	}
	// As "Add task" makes it: the to-do's words, medium priority, in To do.
	if w.made == nil || w.made.TaskName != "Write the launch post" || w.made.Priority != dgraphStruct.TASK_PRIORITY_MEDIUM || w.made.Status != "" {
		t.Fatalf("made %+v", w.made)
	}
	if !reflect.DeepEqual(w.tiedNew, []uuid.UUID{syncDoc, syncTask}) {
		t.Fatalf("tied %v", w.tiedNew)
	}
	// A member makes tasks too, since roles.
	member := newWorld(t)
	member.admin = false
	if _, err := TodoToTask(context.Background(), syncUser, syncDoc.String(), syncProj.String(), "Check the copy"); err != nil || member.made == nil {
		t.Fatalf("a member's to-do: %v %+v", err, member.made)
	}

	refused := []struct {
		name  string
		set   func(*world)
		title string
		want  error
	}{
		{"someone outside the project", func(w *world) { w.outside = true }, "Write the launch post", ErrTaskNotVisible},
		{"a reader of the doc", func(w *world) { w.level = accessBusiness.LevelView }, "Write the launch post", ErrDocNotEditable},
		{"an empty to-do", func(*world) {}, "   ", ErrEmptyTodo},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			c.set(w)
			if _, err := TodoToTask(context.Background(), syncUser, syncDoc.String(), syncProj.String(), c.title); !errors.Is(err, c.want) || w.made != nil {
				t.Fatalf("got %v, made %+v", err, w.made)
			}
		})
	}
}

func TestATaskDoneElsewhereTicksItsTodoInEveryDocThatHoldsIt(t *testing.T) {
	w := newWorld(t)
	event := func(from, to string) {
		taskToDocs(context.Background(), "task.status_changed", map[string]interface{}{
			"task_id": syncTask.String(), "project_id": syncProj.String(), "old_status": from, "new_status": to, "updated_by_uuid": syncUser.UserDgraphInfo.Uuid,
		})
	}
	event(dgraphStruct.TASK_STATUS_TODO, dgraphStruct.TASK_STATUS_INPROGRESS) // still not done: no doc is written
	event(dgraphStruct.TASK_STATUS_INPROGRESS, dgraphStruct.TASK_STATUS_DONE)
	event(dgraphStruct.TASK_STATUS_DONE, dgraphStruct.TASK_STATUS_TODO)
	want := []map[string]bool{{syncTask.String(): true}, {syncTask.String(): true}, {syncTask.String(): false}, {syncTask.String(): false}}
	if !reflect.DeepEqual(w.ticks, want) {
		t.Fatalf("ticks %v", w.ticks)
	}
}

// Open chips and task lists hear every change they show, and nothing else.
func TestAChangeAChipShowsIsToldOnTheTasksProject(t *testing.T) {
	w := newWorld(t)
	data := map[string]interface{}{"task_id": syncTask.String(), "project_id": syncProj.String()}
	for _, kind := range []string{"task.created", "task.assigned", "task.deleted", "task.restored", "task.status_changed", "task.comment", "post.created"} {
		taskToDocs(context.Background(), kind, data)
	}
	one := mqttStruct.MqttTaskChanged{TaskUuid: syncTask.String(), ProjectUuid: syncProj.String()}
	if !reflect.DeepEqual(w.published, []mqttStruct.MqttTaskChanged{one, one, one, one, one}) {
		t.Fatalf("published %+v", w.published)
	}
	if len(w.ticks) != 0 {
		t.Fatalf("a change that isn't done or undone ticked a doc: %v", w.ticks)
	}
}

// A rename is no event, so it is told on the project's topic itself: a
// chip showed a renamed task's old name until the page was read again.
func TestARenameIsToldOnTheTasksProject(t *testing.T) {
	w := newWorld(t)
	taskRenamed(syncTask, &dgraphStruct.DgraphProject{Uuid: syncProj.String()})
	taskRenamed(syncTask, nil)
	taskRenamed(syncTask, &dgraphStruct.DgraphProject{})
	want := []mqttStruct.MqttTaskChanged{{TaskUuid: syncTask.String(), ProjectUuid: syncProj.String()}}
	if !reflect.DeepEqual(w.published, want) {
		t.Fatalf("published %+v, want %+v", w.published, want)
	}
}

// A doc follows a task until a change to the task finds it no longer holds
// the to-do (deleted for good, or the doc gone); a save never unties it, so a
// to-do brought back with undo still follows its task.
func TestADocStopsFollowingATaskOnlyOnceItHoldsNoTodoForIt(t *testing.T) {
	gone, trashed := uuid.MustParse("0d0c0000-0000-4000-8000-000000000006"), uuid.MustParse("0d0c0000-0000-4000-8000-000000000007")
	w := newWorld(t)
	docsHolding = func(context.Context, uuid.UUID) ([]uuid.UUID, error) {
		return []uuid.UUID{syncDoc, syncDoc2, gone, trashed}, nil
	}
	w.answers = map[uuid.UUID]collab.TaskChecks{syncDoc2: {Held: 0}}
	w.failures = map[uuid.UUID]error{gone: collab.ErrNoSuchDoc, trashed: collab.ErrDocInTrash}
	taskToDocs(context.Background(), "task.status_changed", map[string]interface{}{
		"task_id": syncTask.String(), "project_id": syncProj.String(), "old_status": dgraphStruct.TASK_STATUS_TODO, "new_status": dgraphStruct.TASK_STATUS_DONE,
	})
	if !reflect.DeepEqual(w.untied, []uuid.UUID{syncDoc2, syncTask, gone, syncTask}) {
		t.Fatalf("untied %v", w.untied)
	}
}
