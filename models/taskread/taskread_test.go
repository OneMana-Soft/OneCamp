package taskread

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const (
	secret = "7a5c00a8-0000-4000-8000-000000000001"
	open   = "7a5c00a8-0000-4000-8000-000000000002"
	other  = "7a5c00a8-0000-4000-8000-000000000003"
)

func task(id, name string) *dgraphStruct.DgraphTask {
	return &dgraphStruct.DgraphTask{Uuid: id, Name: name}
}

func withDecide(t *testing.T, d func(ctx context.Context, reader string) (map[string]bool, error)) {
	t.Helper()
	was := Decide
	Decide = d
	t.Cleanup(func() { Decide = was })
}

func TestTheEmptyFilterChangesNothing(t *testing.T) {
	var f Filter
	if !f.Empty() || f.Hides(secret) || f.Cond() != "" || f.Directive() != "" || f.AndCond("has(task_name)") != "has(task_name)" {
		t.Fatalf("an empty filter hid something: %+v", f)
	}
	p := &dgraphStruct.DgraphProject{Tasks: []*dgraphStruct.DgraphTask{task(secret, "Payroll")}}
	f.Keep(p)
	if len(p.Tasks) != 1 {
		t.Fatal("an empty filter took a task out")
	}
}

func TestAQueryLeavesTheHiddenOutOfListsAndCounts(t *testing.T) {
	f := Of(map[string]bool{strings.ToUpper(secret): true, "not-a-uuid": true, other: false})
	if f.Cond() != `NOT eq(task_uuid, ["`+secret+`"])` {
		t.Fatalf("cond: %s", f.Cond())
	}
	if got := f.AndCond(`not gt(task_deleted_at, "1970-01-01T00:00:00Z")`); got != `(not gt(task_deleted_at, "1970-01-01T00:00:00Z")) AND NOT eq(task_uuid, ["`+secret+`"])` {
		t.Fatalf("and: %s", got)
	}
	if f.AndCond("") != f.Cond() || f.Directive() != "@filter("+f.Cond()+")" {
		t.Fatal("cond alone, or as a directive")
	}
	if !f.Hides(secret) || f.Hides(open) || f.Hides(other) {
		t.Fatal("hides by id, in any case, only what it was given")
	}
	if ids := f.IDs(); len(ids) != 1 || ids[0] != secret {
		t.Fatalf("only uuids name tasks: %v", ids)
	}
}

func TestAnAnswerKeepsOnlyWhatTheReaderMaySee(t *testing.T) {
	f := Of(map[string]bool{secret: true})
	parent := task(secret, "Payroll")
	visible := task(open, "Launch post")
	visible.ParentTask = parent
	visible.SubTasks = []*dgraphStruct.DgraphTask{task(secret, "Payroll"), task(other, "Copy")}
	blocker := task(secret, "Payroll")
	blocker.DependencyKind = "ss"
	visible.BlockedBy = []*dgraphStruct.DgraphTask{blocker}
	visible.Comments = []*dgraphStruct.DgraphComment{{Uuid: "c1", Task: task(secret, "Payroll")}}
	p := &dgraphStruct.DgraphProject{
		Tasks:     []*dgraphStruct.DgraphTask{task(secret, "Payroll"), visible},
		TasksTodo: []*dgraphStruct.DgraphTask{task(secret, "Payroll")},
	}
	f.Keep(p)
	if len(p.Tasks) != 1 || p.Tasks[0].Uuid != open || len(p.TasksTodo) != 0 {
		t.Fatalf("the project's lists keep the hidden task: %+v", p.Tasks)
	}
	got := p.Tasks[0]
	if got.ParentTask == nil || !got.ParentTask.Hidden || got.ParentTask.Name != "" || got.ParentTask.Uuid != secret {
		t.Fatalf("a hidden parent is a stub, its id and nothing more: %+v", got.ParentTask)
	}
	if len(got.SubTasks) != 1 || got.SubTasks[0].Uuid != other {
		t.Fatalf("a hidden sub-task leaves the list: %+v", got.SubTasks)
	}
	if len(got.BlockedBy) != 1 || !got.BlockedBy[0].Hidden || got.BlockedBy[0].Name != "" || got.BlockedBy[0].DependencyKind != "ss" {
		t.Fatalf("a hidden dependency stays as a stub, with the link's kind: %+v", got.BlockedBy)
	}
	if c := got.Comments[0].Task; c == nil || !c.Hidden || c.Name != "" {
		t.Fatalf("a comment's hidden task is a stub: %+v", c)
	}
}

func TestAListReadOnItsOwnLosesItsHiddenTasks(t *testing.T) {
	f := Of(map[string]bool{secret: true})
	list := []*dgraphStruct.DgraphTask{task(secret, "Payroll"), task(open, "Launch post"), nil}
	if got := f.KeepTasks(list); len(got) != 1 || got[0].Uuid != open {
		t.Fatalf("keep tasks: %+v", got)
	}
	list = []*dgraphStruct.DgraphTask{task(secret, "Payroll"), task(open, "Launch post")}
	f.Keep(&list)
	if len(list) != 1 || list[0].Uuid != open {
		t.Fatalf("keep through a pointer to the list: %+v", list)
	}
	values := struct {
		TaskInfo []dgraphStruct.DgraphTask `json:"taskInfo"`
	}{TaskInfo: []dgraphStruct.DgraphTask{*task(secret, "Payroll"), *task(open, "Launch post")}}
	f.Keep(&values)
	if len(values.TaskInfo) != 1 || values.TaskInfo[0].Uuid != open {
		t.Fatalf("a list of task values: %+v", values.TaskInfo)
	}
}

func TestEachRequestWorksOutItsReadersFilterOnce(t *testing.T) {
	calls := map[string]int{}
	withDecide(t, func(_ context.Context, reader string) (map[string]bool, error) {
		calls[reader]++
		if reader == "" {
			// Nobody: every restricted task.
			return map[string]bool{secret: true, other: true}, nil
		}
		return map[string]bool{secret: true}, nil
	})
	me := uuid.MustParse("5a5c00a8-0000-4000-8000-0000000000aa")
	user := userModels.UserInfo{}
	user.UserPostgresInfo.Id = me
	ctx := Install(context.WithValue(context.Background(), helpers.UserInfoContextKey, user))
	for i := 0; i < 3; i++ {
		f, err := From(ctx)
		if err != nil || !f.Hides(secret) || f.Hides(other) {
			t.Fatalf("the reader's filter: %+v %v", f, err)
		}
	}
	if calls[me.String()] != 1 {
		t.Fatalf("decided %d times in one request", calls[me.String()])
	}

	// A read that can't say whose it is hides every restricted task.
	nobody, err := From(context.Background())
	if err != nil || !nobody.Hides(secret) || !nobody.Hides(other) {
		t.Fatalf("no reader fails closed: %+v %v", nobody, err)
	}
	// A read with no audience hides nothing.
	if sys, _ := From(WithSystem(context.Background())); !sys.Empty() {
		t.Fatal("a system read hid something")
	}
	// One recipient's view, whoever is reading.
	if f, _ := For(ctx, other); !f.Hides(secret) || calls[other] != 1 {
		t.Fatal("For decides for its recipient")
	}
}

// Until something decides (a program or a test that doesn't link
// business/WorkAccess), every restricted task is hidden from everyone, and a
// failed read of them is the read's error: never a sight of one.
func TestNothingDecidedHidesEveryRestrictedTask(t *testing.T) {
	withDecide(t, nil)
	was := everyRestricted
	t.Cleanup(func() { everyRestricted = was })
	everyRestricted = func(context.Context) (map[string]bool, error) { return map[string]bool{secret: true}, nil }
	admin := userModels.UserInfo{}
	admin.UserPostgresInfo.Id = uuid.MustParse("5a5c00a8-0000-4000-8000-0000000000cc")
	ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, admin)
	if f, err := From(ctx); err != nil || !f.Hides(secret) || f.Hides(open) {
		t.Fatalf("from with nothing deciding: %v %v", f.IDs(), err)
	}
	if f, err := For(ctx, admin.UserPostgresInfo.Id.String()); err != nil || !f.Hides(secret) {
		t.Fatalf("for with nothing deciding: %v %v", f.IDs(), err)
	}
	down := errors.New("postgres down")
	everyRestricted = func(context.Context) (map[string]bool, error) { return nil, down }
	if _, err := From(ctx); !errors.Is(err, down) {
		t.Fatalf("a failed read of what is restricted: %v", err)
	}
	boom := errors.New("postgres down")
	withDecide(t, func(context.Context, string) (map[string]bool, error) { return nil, boom })
	if _, err := From(Install(context.Background())); !errors.Is(err, boom) {
		t.Fatalf("a failed decision is the read's error: %v", err)
	}
}

func TestAQueryWithRoomForTheFilterIsFilledEverywhere(t *testing.T) {
	q := `{ p { open: count(project_tasks @filter(has(task_uuid) ` + Mark + `)) project_tasks @filter(has(task_uuid) ` + Mark + `) { task_name } } }`
	if got := (Filter{}).Fill(q); strings.Contains(got, Mark) || strings.Contains(got, "NOT eq") {
		t.Fatalf("filled with nothing hidden: %s", got)
	}
	got := Of(map[string]bool{secret: true}).Fill(q)
	if strings.Contains(got, Mark) || strings.Count(got, `AND NOT eq(task_uuid, ["`+secret+`"])`) != 2 {
		t.Fatalf("both blocks leave the hidden task out: %s", got)
	}
	if Of(map[string]bool{secret: true}).In() != `eq(task_uuid, ["`+secret+`"])` || (Filter{}).In() != "" {
		t.Fatal("In finds the hidden tasks")
	}
}

func TestASearchLeavesTheHiddenOutOfItsCountsAndPages(t *testing.T) {
	body := `{"size":20,"sort":[{"created_date":"desc"}],"query":{"bool":{"should":[{"term":{"_index":"tasks"}}]}},"highlight":{"fields":{"task_name":{}}}}`
	same, err := (Filter{}).SearchWithout(body, "task_id")
	if err != nil || same != body {
		t.Fatalf("nothing hidden leaves the body as it was: %v", err)
	}
	got, err := Of(map[string]bool{secret: true}).SearchWithout(body, "task_id", "comment_task_id")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Size  int `json:"size"`
		Query struct {
			Bool struct {
				Must    []json.RawMessage           `json:"must"`
				MustNot []map[string]map[string]any `json:"must_not"`
			} `json:"bool"`
		} `json:"query"`
		Sort      []any `json:"sort"`
		Highlight any   `json:"highlight"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Size != 20 || len(parsed.Sort) != 1 || parsed.Highlight == nil {
		t.Fatalf("the body's other parts are kept: %s", got)
	}
	if len(parsed.Query.Bool.Must) != 1 || !strings.Contains(string(parsed.Query.Bool.Must[0]), `"should"`) {
		t.Fatalf("the query is kept as the must: %s", got)
	}
	if len(parsed.Query.Bool.MustNot) != 2 || parsed.Query.Bool.MustNot[1]["terms"]["comment_task_id"] == nil {
		t.Fatalf("each field names the hidden tasks: %s", got)
	}
}

func TestWhatAnyoneWillReadIsGatheredAsNobody(t *testing.T) {
	withDecide(t, func(_ context.Context, reader string) (map[string]bool, error) {
		if reader == "" {
			return map[string]bool{secret: true, other: true}, nil
		}
		return map[string]bool{}, nil
	})
	admin := userModels.UserInfo{}
	admin.UserPostgresInfo.Id = uuid.MustParse("5a5c00a8-0000-4000-8000-0000000000cc")
	ctx := Install(context.WithValue(context.Background(), helpers.UserInfoContextKey, admin))
	if f, _ := From(ctx); !f.Empty() {
		t.Fatal("the admin sees every task")
	}
	if f, _ := From(AsNobody(ctx)); !f.Hides(secret) || !f.Hides(other) {
		t.Fatal("what an update gathers, it gathers as nobody")
	}
	if f, _ := Nobody(ctx); !f.Hides(secret) {
		t.Fatal("Nobody")
	}
}

// An assistant's tool, or an agent's step, reads as the person it acts for,
// whoever is signed in where it runs; what anyone will read is still gathered
// as nobody.
func TestAToolReadsAsThePersonItActsFor(t *testing.T) {
	asker, signedIn := "5a5c00a8-0000-4000-8000-0000000000dd", uuid.MustParse("5a5c00a8-0000-4000-8000-0000000000cc")
	withDecide(t, func(_ context.Context, reader string) (map[string]bool, error) {
		if reader == asker {
			return map[string]bool{secret: true}, nil
		}
		return map[string]bool{}, nil
	})
	admin := userModels.UserInfo{}
	admin.UserPostgresInfo.Id = signedIn
	ctx := Install(context.WithValue(context.Background(), helpers.UserInfoContextKey, admin))
	acting := ActingFor(ctx, " "+strings.ToUpper(asker)+" ")
	if got := ReaderOf(acting); got != asker {
		t.Fatalf("reads as %q, want the person it acts for", got)
	}
	if f, _ := From(acting); !f.Hides(secret) {
		t.Fatal("the asker's tool sees what the asker may not")
	}
	if f, _ := From(ctx); !f.Empty() {
		t.Fatal("the signed-in person's own reads changed")
	}
	if ReaderOf(AsNobody(acting)) != "" || ReaderOf(ActingFor(ctx, "")) != "" {
		t.Fatal("nobody is nobody, acting for anyone or no one")
	}
	// Acting as a person reads as them, known as a signed-in person is.
	profileOnly := userModels.UserInfo{}
	profileOnly.UserDgraphInfo.Uuid = strings.ToUpper(asker)
	if got := ReaderOf(ActingAs(acting, admin)); got != signedIn.String() {
		t.Fatalf("acting as the signed-in person reads as %q", got)
	}
	if got := ReaderOf(ActingAs(ctx, profileOnly)); got != asker {
		t.Fatalf("acting as someone known by their profile reads as %q", got)
	}
}

// A restricted task the reader may see carries the lock (task_restricted,
// WA2b), wherever it sits in an answer: a list, a sub-task, a parent, a
// dependency, a list of task values, a list read on its own. Nothing else
// does, and what is hidden stays hidden.
func TestARestrictedTaskTheReaderMaySeeCarriesTheLock(t *testing.T) {
	f := Of(map[string]bool{secret: false, other: true})
	if f.Hides(secret) || f.Cond() != `NOT eq(task_uuid, ["`+other+`"])` || !Of(map[string]bool{secret: false}).Empty() {
		t.Fatalf("a restricted task the reader may see is no hidden one: %+v", f)
	}
	if !f.Restricted(strings.ToUpper(secret)) || f.Restricted(open) || f.Restricted(other) {
		t.Fatal("restricted: only the one the reader may see, in any case")
	}
	sub := task(secret, "Payroll")
	plain := task(open, "Launch post")
	plain.SubTasks = []*dgraphStruct.DgraphTask{sub, task(other, "Bonus")}
	plain.ParentTask = task(secret, "Payroll")
	plain.BlockedBy = []*dgraphStruct.DgraphTask{task(secret, "Payroll")}
	p := &dgraphStruct.DgraphProject{Tasks: []*dgraphStruct.DgraphTask{task(secret, "Payroll"), plain}}
	f.Keep(p)
	if !p.Tasks[0].Restricted || p.Tasks[1].Restricted {
		t.Fatalf("the list's tasks: %+v %+v", p.Tasks[0], p.Tasks[1])
	}
	if len(plain.SubTasks) != 1 || !plain.SubTasks[0].Restricted || !plain.ParentTask.Restricted || !plain.BlockedBy[0].Restricted {
		t.Fatalf("a sub-task, a parent and a dependency carry it: %+v", plain)
	}
	if b, _ := json.Marshal(p.Tasks[0]); !strings.Contains(string(b), `"task_restricted":true`) {
		t.Fatalf("the app reads task_restricted: %s", b)
	}
	if b, _ := json.Marshal(plain.SubTasks[0].ParentTask); strings.Contains(string(b), "task_restricted") {
		t.Fatalf("an open task says nothing: %s", b)
	}

	values := struct {
		TaskInfo []dgraphStruct.DgraphTask `json:"taskInfo"`
	}{TaskInfo: []dgraphStruct.DgraphTask{*task(secret, "Payroll")}}
	f.Keep(&values)
	if !values.TaskInfo[0].Restricted {
		t.Fatal("a list of task values")
	}
	got := f.KeepTasks([]*dgraphStruct.DgraphTask{task(secret, "Payroll"), task(other, "Bonus")})
	if len(got) != 1 || !got[0].Restricted {
		t.Fatalf("a list read on its own: %+v", got)
	}
	one := task(secret, "Payroll")
	f.Keep(one)
	if !one.Restricted {
		t.Fatal("a task read on its own")
	}
}
