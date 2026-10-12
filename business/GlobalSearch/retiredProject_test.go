package business

import (
	"context"
	"testing"

	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// On the demo, searching "launch" found "Q4 launch-retired-1791614951", an
// archived copy whose search entry had been written back to live. Search now
// checks a project where it lives, as it checks tasks and docs: an archived
// one is dropped and its entry kept (it can be unarchived); a project that is
// gone is dropped and its entry forgotten.
func TestSearchDropsAnArchivedProject(t *testing.T) {
	const live, archived, gone = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	if _, ok := liveness.Checkers[openSearchStruct.PROJECT_TYPE]; !ok {
		t.Fatal("search does not check whether a project is archived")
	}
	forgotten := withChecks(t, map[string]liveness.Checker{
		openSearchStruct.PROJECT_TYPE: func(context.Context, []string) (map[string]bool, error) {
			return map[string]bool{live: true, archived: false}, nil
		},
	})
	project := func(id, name string) *openSearchStruct.GlobalSearchOpenSearchResp {
		return &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.PROJECT_TYPE,
			Project: &openSearchStruct.OpenSearchProject{Uuid: id, ProjectName: name}}
	}
	got := keepLiveHits(context.Background(), []*openSearchStruct.GlobalSearchOpenSearchResp{
		project(live, "Q4 launch"), project(archived, "Q4 launch-retired-1791614951"), project(gone, "Q3 launch"),
	})
	if len(got) != 1 || got[0].Project.Uuid != live {
		t.Fatalf("kept %d projects: %+v", len(got), got)
	}
	if len(*forgotten) != 1 || (*forgotten)[0].Project.Uuid != gone {
		t.Fatalf("forgot %v", *forgotten)
	}
	if hitIndex[openSearchStruct.PROJECT_TYPE] != openSearchStruct.PROJECT_INDEX {
		t.Error("a gone project's entry would not be forgotten from the projects index")
	}
}
