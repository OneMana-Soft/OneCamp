package business

import (
	"context"
	"strings"
	"testing"

	whoKnowsBusiness "github.com/akashc777/OneCamp/business/WhoKnows"
	whoKnowsDomain "github.com/akashc777/OneCamp/domain/WhoKnows"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func whoKnowsCaller() *userModels.UserInfo {
	var u userModels.UserInfo
	u.UserPostgresInfo.IsAdmin = true
	u.UserPostgresInfo.EmailID = "admin@example.test"
	u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "00000000-0000-0000-0000-0000000000a1",
		Channels: []*dgraphStruct.DgraphChannel{{Uuid: "c1"}}, Projects: []*dgraphStruct.DgraphProject{{Uuid: "p1"}}}
	return &u
}

// find_people's topic asks the shared Who knows as the caller, a member at
// most through a token, with the kNN seeds added; it cites each item by id.
func TestFindPeopleTopicAsksWhoKnowsAsAMember(t *testing.T) {
	var got whoKnowsBusiness.Asker
	var sources int
	old := askWhoKnows
	t.Cleanup(func() { askWhoKnows = old })
	askWhoKnows = func(_ context.Context, a whoKnowsBusiness.Asker, text string, _ int, more ...whoKnowsBusiness.SeedSource) (*whoKnowsBusiness.Answer, error) {
		got, sources = a, len(more)
		return &whoKnowsBusiness.Answer{Query: text, People: []whoKnowsBusiness.Person{{ID: "u1", Name: "Cy", Wrote: 1, Created: 1, Score: 6,
			Evidence: []whoKnowsBusiness.Evidence{{Kind: "doc", ID: "d1", Title: "Budget plan", Role: "wrote"}}}}}, nil
	}
	text, err := whoKnowsText(helpers.WithActsThroughToken(context.Background()), whoKnowsCaller(), "the budget")
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject.Admin {
		t.Error("through a token the admin asked as an admin")
	}
	if got.Subject.UserUUID != whoKnowsCaller().UserDgraphInfo.Uuid || strings.Join(got.Channels, ",") != "c1" || strings.Join(got.Projects, ",") != "p1" {
		t.Errorf("asker %+v", got)
	}
	if sources != 1 {
		t.Errorf("%d extra seed sources, want the kNN one", sources)
	}
	for _, want := range []string{"Cy [u1]: wrote 1 docs, created 1 tasks", `wrote doc "Budget plan" [d1]`} {
		if !strings.Contains(text, want) {
			t.Errorf("answer lacks %q:\n%s", want, text)
		}
	}
	// In the app, the admin's own subject keeps its flag (Who knows never reads it).
	_, _ = whoKnowsText(context.Background(), whoKnowsCaller(), "the budget")
	if !got.Subject.Admin {
		t.Error("the in-app subject lost its flag; the token check above proves nothing")
	}
}

// A kNN hit is the item it stands for: a comment its parent, never a chat.
func TestWhoKnowsKNNHitsAreItems(t *testing.T) {
	cases := []struct {
		hit  ai.SimilarResult
		want whoKnowsDomain.Seed
		ok   bool
	}{
		{ai.SimilarResult{ContentType: "doc", ContentUUID: "d"}, whoKnowsDomain.Seed{Kind: "doc", ID: "d"}, true},
		{ai.SimilarResult{ContentType: "task", ContentUUID: "t"}, whoKnowsDomain.Seed{Kind: "task", ID: "t"}, true},
		{ai.SimilarResult{ContentType: "post", ContentUUID: "p"}, whoKnowsDomain.Seed{Kind: "message", ID: "p"}, true},
		{ai.SimilarResult{ContentType: "comment", ContentUUID: "c", TaskUUID: "t"}, whoKnowsDomain.Seed{Kind: "task", ID: "t"}, true},
		{ai.SimilarResult{ContentType: "comment", ContentUUID: "c", PostUUID: "p"}, whoKnowsDomain.Seed{Kind: "message", ID: "p"}, true},
		{ai.SimilarResult{ContentType: "chat", ContentUUID: "x"}, whoKnowsDomain.Seed{}, false},
		{ai.SimilarResult{ContentType: "comment", ContentUUID: "c", BoardUUID: "b"}, whoKnowsDomain.Seed{}, false},
	}
	for _, c := range cases {
		got, ok := seedOfHit(c.hit)
		if ok != c.ok || got != c.want {
			t.Errorf("%+v: %+v %v, want %+v %v", c.hit, got, ok, c.want, c.ok)
		}
	}
}

// No topic and no query is refused before anything is read.
func TestFindPeopleNeedsAQueryOrATopic(t *testing.T) {
	if _, _, err := executeFindPeople(context.Background(), ai.ProposedAction{Params: map[string]string{}}, "x"); err == nil || !strings.Contains(err.Error(), "topic") {
		t.Errorf("%v", err)
	}
}
