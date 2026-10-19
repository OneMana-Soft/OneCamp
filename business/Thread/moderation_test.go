package business

import (
	"context"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// asUser is a request from a person, as the sign-in middleware
// leaves it in the context: a workspace admin, or a member.
func asUser(uuid string, admin bool) context.Context {
	return context.WithValue(context.Background(), helpers.UserInfoContextKey, userModels.UserInfo{
		UserPostgresInfo: userModels.User{IsAdmin: admin},
		UserDgraphInfo:   dgraphStruct.DgraphUser{Uuid: uuid},
	})
}

// B7, owner moderation: each person's threads say whether they may remove
// anyone's comment in them, so the web app offers it to them alone.
func TestThreadsSayWhoMayRemoveAnyonesComment(t *testing.T) {
	w := newWorld(t)
	made, err := w.svc.Create(ctx, who("owner"), w.res, rangeAnchor(), Body{HTML: "<p>x</p>"})
	if err != nil || !made.CanModerate {
		t.Fatalf("the owner's own new thread: %+v, %v", made, err)
	}
	agent := who("bob")
	agent.AgentUUID = "agent-1"
	for _, c := range []struct {
		name string
		ctx  context.Context
		who  accessBusiness.Subject
		want bool
	}{
		{"someone with full access", ctx, who("owner"), true},
		{"an editor", ctx, who("editor"), false},
		{"a commenter", ctx, who("ann"), false},
		{"a viewer", ctx, who("viewer"), false},
		{"a workspace admin who can comment", asUser("bob", true), who("bob"), true},
		{"a member, not an admin", asUser("bob", false), who("bob"), false},
		{"someone else's admin request", asUser("sam", true), who("bob"), false},
		{"an agent, whoever asked it", asUser("bob", true), agent, false},
		{"a workspace admin through an API token", helpers.WithActsThroughToken(asUser("bob", true)), who("bob"), false},
	} {
		list, err := w.svc.List(c.ctx, c.who, w.res, Filter{})
		if err != nil || len(list) != 1 {
			t.Fatalf("%s: %d threads, %v", c.name, len(list), err)
		}
		if list[0].CanModerate != c.want {
			t.Errorf("%s: can moderate %v, want %v", c.name, list[0].CanModerate, c.want)
		}
		got, err := w.svc.Get(c.ctx, c.who, made.ID)
		if err != nil || got.CanModerate != c.want {
			t.Errorf("%s, one thread: can moderate %v, %v", c.name, got.CanModerate, err)
		}
	}
	// The owner too, by the decision's own word (A5 says owner apart from level).
	if !mayModerate(ctx, who("x"), accessBusiness.Decision{Owner: true, Level: accessBusiness.LevelComment}) {
		t.Error("the owner may moderate")
	}
	// Nobody, in the trash.
	if mayModerate(asUser("bob", true), who("bob"), accessBusiness.Decision{Owner: true, Level: accessBusiness.LevelFull, Deleted: true}) {
		t.Error("a doc in the trash has no moderators")
	}
}
