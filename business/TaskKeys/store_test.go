package business

import (
	"context"
	"errors"
	"reflect"
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

// O-L2: a task imported from a source whose keys belong elsewhere (Linear's
// teams) still answers to the key it had there, of any length the source
// allows: ENGINE-7 or E-7 find it as Q4-12 does, through the same read.
func TestAKeyATaskHadInItsSourceFindsIt(t *testing.T) {
	id := uuid.New()
	asked := []string{}
	swap(t, &resolve, func(_ context.Context, k string, n int) (uuid.UUID, error) {
		asked = append(asked, k)
		if (k == "ENGINE" || k == "E") && n == 7 {
			return id, nil
		}
		return uuid.Nil, ErrNoTask
	})
	for _, raw := range []string{"ENGINE-7", "engine-7", " E-7 "} {
		if got, err := Resolve(context.Background(), raw); err != nil || got != id {
			t.Errorf("Resolve(%q) = %s, %v; want the task", raw, got, err)
		}
	}
	for _, raw := range []string{"ENGINEERING1-7", "7-7", "-7", "E-07", "E-", "E 7", "ÉNG-7"} {
		if _, err := Resolve(context.Background(), raw); !errors.Is(err, ErrNoTask) {
			t.Errorf("Resolve(%q) = %v; want ErrNoTask", raw, err)
		}
	}
	if len(asked) != 3 {
		t.Errorf("the read was asked %d times (%v); want once for each key", len(asked), asked)
	}
}

func TestASourceKeyIsOneToTenCapitalsAndDigitsStartingWithALetter(t *testing.T) {
	for raw, want := range map[string]string{"ENG": "ENG", "e": "E", "Q4": "Q4", "ENGINEERIN": "ENGINEERIN"} {
		if got, ok := SourceKey(raw); !ok || got != want {
			t.Errorf("SourceKey(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "4Q", "ENGINEERING", "EN-G", "EN G", "ÉNG"} {
		if got, ok := SourceKey(raw); ok {
			t.Errorf("SourceKey(%q) = %q; want refused", raw, got)
		}
	}
}

// An answer names a task by its key and quoted name, as Linear names an
// issue, and by the name alone when its project has no key or the key can't
// be read.
func TestAnAnswerNamesATaskByItsKey(t *testing.T) {
	keyed, bare := uuid.New(), uuid.New()
	swap(t, &factsFor, func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]model.Facts, error) {
		out := map[uuid.UUID]model.Facts{}
		for _, id := range ids {
			if id == keyed {
				out[id] = model.Facts{Key: "Q4-12"}
			}
		}
		return out, nil
	})
	ctx := context.Background()
	if got := Named(KeyOf(ctx, keyed), `"Write the launch post"`); got != `Q4-12 "Write the launch post"` {
		t.Errorf("a keyed task is named %s", got)
	}
	if got := Named(KeyOf(ctx, bare), `"Draft the FAQ"`); got != `"Draft the FAQ"` {
		t.Errorf("a task without a key is named %s", got)
	}
	swap(t, &factsFor, func(context.Context, []uuid.UUID) (map[uuid.UUID]model.Facts, error) {
		return nil, errors.New("down")
	})
	if got := KeyOf(ctx, keyed); got != "" {
		t.Errorf("a failed read gave the key %q", got)
	}
}

// A notice names its task by key then name (Q4-12 Fix the login), in one
// read for a page of them; a name its reader can't see stays empty, and a
// failed read leaves every name as it was.
func TestANoticeNamesItsTaskByKey(t *testing.T) {
	keyed, bare, hidden := uuid.New(), uuid.New(), uuid.New()
	reads := 0
	swap(t, &factsFor, func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]model.Facts, error) {
		reads++
		out := map[uuid.UUID]model.Facts{}
		for _, id := range ids {
			if id == hidden {
				t.Errorf("read the key of a task its reader can't see")
			}
			if id == keyed || id == hidden {
				out[id] = model.Facts{Key: "Q4-12"}
			}
		}
		return out, nil
	})
	ctx := context.Background()
	names := map[string]string{keyed.String(): "Fix the login", bare.String(): "Draft the FAQ", hidden.String(): "", "not-an-id": "Odd"}
	got := NamedAll(ctx, names)
	want := map[string]string{keyed.String(): "Q4-12 Fix the login", bare.String(): "Draft the FAQ", hidden.String(): "", "not-an-id": "Odd"}
	if !reflect.DeepEqual(got, want) || reads != 1 {
		t.Errorf("named %v in %d reads, want %v in one", got, reads, want)
	}
	if one := NamedTask(ctx, keyed.String(), "Fix the login"); one != "Q4-12 Fix the login" {
		t.Errorf("one task named %q", one)
	}
	swap(t, &factsFor, func(context.Context, []uuid.UUID) (map[uuid.UUID]model.Facts, error) {
		return nil, errors.New("down")
	})
	if got := NamedAll(ctx, names); !reflect.DeepEqual(got, names) {
		t.Errorf("a failed read named them %v", got)
	}
}

// A list of projects carries each one's key, read once for the lot (a new
// task's address names its project by key); a project without one, and
// every project when the read fails, carries none.
func TestAProjectListCarriesEachProjectsKey(t *testing.T) {
	launch, quiet := uuid.New(), uuid.New()
	reads := 0
	swap(t, &projectKeys, func(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
		reads++
		return map[uuid.UUID]string{launch: "Q4"}, nil
	})
	list := []*dgraphStruct.DgraphProject{{Uuid: launch.String()}, {Uuid: quiet.String()}, nil}
	LayProjects(context.Background(), list...)
	if reads != 1 || list[0].Key != "Q4" || list[1].Key != "" {
		t.Errorf("%d reads; keys %q and %q", reads, list[0].Key, list[1].Key)
	}
	swap(t, &projectKeys, func(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
		return nil, errors.New("down")
	})
	again := &dgraphStruct.DgraphProject{Uuid: launch.String()}
	LayProjects(context.Background(), again)
	if again.Key != "" {
		t.Errorf("a failed read gave the key %q", again.Key)
	}
}
