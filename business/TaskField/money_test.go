package business

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/TaskField"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// standIn makes projectsWhere answer admin for the projects named, counting
// its reads, for the rest of the test.
func standIn(t *testing.T, admin map[string]bool, err error) *int {
	t.Helper()
	reads := 0
	was := projectsWhere
	projectsWhere = func(_ context.Context, userUUID, userUID string, need workAccess.Need, ids []string) (map[string]bool, error) {
		reads++
		if need != workAccess.NeedManage || (userUUID == "" && userUID == "") {
			t.Errorf("asked %v for %q/%q", need, userUUID, userUID)
		}
		out := map[string]bool{}
		for _, id := range ids {
			if admin[id] {
				out[id] = true
			}
		}
		return out, err
	}
	t.Cleanup(func() { projectsWhere = was })
	return &reads
}

// Money is read and set by its project's admins only: the server reads all
// of it, nobody (a bare context) none, a person what they are an admin of,
// and a failed read none (V-698).
func TestMoneyIsForProjectAdminsOnly(t *testing.T) {
	ctx := context.Background()
	mine, theirs := uuid.NewString(), uuid.NewString()
	reads := standIn(t, map[string]bool{mine: true}, nil)
	fay := Reader{UUID: uuid.NewString(), UID: "0x1"}

	if got, _ := MoneyProjects(ctx, Reader{System: true}, []string{mine, theirs}); !got[mine] || !got[theirs] {
		t.Errorf("the server: %v", got)
	}
	if got, _ := MoneyProjects(ctx, Reader{}, []string{mine}); len(got) != 0 || *reads != 0 {
		t.Errorf("nobody: %v, %d reads", got, *reads)
	}
	if got, _ := MoneyProjects(ctx, fay, []string{mine, theirs}); !got[mine] || got[theirs] || *reads != 1 {
		t.Errorf("fay: %v, %d reads", got, *reads)
	}
	if got, _ := MoneyProjects(ctx, fay, nil); len(got) != 0 || *reads != 1 {
		t.Errorf("no projects asked: %v, %d reads", got, *reads)
	}

	budget := &model.Field{ID: uuid.New(), ProjectID: uuid.MustParse(theirs), Name: "Budget", Type: TypeMoney}
	ours := &model.Field{ID: uuid.New(), ProjectID: uuid.MustParse(mine), Name: "Cost", Type: TypeMoney}
	notes := &model.Field{ID: uuid.New(), ProjectID: uuid.MustParse(theirs), Name: "Notes", Type: TypeText}
	fields := []*model.Field{budget, ours, notes}
	hidden := Hidden(ctx, fay, fields)
	if !hidden[budget.ID.String()] || hidden[ours.ID.String()] || hidden[notes.ID.String()] {
		t.Errorf("hidden from fay: %v", hidden)
	}
	if got := Readable(ctx, fay, fields); len(got) != 2 || got[0] != ours || got[1] != notes {
		t.Errorf("readable by fay: %v", got)
	}
	before := *reads
	if h := Hidden(ctx, fay, []*model.Field{notes}); len(h) != 0 || *reads != before {
		t.Errorf("no money field, no read: %v, %d reads", h, *reads-before)
	}
	views := Views(hidden, fields...)
	if !views[0].Hidden || views[1].Hidden || views[2].Hidden || views[0].FilterID != FilterID(budget.ID) {
		t.Errorf("views: %+v", views)
	}
	b, _ := json.Marshal(views[0])
	var asJSON map[string]any
	_ = json.Unmarshal(b, &asJSON)
	if asJSON["hidden"] != true || asJSON["name"] != "Budget" || asJSON["type"] != "money" {
		t.Errorf("a hidden view as the app gets it: %s", b)
	}

	if err := MaySet(ctx, fay, budget); !errors.Is(err, ErrMoneyAdminsOnly) {
		t.Errorf("fay setting another project's money: %v", err)
	}
	if err := MaySet(ctx, fay, ours); err != nil {
		t.Errorf("fay setting her project's money: %v", err)
	}
	if err := MaySet(ctx, Reader{}, notes); err != nil {
		t.Errorf("anyone setting a text field: %v", err)
	}
	if err := MaySet(ctx, Reader{}, ours); !errors.Is(err, ErrMoneyAdminsOnly) {
		t.Errorf("nobody setting money: %v", err)
	}

	// A read that fails keeps money from everyone.
	standIn(t, map[string]bool{mine: true}, errors.New("graph down"))
	if h := Hidden(ctx, fay, fields); !h[ours.ID.String()] || !h[budget.ID.String()] || h[notes.ID.String()] {
		t.Errorf("hidden when the read fails: %v", h)
	}
	if err := MaySet(ctx, fay, ours); !errors.Is(err, ErrMoneyAdminsOnly) {
		t.Errorf("setting money when the read fails: %v", err)
	}
}

// A message every member hears alike carries no money value and asks each
// app to read the task again; any other field's value is told as it is.
func TestForEveryoneTellsNoMoney(t *testing.T) {
	if v, refetch := ForEveryone(&model.Field{Type: TypeMoney}, json.RawMessage(`125000`)); v != nil || !refetch {
		t.Errorf("money: %s, %v", v, refetch)
	}
	if v, refetch := ForEveryone(&model.Field{Type: TypeText}, json.RawMessage(`"hi"`)); string(v) != `"hi"` || refetch {
		t.Errorf("text: %s, %v", v, refetch)
	}
}

// blank takes hidden fields' values off every task, and nothing else.
func TestBlankTakesOffOnlyHiddenValues(t *testing.T) {
	values := model.Values{
		"t1": {"budget": json.RawMessage(`1`), "notes": json.RawMessage(`"a"`)},
		"t2": {"budget": json.RawMessage(`2`)},
	}
	blank(values, map[string]bool{"budget": true})
	if len(values["t1"]) != 1 || string(values["t1"]["notes"]) != `"a"` || len(values["t2"]) != 0 {
		t.Errorf("blanked: %v", values)
	}
	blank(values, nil)
	if len(values["t1"]) != 1 {
		t.Errorf("blank with nothing hidden: %v", values)
	}
}

// ReaderOf names the server for a system context, the signed-in person (with
// their graph uid) for a request's, and nobody for a bare one.
func TestReaderOfAContext(t *testing.T) {
	bg := context.Background()
	if r := ReaderOf(bg); r != (Reader{}) {
		t.Errorf("a bare context: %+v", r)
	}
	if r := ReaderOf(taskread.WithSystem(bg)); !r.System {
		t.Errorf("a system context: %+v", r)
	}
	id := uuid.New()
	u := userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id}}
	u.UserDgraphInfo.Uid, u.UserDgraphInfo.Uuid = "0x2a", id.String()
	for _, v := range []any{u, &u} {
		if r := ReaderOf(context.WithValue(bg, helpers.UserInfoContextKey, v)); r.UUID != id.String() || r.UID != "0x2a" || r.System {
			t.Errorf("a request's context (%T): %+v", v, r)
		}
	}
	if r := ReaderOf(taskread.AsNobody(context.WithValue(bg, helpers.UserInfoContextKey, u))); r != (Reader{}) {
		t.Errorf("a context read as nobody: %+v", r)
	}
}
