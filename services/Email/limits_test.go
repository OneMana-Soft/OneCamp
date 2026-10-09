package email

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTheDailyCapRefusesThenResetsTheNextDay(t *testing.T) {
	var c dailyCounter
	day := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := c.take(day, 3, 0); err != nil {
			t.Fatalf("message %d is within the cap: %v", i+1, err)
		}
	}
	err := c.take(day, 3, 0)
	if err == nil || IsTerminal(err) {
		t.Fatalf("the fourth is refused, and retryable: %v", err)
	}
	if err := c.take(day.Add(24*time.Hour), 3, 0); err != nil {
		t.Fatalf("a new day starts a new count: %v", err)
	}
	if err := c.take(day, 0, 0); err != nil {
		t.Fatal("no cap means no limit")
	}
}

func TestEssentialOnlyTurnsOffNotificationEmail(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "re_x")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	if !NotificationEmailEnabled() {
		t.Fatal("with a key and no restriction, notifications are emailed")
	}
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "true")
	if NotificationEmailEnabled() || !IsEmailEnabled() {
		t.Fatal("essential only: no notification email, but email itself is on")
	}
}

// Invitations stop InvitationReserve short of the day's cap, so a password
// reset still goes out after an admin has invited a whole team, and the
// refusal is worded for the admin.
func TestInvitationsKeepTheLastOfTheDayForPasswordResets(t *testing.T) {
	var c dailyCounter
	day := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	if got := c.left(day, 20, InvitationReserve); got != 15 {
		t.Fatalf("a fresh day leaves %d invitations, want 15", got)
	}
	for i := 0; i < 15; i++ {
		if err := c.take(day, 20, InvitationReserve); err != nil {
			t.Fatalf("invitation %d: %v", i+1, err)
		}
	}
	err := c.take(day, 20, InvitationReserve)
	if err == nil || IsTerminal(err) || !strings.Contains(Reason(err), "kept for password resets") {
		t.Fatalf("the sixteenth invitation: %v (%q)", err, Reason(err))
	}
	if got := c.left(day, 20, InvitationReserve); got != 0 {
		t.Errorf("%d invitations left, want none", got)
	}
	for i := 0; i < 5; i++ {
		if err := c.take(day, 20, 0); err != nil {
			t.Fatalf("password reset %d after the invitations: %v", i+1, err)
		}
	}
	if err := c.take(day, 20, 0); err == nil || !strings.Contains(Reason(err), "sent its 20 emails for today") {
		t.Fatalf("past the cap itself: %v", err)
	}
}

// A small cap still emails invitations: the reserve is at most a quarter of
// it. With all five kept, a cap of 5 or less emailed none.
func TestASmallDailyCapStillEmailsInvitations(t *testing.T) {
	day := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	for cap, want := range map[int]int{1: 1, 3: 3, 4: 3, 5: 4, 8: 6, 20: 15, 100: 95} {
		var c dailyCounter
		if got := c.left(day, cap, InvitationReserve); got != want {
			t.Errorf("a cap of %d leaves %d invitations, want %d", cap, got, want)
		}
		for i := 0; i < want; i++ {
			if err := c.take(day, cap, InvitationReserve); err != nil {
				t.Fatalf("cap %d: invitation %d: %v", cap, i+1, err)
			}
		}
		if err := c.take(day, cap, InvitationReserve); err == nil {
			t.Errorf("cap %d: an invitation past the allowance went", cap)
		}
		for i := want; i < cap; i++ {
			if err := c.take(day, cap, 0); err != nil {
				t.Errorf("cap %d: a password reset after the invitations: %v", cap, err)
			}
		}
	}
}

// An invitation past the allowance is refused before the provider is asked;
// one within it goes, and so does a password reset after it.
func TestASendKeepingTheReserveIsRefusedBeforeTheProviderIsAsked(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("EMAIL_DAILY_CAP", "20") // fifteen invitations, five kept back
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

	if left, capped := InvitationsLeftToday(); !capped || left != 15 {
		t.Fatalf("allowance %d (capped %v), want 15", left, capped)
	}
	invite := SendOptions{From: "noreply@acme.org", To: "ada@acme.org", Subject: "Join", HTML: "<p>Join</p>", Keep: InvitationReserve}
	ctx := context.Background()
	for i := 0; i < 15; i++ {
		if _, err := SendEmailWithOptions(ctx, invite); err != nil {
			t.Fatalf("invitation %d: %v", i+1, err)
		}
	}
	if _, err := SendEmailWithOptions(ctx, invite); err == nil || calls != 15 {
		t.Fatalf("the sixteenth invitation: %v after %d calls to the provider, want refused after 15", err, calls)
	}
	if left, _ := InvitationsLeftToday(); left != 0 {
		t.Errorf("allowance after fifteen: %d", left)
	}
	if err := SendPasswordResetEmail(ctx, "cy@acme.org", "noreply@acme.org", "https://acme.org/reset"); err != nil {
		t.Errorf("a password reset after the invitations: %v", err)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
