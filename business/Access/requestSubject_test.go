package business

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// The person a request is from is the one the auth middleware put on it, as a
// value or a pointer; with nobody there, no one (and no panic).
func TestRequestSubjectIsTheSignedInMember(t *testing.T) {
	var u userModels.UserInfo
	u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "7d1e5c2a-9b3f-4a6e-8c1d-2f4b6a8e0c3d"}
	for _, v := range []any{u, &u} {
		r := httptest.NewRequest("GET", "/", nil).WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, v))
		got, who := RequestSubject(r)
		if who != (Subject{UserUID: "0x1", UserUUID: u.UserDgraphInfo.Uuid}) || got.UserDgraphInfo.Uuid != u.UserDgraphInfo.Uuid {
			t.Errorf("%T: %+v %+v", v, who, got)
		}
	}
	if _, who := RequestSubject(httptest.NewRequest("GET", "/", nil)); who != (Subject{}) {
		t.Errorf("nobody signed in: %+v", who)
	}
}

// A workspace admin's request says so, and being one counts only while they
// act themselves: the same person through an agent is no admin.
func TestAnAdminActsAsOneOnlyThemselves(t *testing.T) {
	var u userModels.UserInfo
	u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "3c9b2f4e-1a7d-4e8b-9c6f-5d2a8b1e7f40"}
	u.UserPostgresInfo.IsAdmin = true
	r := httptest.NewRequest("GET", "/", nil).WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, u))
	_, who := RequestSubject(r)
	if !who.Admin || !who.ActingAdmin() {
		t.Fatalf("an admin's own request: %+v", who)
	}
	who.AgentUUID = "8f1d2c3b-4a5e-4f60-8b7a-9c0d1e2f3a4b"
	if who.ActingAdmin() {
		t.Error("an admin's agent acts as an admin")
	}
	if (Subject{UserUUID: u.UserDgraphInfo.Uuid}).ActingAdmin() {
		t.Error("a member acts as an admin")
	}
}

// An account a caller holds, rather than a request, names its person by the
// same rule, admin included: an agent acting for whoever asked it starts from
// it and adds itself (and so is no admin).
func TestSubjectOfIsRequestSubjectsRule(t *testing.T) {
	if SubjectOf(nil) != (Subject{}) {
		t.Fatal("no account is someone")
	}
	var u userModels.UserInfo
	u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0x3", Uuid: "5b0c7e1a-2d4f-4c8e-9a6b-3f1d8e2c7a50"}
	u.UserPostgresInfo.IsAdmin = true
	want := Subject{UserUID: "0x3", UserUUID: u.UserDgraphInfo.Uuid, Admin: true}
	if got := SubjectOf(&u); got != want {
		t.Fatalf("SubjectOf = %+v, want %+v", got, want)
	}
	r := httptest.NewRequest("GET", "/", nil).WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, u))
	if _, who := RequestSubject(r); who != want {
		t.Fatalf("RequestSubject = %+v, want SubjectOf's %+v", who, want)
	}
}

// A token acts for its owner with none of their admin powers, with or
// without an agent (the owner's decision, 16 Oct 2026: least privilege), so
// an admin's token is refused what only an admin may do: deleting for good a
// doc they never had, which they may do themselves in the app.
func TestAnAdminsTokenIsRefusedWhatOnlyAnAdminMay(t *testing.T) {
	if TokenSubjectOf(nil, "8f1d2c3b-4a5e-4f60-8b7a-9c0d1e2f3a4b") != (Subject{}) {
		t.Fatal("no account is someone")
	}
	var u userModels.UserInfo
	u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0x4", Uuid: "6e2a9d1c-3b5f-4d7e-8a0c-1f9b2e4d6c80"}
	u.UserPostgresInfo.IsAdmin = true
	self := SubjectOf(&u)
	token := TokenSubjectOf(&u, "")
	if token.Admin || token.ActingAdmin() || token.UserUID != self.UserUID || token.UserUUID != self.UserUUID {
		t.Fatalf("an admin's token = %+v (the account names %+v)", token, self)
	}
	const agent = "8f1d2c3b-4a5e-4f60-8b7a-9c0d1e2f3a4b"
	if bound := TokenSubjectOf(&u, agent); bound.Admin || bound.AgentUUID != agent {
		t.Fatalf("an admin's token bound to an agent = %+v", bound)
	}

	neverTheirs := Decision{Deleted: true, Was: LevelNone}
	if err := deletedRefusal(neverTheirs, LevelFull, self.ActingAdmin()); err != nil {
		t.Fatalf("the admin themselves, deleting for good: %v", err)
	}
	if err := deletedRefusal(neverTheirs, LevelFull, token.ActingAdmin()); err == nil {
		t.Fatal("an admin's token deleted for good a doc its owner never had")
	}
}

// An executor names its person as the app does in a session, and as a token
// does through one: the same admin is an admin acting themselves in the app,
// and no admin at all through the public API, MCP, or a run a token queued.
func TestSubjectForIsATokensSubjectThroughAToken(t *testing.T) {
	var u userModels.UserInfo
	u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0x3", Uuid: "5e2b8a1c-7d4f-4c9e-a3b6-1f8d2e7c9a04"}
	u.UserPostgresInfo.IsAdmin = true
	if who := SubjectFor(context.Background(), &u); !who.ActingAdmin() || who != SubjectOf(&u) {
		t.Errorf("in the app: %+v, want SubjectOf's %+v", who, SubjectOf(&u))
	}
	for name, ctx := range map[string]context.Context{
		"the public API or MCP":       context.WithValue(context.Background(), helpers.ApiTokenIDContextKey, "token-1"),
		"a run a token queued, later": helpers.WithActsThroughToken(context.Background()),
	} {
		if who := SubjectFor(ctx, &u); who.Admin || who.ActingAdmin() || who != TokenSubjectOf(&u, "") {
			t.Errorf("%s: %+v, want TokenSubjectOf's %+v", name, who, TokenSubjectOf(&u, ""))
		}
	}
	if who := SubjectFor(context.Background(), nil); who != (Subject{}) {
		t.Errorf("no account: %+v", who)
	}
}
