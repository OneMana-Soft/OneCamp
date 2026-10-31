package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	qualityModel "github.com/akashc777/OneCamp/models/postgres/CallQuality"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// How the install's calls went is an admin's to read, acting themselves: a
// member is refused, and so is an admin through an API token, and neither
// has the days read for them; someone signed out is asked to sign in.
func TestTheInstallsCallDaysAreAnAdminsAlone(t *testing.T) {
	was := qualityDays
	t.Cleanup(func() { qualityDays = was })
	read := 0
	qualityDays = func(context.Context, int, time.Time) ([]qualityModel.Day, error) {
		read++
		return []qualityModel.Day{}, nil
	}
	id := uuid.New()
	member := &models.UserInfo{}
	member.UserDgraphInfo.Uid, member.UserDgraphInfo.Uuid, member.UserPostgresInfo.Id = "0x2a", id.String(), id
	admin := *member
	admin.UserPostgresInfo.IsAdmin = true
	for _, c := range []struct {
		who   string
		info  *models.UserInfo
		token bool
		want  int
		code  string
	}{
		{"someone signed out", nil, false, http.StatusUnauthorized, ""},
		{"a member", member, false, http.StatusForbidden, "not_allowed"},
		{"an admin through an API token", &admin, true, http.StatusForbidden, "not_allowed"},
		{"an admin", &admin, false, http.StatusOK, ""},
	} {
		before := read
		got, out := askAs(CallQualityDays, httptest.NewRequest(http.MethodGet, "/calls/quality/days", nil), c.info, c.token)
		if got != c.want || (c.code != "" && codeOf(out) != c.code) {
			t.Errorf("%s: %d %q, want %d %q", c.who, got, codeOf(out), c.want, c.code)
		}
		if asked := read > before; asked != (c.want == http.StatusOK) {
			t.Errorf("%s: the days were read: %v", c.who, asked)
		}
	}
}
