package controllers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A handler asked with nobody signed in answers 401, it does not panic: it
// asserted the request's user (ctx.Value(...).(UserInfo)), which panics when
// there is none, and the recoverer turned that into a 500.
func TestAHandlerWithNobodySignedInAnswers401(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"GetSelfAdminProfile":      GetSelfAdminProfile,
		"GetActiveUserEmojiStatus": GetActiveUserEmojiStatus,
	} {
		rec := httptest.NewRecorder()
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked with nobody signed in: %v", name, r)
				}
			}()
			h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		}()
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with nobody signed in: %d, want 401", name, rec.Code)
		}
	}
}
