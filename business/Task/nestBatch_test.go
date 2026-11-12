package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	domain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A batch nests as the hierarchy lets it (jira-01 J-H, domain/Task
// MayParent): epic -> task -> sub-task, whether the parent is in the batch
// or made before.
func TestABatchNestsAsTheHierarchyLets(t *testing.T) {
	made := map[string]domain.Place{
		"top": {UUID: "top", Project: "p"},
		"sub": {UUID: "sub", Project: "p", Parent: "top"},
	}
	under := func(id string) *dgraphStruct.DgraphTask { return &dgraphStruct.DgraphTask{Uuid: id, Uid: "0x1"} }
	ok := map[string][]NewTask{
		"epic, task, sub-task": {{Parent: -1, Epic: true}, {Parent: 0}, {Parent: 1}},
		"under a top task":     {{Parent: -1, Under: under("top")}},
	}
	refused := map[string][]NewTask{
		"three plain levels":     {{Parent: -1}, {Parent: 0}, {Parent: 1}},
		"an epic under a task":   {{Parent: -1}, {Parent: 0, Epic: true}},
		"an epic under one made": {{Parent: -1, Epic: true, Under: under("top")}},
		"under a sub-task made":  {{Parent: -1, Under: under("sub")}},
		"a sub-task's own task":  {{Parent: -1, Under: under("top")}, {Parent: 0}},
	}
	for name, b := range ok {
		if err := nestBatch("p", made, b); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, b := range refused {
		if err := nestBatch("p", made, b); !errors.Is(err, domain.ErrHierarchy) {
			t.Errorf("%s: %v, want a hierarchy refusal", name, err)
		}
	}
	if err := nestBatch("p", made, []NewTask{{Parent: -1, Under: under("gone")}}); !errors.Is(err, ErrTaskNotVisible) {
		t.Errorf("under a task that isn't there: %v", err)
	}
}

// A batch's epics keep its project within MaxEpics (CR-B36 F2): a batch that
// would take the project past the cap is refused in words; one that fills it
// is made; a batch with no epic reads none.
func TestABatchKeepsTheProjectWithinMaxEpics(t *testing.T) {
	old := epicsIn
	t.Cleanup(func() { epicsIn = old })
	held, reads := 0, 0
	epicsIn = func(_ context.Context, projects ...string) ([]string, error) {
		reads++
		if len(projects) != 1 || projects[0] != "p" {
			t.Errorf("read the epics of %v, want the batch's project", projects)
		}
		return make([]string, held), nil
	}
	twoEpics := []NewTask{{Parent: -1, Epic: true}, {Parent: -1, Epic: true}}
	held = domain.MaxEpics - 1
	if err := mayNest(context.Background(), "p", twoEpics); !errors.Is(err, domain.ErrHierarchy) || !strings.Contains(err.Error(), "at most") {
		t.Errorf("two epics past the cap: %v, want refused in words", err)
	}
	held = domain.MaxEpics - 2
	if err := mayNest(context.Background(), "p", twoEpics); err != nil {
		t.Errorf("two epics that fill the cap: %v", err)
	}
	reads = 0
	if err := mayNest(context.Background(), "p", []NewTask{{Parent: -1}, {Parent: 0}}); err != nil || reads != 0 {
		t.Errorf("a batch with no epic: %v, %d reads of the epics; want none", err, reads)
	}
}
