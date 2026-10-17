package controllers

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	passwordResetModels "github.com/akashc777/OneCamp/models/postgres/PasswordReset"
)

// QA SH-20 (14 Oct 2026): a reset link with a made-up, used or expired token
// showed the "Choose a new password" form, and said it was no good only after
// a new password was typed. The page asks first.
func TestAResetLinkIsCheckedBeforeTheForm(t *testing.T) {
	prev := validResetToken
	t.Cleanup(func() { validResetToken = prev })
	validResetToken = func(token string) (*passwordResetModels.PasswordResetToken, error) {
		if token == "good" {
			return &passwordResetModels.PasswordResetToken{Token: token}, nil
		}
		return nil, errors.New("no such token")
	}
	ask := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		CheckResetLink(rec, httptest.NewRequest(http.MethodPost, "/auth/reset-password/check", bytes.NewBufferString(body)))
		return rec.Code, rec.Body.String()
	}
	if code, body := ask(`{"token":"good"}`); code != http.StatusOK || !strings.Contains(body, `"valid": true`) {
		t.Errorf("a good link: %d %s", code, body)
	}
	for _, body := range []string{`{"token":"made-up"}`, `{"token":""}`, `not json`} {
		if code, out := ask(body); code != http.StatusBadRequest || !strings.Contains(out, `"reset_link_invalid"`) {
			t.Errorf("%s: %d %s", body, code, out)
		}
	}
}
