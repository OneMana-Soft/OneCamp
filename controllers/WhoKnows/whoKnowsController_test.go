package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func requestAs(ctx context.Context, admin bool) *http.Request {
	var u userModels.UserInfo
	u.UserPostgresInfo.Id = uuid.New()
	u.UserPostgresInfo.EmailID = "a@example.test"
	u.UserPostgresInfo.IsAdmin = admin
	u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0x1", Uuid: u.UserPostgresInfo.Id.String(),
		Channels: []*dgraphStruct.DgraphChannel{{Uuid: "c"}}, Projects: []*dgraphStruct.DgraphProject{{Uuid: "p"}}, Teams: []*dgraphStruct.DgraphTeam{{Uuid: "t"}}}
	req := httptest.NewRequest("GET", "/people/who-knows?q=plan", nil)
	return req.WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, u))
}

// Through a token, an admin asks as a member: the subject carries no admin
// flag. In the app, the same admin's subject does (it changes nothing in Who
// knows, which never reads it, but it shows the token is what removed it).
func TestWhoKnowsThroughATokenReadsAsAMember(t *testing.T) {
	a, ok := askerOf(requestAs(helpers.WithActsThroughToken(context.Background()), true))
	if !ok || a.Subject.Admin {
		t.Errorf("through a token: %+v, want a member's subject", a.Subject)
	}
	a, _ = askerOf(requestAs(context.Background(), true))
	if !a.Subject.Admin {
		t.Error("in the app the admin's own subject lost its flag; the test above proves nothing")
	}
	if strings.Join(a.Channels, ",") != "c" || strings.Join(a.Projects, ",") != "p" || strings.Join(a.Teams, ",") != "t" || a.Email != "a@example.test" {
		t.Errorf("reach %+v", a)
	}
}

// Nobody signed in is told to sign in; words too short or too long are 400s,
// before
// anything is read.
func TestWhoKnowsRefusesBeforeReading(t *testing.T) {
	rec := httptest.NewRecorder()
	GetWhoKnows(rec, httptest.NewRequest("GET", "/people/who-knows?q=plan", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("nobody: %d", rec.Code)
	}
	for _, q := range []string{"q=a", "q=" + strings.Repeat("x", 201)} {
		rec := httptest.NewRecorder()
		r := requestAs(context.Background(), false)
		r.URL.RawQuery = q
		GetWhoKnows(rec, r)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	r := requestAs(context.Background(), false)
	r.Method, r.Body = "PUT", http.NoBody
	SetMyWhoKnows(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a PUT with no choice: %d", rec.Code)
	}
}
