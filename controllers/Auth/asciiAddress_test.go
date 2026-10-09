package controllers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	business "github.com/akashc777/OneCamp/business/User"
)

// An address with characters outside ASCII is matched to no account: a
// password sign-in with one answers as a wrong password does, and single
// sign-on refuses it, both before any lookup (no store is reached here).
func TestAnAddressOutsideASCIIIsMatchedToNoAccount(t *testing.T) {
	rec := httptest.NewRecorder()
	EmailLogin(rec, httptest.NewRequest(http.MethodPost, "/auth/login",
		bytes.NewBufferString(`{"email":"Kate@example.com","password":"a long enough password"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("password sign-in: %d %s", rec.Code, rec.Body.String())
	}
	if _, _, err := lookupOrProvision(context.Background(), "İnci@example.com", "inci", "oidc"); !errors.Is(err, business.ErrAddressNotASCII) {
		t.Errorf("single sign-on: %v, want ErrAddressNotASCII", err)
	}
}
