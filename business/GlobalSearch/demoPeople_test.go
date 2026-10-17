package business

import (
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

func TestTheDemoVisitorsSearchFindsOnlyTheDemosPeople(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	hits := []*openSearchStruct.GlobalSearchOpenSearchResp{
		{Type: "user", User: &openSearchStruct.OpenSearchUser{UserName: "owner", UserEmail: "owner@example.com"}},
		{Type: "user", User: &openSearchStruct.OpenSearchUser{UserName: "maya", UserEmail: "maya" + helpers.DemoCastEmailDomain}},
		{Type: "task", Task: &openSearchStruct.OpenSearchTask{Uuid: "t1"}},
	}
	got := demoPeopleHits("visitor@demo.example", hits)
	if len(got) != 2 || got[0].User == nil || got[0].User.UserName != "maya" || got[1].Task == nil {
		t.Errorf("got %+v", got)
	}
	if len(demoPeopleHits("owner@example.com", hits)) != 3 {
		t.Error("a demo member's search lost a person")
	}
}
