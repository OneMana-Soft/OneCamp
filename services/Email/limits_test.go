package email

import (
	"testing"
	"time"
)

func TestTheDailyCapRefusesThenResetsTheNextDay(t *testing.T) {
	var c dailyCounter
	day := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := c.take(day, 3); err != nil {
			t.Fatalf("message %d is within the cap: %v", i+1, err)
		}
	}
	err := c.take(day, 3)
	if err == nil || IsTerminal(err) {
		t.Fatalf("the fourth is refused, and retryable: %v", err)
	}
	if err := c.take(day.Add(24*time.Hour), 3); err != nil {
		t.Fatalf("a new day starts a new count: %v", err)
	}
	if err := c.take(day, 0); err != nil {
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
