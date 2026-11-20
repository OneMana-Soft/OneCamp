package email

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// The public demo mails nothing, whoever asks and whatever it is: its
// addresses are its shared visitor's, its cast's and those of the few who run
// it (V-1261). A password reset and an invitation are refused before the
// provider is asked, with a key set and the address deliverable.
func TestTheDemoSendsNoEmailAtAll(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("EMAIL_DAILY_CAP", "")
	t.Setenv("DEMO_MODE", "true")
	reset := func() {
		sentToday.mu.Lock()
		sentToday.day, sentToday.count = "", 0
		sentToday.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	oldMX, oldClient := lookupMX, SharedHTTPClient
	t.Cleanup(func() { lookupMX, SharedHTTPClient = oldMX, oldClient })
	lookupMX = func(context.Context, string) ([]*net.MX, error) { return []*net.MX{{Host: "mx.acme.org."}}, nil }
	calls := 0
	SharedHTTPClient = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"m1"}`)), Header: http.Header{}}, nil
	})}
	ctx := context.Background()

	if err := SendPasswordResetEmail(ctx, "ada@acme.org", "noreply@acme.org", "https://app.acme.org/reset-password?token=t"); err == nil {
		t.Error("the demo sent a password reset")
	}
	invite := SendOptions{From: "noreply@acme.org", To: "ada@acme.org", Subject: "Join", HTML: "<p>Join</p>", Keep: InvitationReserve}
	if _, err := SendEmailWithOptions(ctx, invite); err == nil {
		t.Error("the demo sent an invitation")
	}
	if calls != 0 {
		t.Fatalf("the provider was asked %d times on the demo, want 0", calls)
	}
}
