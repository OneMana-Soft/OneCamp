package business

import (
	"context"
	"errors"
	"testing"

	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
)

const (
	liveTask = "8611f10f-70f7-462e-b966-ab5c89423881"
	goneTask = "f2b3b6ff-0000-4000-8000-000000000001"
)

func withChecks(t *testing.T, checkers map[string]liveness.Checker) *[]*openSearchStruct.GlobalSearchOpenSearchResp {
	t.Helper()
	oldC, oldF := liveCheckers, forgetFn
	var forgotten []*openSearchStruct.GlobalSearchOpenSearchResp
	liveCheckers = checkers
	forgetFn = func(g []*openSearchStruct.GlobalSearchOpenSearchResp) { forgotten = append(forgotten, g...) }
	t.Cleanup(func() { liveCheckers, forgetFn = oldC, oldF })
	return &forgotten
}

func task(id string) *openSearchStruct.GlobalSearchOpenSearchResp {
	return &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.TASK_TYPE, Task: &openSearchStruct.OpenSearchTask{Uuid: id}}
}

// A restore to an earlier point left results for tasks that no longer exist;
// they are dropped and forgotten, and a user (not checked) is kept.
func TestSearchDropsWhatNoLongerExists(t *testing.T) {
	forgotten := withChecks(t, map[string]liveness.Checker{
		"task": func(context.Context, []string) (map[string]bool, error) { return map[string]bool{liveTask: true}, nil },
	})
	user := &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.USER_TYPE, User: &openSearchStruct.OpenSearchUser{}}
	got := keepLiveHits(context.Background(), []*openSearchStruct.GlobalSearchOpenSearchResp{task(goneTask), task(liveTask), user})
	if len(got) != 2 || got[0].Task.Uuid != liveTask || got[1] != user {
		t.Fatalf("kept %v", got)
	}
	if len(*forgotten) != 1 || (*forgotten)[0].Task.Uuid != goneTask {
		t.Fatalf("forgot %v", *forgotten)
	}
}

// "Has more" is what the index said, before a stale hit is dropped; the
// counts lose the dropped hit, so the page never says more than it shows.
func TestAPageThatLosesAHitStillSaysThereIsMore(t *testing.T) {
	withChecks(t, map[string]liveness.Checker{
		"task": func(context.Context, []string) (map[string]bool, error) { return map[string]bool{liveTask: true}, nil },
	})
	saved := searchIndex
	t.Cleanup(func() { searchIndex = saved })
	searchIndex = func(context.Context, []string, []byte) (opensearchModels.SearchAnswer, error) {
		return opensearchModels.SearchAnswer{
			Hits:    []*openSearchStruct.GlobalSearchOpenSearchResp{task(goneTask), task(liveTask)},
			ByIndex: map[string]int{openSearchStruct.TASK_INDEX: 3},
			Total:   3,
		}, nil
	}
	spec := querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeSearch}, Filter: &querySpec.Filter{Field: "text", Operator: querySpec.OperatorContains, Values: []querySpec.Value{{Text: "launch"}}}}
	p, err := Search(context.Background(), Reach{UserUUID: "me"}, spec, Page{Size: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasMore || len(p.Page) != 1 || p.Total != 2 || p.Totals[KindTask] != 2 {
		t.Fatalf("has more %v, page %d, total %d %v; want more, 1, and 2 counted", p.HasMore, len(p.Page), p.Total, p.Totals)
	}
}

func TestSearchKeepsResultsWhenTheCheckCannotRun(t *testing.T) {
	forgotten := withChecks(t, map[string]liveness.Checker{
		"task": func(context.Context, []string) (map[string]bool, error) { return nil, errors.New("database down") },
	})
	if got := keepLiveHits(context.Background(), []*openSearchStruct.GlobalSearchOpenSearchResp{task(goneTask)}); len(got) != 1 {
		t.Fatal("a check that could not run dropped a result")
	}
	if len(*forgotten) != 0 {
		t.Fatal("a check that could not run forgot an entry")
	}
}
