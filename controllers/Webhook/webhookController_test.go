package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	business "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// ------------------------------------------------------------------
// Pure helper tests (no DB)
// ------------------------------------------------------------------

func TestIsWebhookValidationError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"random internal", errors.New("database connection lost"), false},
		{"invalid type", errors.New("invalid webhook type: foo"), true},
		{"outgoing requires url", errors.New("outgoing webhook requires a target_url"), true},
		{"invalid target_url", errors.New("invalid target_url: parse error"), true},
		{"marshal fail", errors.New("failed to marshal events"), true},
		{"not found", errors.New("webhook not found"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isWebhookValidationError(c.err)
			if got != c.want {
				t.Errorf("isWebhookValidationError(%q) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// ------------------------------------------------------------------
// Request-shape validation tests (no DB — rely on early-return guards)
// ------------------------------------------------------------------

func TestHandleCreateWebhook_Unauthorized(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/admin/webhooks", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	HandleCreateWebhook(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleCreateWebhook_InvalidBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/admin/webhooks", strings.NewReader("not-json"))
	req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, fakeAdminUser()))
	w := httptest.NewRecorder()

	HandleCreateWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleCreateWebhook_MissingName(t *testing.T) {
	body, _ := json.Marshal(business.WebhookCreateInput{Type: "incoming"})
	req := httptest.NewRequest(http.MethodPost, "/admin/webhooks", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, fakeAdminUser()))
	w := httptest.NewRecorder()

	HandleCreateWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if !strings.Contains(w.Body.String(), "Name is required") {
		t.Errorf("body = %q, want 'Name is required'", w.Body.String())
	}
}

func TestHandleCreateWebhook_MissingType(t *testing.T) {
	body, _ := json.Marshal(business.WebhookCreateInput{Name: "test"})
	req := httptest.NewRequest(http.MethodPost, "/admin/webhooks", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, fakeAdminUser()))
	w := httptest.NewRecorder()

	HandleCreateWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if !strings.Contains(w.Body.String(), "Type is required") {
		t.Errorf("body = %q, want 'Type is required'", w.Body.String())
	}
}

func TestHandleCreateWebhook_InvalidType(t *testing.T) {
	body, _ := json.Marshal(business.WebhookCreateInput{Name: "test", Type: "unknown"})
	req := httptest.NewRequest(http.MethodPost, "/admin/webhooks", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, fakeAdminUser()))
	w := httptest.NewRecorder()

	HandleCreateWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleCreateWebhook_NameTooLong(t *testing.T) {
	body, _ := json.Marshal(business.WebhookCreateInput{
		Name: strings.Repeat("a", 121),
		Type: "incoming",
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/webhooks", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, fakeAdminUser()))
	w := httptest.NewRecorder()

	HandleCreateWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleCreateWebhook_EmptyEventString(t *testing.T) {
	body, _ := json.Marshal(business.WebhookCreateInput{
		Name:   "test",
		Type:   "incoming",
		Events: []string{"post.created", "", "chat.created"},
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/webhooks", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, fakeAdminUser()))
	w := httptest.NewRecorder()

	HandleCreateWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if !strings.Contains(w.Body.String(), "Event at index 1 cannot be empty") {
		t.Errorf("body = %q, want 'Event at index 1 cannot be empty'", w.Body.String())
	}
}

func TestHandleUpdateWebhook_InvalidID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/admin/webhooks/bad-id", strings.NewReader("{}"))
	req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, fakeAdminUser()))
	w := httptest.NewRecorder()

	HandleUpdateWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleUpdateWebhook_EmptyEventString(t *testing.T) {
	body, _ := json.Marshal(business.WebhookUpdateInput{
		Name:   "test",
		Events: []string{"", "post.created"},
	})
	req := httptest.NewRequest(http.MethodPut, "/admin/webhooks/"+uuid.New().String(), bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, fakeAdminUser()))
	w := httptest.NewRecorder()

	HandleUpdateWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// ------------------------------------------------------------------
// helpers
// ------------------------------------------------------------------

func fakeAdminUser() *userModel.UserInfo {
	username := "admin"
	return &userModel.UserInfo{
		UserPostgresInfo: userModel.User{
			Id:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			IsAdmin:  true,
			Username: &username,
		},
	}
}
