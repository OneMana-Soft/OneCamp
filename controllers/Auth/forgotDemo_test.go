package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// forgot asks POST /auth/forgot-password for an account that can reset, with
// every step past the answer faked, and says what the server answered, how
// often it looked the address up and minted a token, and whether a link was
// mailed within wait.
func forgot(t *testing.T, wait time.Duration) (body string, looked, minted int32, mailed bool) {
	t.Helper()
	prevAccount, prevMint, prevSend := resettableAccount, mintResetToken, sendPasswordReset
	t.Cleanup(func() { resettableAccount, mintResetToken, sendPasswordReset = prevAccount, prevMint, prevSend })
	var lookups, mints atomic.Int32
	sent := make(chan struct{}, 1)
	resettableAccount = func(context.Context, string) (uuid.UUID, bool) {
		lookups.Add(1)
		return uuid.New(), true
	}
	mintResetToken = func(uuid.UUID, string, time.Time) error {
		mints.Add(1)
		return nil
	}
	sendPasswordReset = func(context.Context, string, string, string) error {
		sent <- struct{}{}
		return nil
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/forgot-password", strings.NewReader(`{"email":"visitor@kestrel.studio"}`))
	ForgotPassword(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 whatever the address", rec.Code)
	}
	select {
	case <-sent:
		mailed = true
	case <-time.After(wait):
	}
	return rec.Body.String(), lookups.Load(), mints.Load(), mailed
}

// The public demo's addresses are its shared visitor's, its cast's and those
// of the few who run it, and anyone may ask for a reset without signing in.
// On the demo a reset for a real account answers exactly as one off it does,
// and nothing is looked up, minted or mailed (V-1261). Off the demo the same
// request mints and mails one link, so the fakes are the ones the handler
// calls.
func TestTheDemoMailsNoResetLink(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "re_test")

	t.Setenv("DEMO_MODE", "")
	want, looked, minted, mailed := forgot(t, 2*time.Second)
	if looked != 1 || minted != 1 || !mailed {
		t.Fatalf("off the demo: looked up %d, minted %d, mailed %v; want 1, 1, true", looked, minted, mailed)
	}

	t.Setenv("DEMO_MODE", "true")
	got, looked, minted, mailed := forgot(t, 100*time.Millisecond)
	if got != want {
		t.Errorf("the demo answered %q, want the answer every reset gets %q", got, want)
	}
	if looked != 0 || minted != 0 || mailed {
		t.Errorf("on the demo: looked up %d, minted %d, mailed %v; want 0, 0, false", looked, minted, mailed)
	}
}
