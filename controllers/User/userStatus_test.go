package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
)

// A status the server doesn't know is the caller's mistake, so it answers 400.
// It answered 401, which the web app reads as "sign in again".
func TestAnUnknownStatusIsABadRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/user/updateUserStatus", strings.NewReader(`{"user_status":"away"}`))
	r = r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, models.UserInfo{}))
	w := httptest.NewRecorder()
	UpdateUserStatus(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
}
