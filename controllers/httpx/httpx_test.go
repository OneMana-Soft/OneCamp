package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestMustUserAndRequireAdmin(t *testing.T) {
	msg := func(rec *httptest.ResponseRecorder) string {
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body["msg"]
	}
	rec := httptest.NewRecorder()
	if _, ok := MustUser(rec, httptest.NewRequest(http.MethodGet, "/", nil)); ok || rec.Code != http.StatusUnauthorized || msg(rec) != helpers.UnauthorizedMessage {
		t.Fatalf("nobody signed in: ok %v, %d %q", ok, rec.Code, msg(rec))
	}
	member := userModel.UserInfo{}
	member.UserPostgresInfo.EmailID = "m@example.test"
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, member))
	rec = httptest.NewRecorder()
	if u, ok := MustUser(rec, r); !ok || u.UserPostgresInfo.EmailID != "m@example.test" {
		t.Fatalf("a member: %v %+v", ok, u)
	}
	rec = httptest.NewRecorder()
	if _, ok := RequireAdmin(rec, r); ok || rec.Code != http.StatusForbidden {
		t.Fatalf("a member isn't an admin: %v %d", ok, rec.Code)
	}
	admin := member
	admin.UserPostgresInfo.IsAdmin = true
	r = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, &admin))
	rec = httptest.NewRecorder()
	if _, ok := RequireAdmin(rec, r); !ok {
		t.Fatalf("an admin (held as a pointer): %d", rec.Code)
	}
}
