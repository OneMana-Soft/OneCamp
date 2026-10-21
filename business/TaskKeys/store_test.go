package business

import (
	"context"
	"errors"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/TaskKey"
	"github.com/google/uuid"
)

func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func TestEnsureSkipsTheKeysOtherProjectsHave(t *testing.T) {
	project := uuid.New()
	var tried []string
	swap(t, &taken, func(_ context.Context, p uuid.UUID, keys []string) (map[string]bool, error) {
		if p != project {
			t.Errorf("asked about project %s", p)
		}
		return map[string]bool{"Q4": true, "QL": true}, nil
	})
	swap(t, &ensureKey, func(_ context.Context, _ uuid.UUID, candidates []string) (string, error) {
		tried = candidates
		return candidates[0], nil
	})
	got, err := Ensure(context.Background(), project, "Q4 launch")
	if err != nil || got == "Q4" || got == "QL" {
		t.Fatalf("Ensure = %q, %v; want a key no other project has", got, err)
	}
	for _, k := range tried {
		if k == "Q4" || k == "QL" {
			t.Errorf("tried %q, which another project has", k)
		}
	}
}

func TestEnsureReadsTheProjectsNameWhenNotGiven(t *testing.T) {
	swap(t, &projectName, func(context.Context, uuid.UUID) (string, error) { return "Customer onboarding", nil })
	swap(t, &taken, func(context.Context, uuid.UUID, []string) (map[string]bool, error) { return nil, nil })
	swap(t, &ensureKey, func(_ context.Context, _ uuid.UUID, c []string) (string, error) { return c[0], nil })
	if got, err := Ensure(context.Background(), uuid.New(), ""); err != nil || got != "CO" {
		t.Errorf("Ensure = %q, %v; want CO", got, err)
	}
}

// Lay sets keys and types on the tasks an answer holds, nested ones too, in
// one read; a task the read knows nothing of keeps none.
func TestLaySetsKeysAndTypesOnEveryTaskInAnAnswer(t *testing.T) {
	parent, task, sub, waits, unknown := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	reads := 0
	swap(t, &factsFor, func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]model.Facts, error) {
		reads++
		if len(ids) != 5 {
			t.Errorf("read %d tasks, want 5", len(ids))
		}
		bug := &model.Type{ID: uuid.New(), Builtin: "bug", Name: "Bug", Icon: "bug"}
		return map[uuid.UUID]model.Facts{
			parent: {Key: "Q4-1"}, task: {Key: "Q4-2", Type: bug}, sub: {Key: "Q4-3"}, waits: {Key: "Q4-4"},
		}, nil
	})
	sl := &dgraphStruct.DgraphTask{Uuid: sub.String(), Key: "STALE-9", WorkType: &dgraphStruct.TaskWorkType{Name: "stale"}}
	t1 := &dgraphStruct.DgraphTask{
		Uuid:       task.String(),
		ParentTask: &dgraphStruct.DgraphTask{Uuid: parent.String()},
		SubTasks:   []*dgraphStruct.DgraphTask{sl},
		BlockedBy:  []*dgraphStruct.DgraphTask{{Uuid: waits.String()}},
	}
	t2 := &dgraphStruct.DgraphTask{Uuid: unknown.String()}
	Lay(context.Background(), t1, t2, nil, &dgraphStruct.DgraphTask{Uuid: "not-a-uuid"})
	if reads != 1 {
		t.Errorf("%d reads, want 1", reads)
	}
	if t1.Key != "Q4-2" || t1.WorkType == nil || t1.WorkType.Builtin != "bug" {
		t.Errorf("the task: %q %+v", t1.Key, t1.WorkType)
	}
	if t1.ParentTask.Key != "Q4-1" || sl.Key != "Q4-3" || t1.BlockedBy[0].Key != "Q4-4" {
		t.Errorf("nested keys: parent %q, sub-task %q, dependency %q", t1.ParentTask.Key, sl.Key, t1.BlockedBy[0].Key)
	}
	if sl.WorkType != nil {
		t.Errorf("the sub-task, a plain Task now, kept its old type %+v", sl.WorkType)
	}
	if t2.Key != "" {
		t.Errorf("a task with no row got %q", t2.Key)
	}
}

func TestLayWithoutKeysWhenTheReadFails(t *testing.T) {
	swap(t, &factsFor, func(context.Context, []uuid.UUID) (map[uuid.UUID]model.Facts, error) {
		return nil, errors.New("down")
	})
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString()}
	Lay(context.Background(), task)
	if task.Key != "" {
		t.Errorf("got %q from a failed read", task.Key)
	}
}

func TestARefIsAnIdOrAKey(t *testing.T) {
	id := uuid.New()
	swap(t, &resolve, func(_ context.Context, k string, n int) (uuid.UUID, error) {
		if k == "Q4" && n == 12 {
			return id, nil
		}
		return uuid.Nil, ErrNoTask
	})
	if got, err := Ref(context.Background(), id.String()); err != nil || got != id {
		t.Errorf("an id: %s, %v", got, err)
	}
	if got, err := Ref(context.Background(), "q4-12"); err != nil || got != id {
		t.Errorf("a key: %s, %v", got, err)
	}
	for _, raw := range []string{"Q4-13", "nothing", ""} {
		if _, err := Ref(context.Background(), raw); !errors.Is(err, ErrNoTask) {
			t.Errorf("Ref(%q) = %v; want ErrNoTask", raw, err)
		}
	}
}

// The sweep keys every project without one and numbers the rest; one that
// fails doesn't stop the others.
func TestTheSweepKeysProjectsAndNumbersTheirTasks(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	swap(t, &projectsWithoutKeys, func(context.Context, int) ([]model.Unkeyed, error) {
		return []model.Unkeyed{{ID: a, Name: "Website"}, {ID: b, Name: "Q4 launch"}}, nil
	})
	swap(t, &taken, func(context.Context, uuid.UUID, []string) (map[string]bool, error) { return nil, nil })
	keyed := map[uuid.UUID]string{}
	swap(t, &ensureKey, func(_ context.Context, p uuid.UUID, cs []string) (string, error) {
		if p == a {
			return "", errors.New("down")
		}
		keyed[p] = cs[0]
		return cs[0], nil
	})
	swap(t, &projectsUnnumbered, func(context.Context, int) ([]uuid.UUID, error) { return []uuid.UUID{b, c}, nil })
	numbered := map[uuid.UUID]bool{}
	swap(t, &numberProject, func(_ context.Context, p uuid.UUID) (int, error) {
		numbered[p] = true
		return 3, nil
	})
	if err := sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if keyed[b] != "Q4" || len(keyed) != 1 {
		t.Errorf("keyed %v; want Q4 launch's Q4 despite Website failing", keyed)
	}
	if !numbered[b] || !numbered[c] {
		t.Errorf("numbered %v; want both", numbered)
	}
}

// WA2: a task the reader can't see is a stub that says only that it is there:
// it is given no key or type, and isn't read for one.
func TestLayLeavesATaskTheReaderCantSeeBare(t *testing.T) {
	task, hidden := uuid.New(), uuid.New()
	swap(t, &factsFor, func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]model.Facts, error) {
		for _, id := range ids {
			if id == hidden {
				t.Error("a stub's key was read")
			}
		}
		return map[uuid.UUID]model.Facts{task: {Key: "Q4-2"}, hidden: {Key: "Q4-9"}}, nil
	})
	stub := &dgraphStruct.DgraphTask{Uuid: hidden.String(), Hidden: true}
	t1 := &dgraphStruct.DgraphTask{Uuid: task.String(), ParentTask: stub, BlockedBy: []*dgraphStruct.DgraphTask{stub}}
	Lay(context.Background(), t1)
	if t1.Key != "Q4-2" || stub.Key != "" || stub.WorkType != nil {
		t.Fatalf("the task %q, the stub %q %+v", t1.Key, stub.Key, stub.WorkType)
	}
}
