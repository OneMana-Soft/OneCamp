package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
)

// The upload dialog says this server's real limit: it said 50 GB while the
// server refused anything over 5 GB.
func TestTheExportLimitIsTheServers(t *testing.T) {
	ask := func() int64 {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(context.Background(),
			helpers.UserInfoContextKey, userModel.UserInfo{UserPostgresInfo: userModel.User{IsAdmin: true}}))
		rec := httptest.NewRecorder()
		HandleLimits(rec, req)
		var out struct {
			MaxBytes int64 `json:"max_bytes"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		return out.MaxBytes
	}
	t.Setenv("EXPORT_MAX_BYTES", "")
	if got := ask(); got != 5*1024*1024*1024 {
		t.Errorf("default: %d", got)
	}
	t.Setenv("EXPORT_MAX_BYTES", "21474836480")
	if got := ask(); got != 20*1024*1024*1024 {
		t.Errorf("raised: %d", got)
	}
	if got := tooLarge(7730941132, 5*1024*1024*1024); got != "That export is 7.2 GB, and this server takes exports up to 5 GB. Whoever runs it can raise the limit (EXPORT_MAX_BYTES)." {
		t.Errorf("too large: %q", got)
	}

	rec := httptest.NewRecorder()
	HandleLimits(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(context.Background(),
		helpers.UserInfoContextKey, userModel.UserInfo{})))
	if rec.Code != http.StatusForbidden {
		t.Errorf("a member asked: %d", rec.Code)
	}
}
