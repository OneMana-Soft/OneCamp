package notification

import (
	"testing"
	"time"

	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
)

// TestQuietHoursDelay covers the four meaningful cases of quietHoursDelay:
//   - prefs disabled (returns now)
//   - same-day window (08:00–17:00)
//   - overnight window (22:00–07:00) when "now" is in the night portion
//   - overnight window (22:00–07:00) when "now" is in the morning portion
func TestQuietHoursDelay(t *testing.T) {
	loc, _ := time.LoadLocation("UTC")
	tz := "UTC"

	makePref := func(enabled bool, start, end string) *prefModels.UserNotificationPreference {
		s, e, t := start, end, tz
		return &prefModels.UserNotificationPreference{
			QuietHoursEnabled: enabled,
			QuietHoursStart:   &s,
			QuietHoursEnd:     &e,
			QuietHoursTZ:      &t,
		}
	}

	t.Run("disabled returns now", func(t *testing.T) {
		now := time.Date(2026, 5, 23, 10, 0, 0, 0, loc)
		got, deferred := quietHoursDelay(now, &prefModels.UserNotificationPreference{})
		if deferred {
			t.Fatalf("expected !deferred when disabled")
		}
		if !got.Equal(now) {
			t.Fatalf("expected return = now")
		}
	})

	t.Run("same-day window inside", func(t *testing.T) {
		// 10:00 with window 08:00-17:00 → defer to 17:00.
		pref := makePref(true, "08:00", "17:00")
		now := time.Date(2026, 5, 23, 10, 0, 0, 0, loc)
		got, deferred := quietHoursDelay(now, pref)
		if !deferred {
			t.Fatalf("expected deferred inside window")
		}
		expected := time.Date(2026, 5, 23, 17, 0, 0, 0, loc)
		if !got.Equal(expected) {
			t.Fatalf("expected %v got %v", expected, got)
		}
	})

	t.Run("same-day window outside", func(t *testing.T) {
		pref := makePref(true, "08:00", "17:00")
		now := time.Date(2026, 5, 23, 20, 0, 0, 0, loc)
		got, deferred := quietHoursDelay(now, pref)
		if deferred {
			t.Fatalf("expected !deferred outside window")
		}
		if !got.Equal(now) {
			t.Fatalf("expected return = now")
		}
	})

	t.Run("overnight window — night side", func(t *testing.T) {
		// 23:00 with 22:00-07:00 → defer to 07:00 next day.
		pref := makePref(true, "22:00", "07:00")
		now := time.Date(2026, 5, 23, 23, 0, 0, 0, loc)
		got, deferred := quietHoursDelay(now, pref)
		if !deferred {
			t.Fatalf("expected deferred")
		}
		expected := time.Date(2026, 5, 24, 7, 0, 0, 0, loc)
		if !got.Equal(expected) {
			t.Fatalf("expected %v got %v", expected, got)
		}
	})

	t.Run("overnight window — morning side", func(t *testing.T) {
		// 03:00 with 22:00-07:00 → defer to 07:00 same day.
		pref := makePref(true, "22:00", "07:00")
		now := time.Date(2026, 5, 23, 3, 0, 0, 0, loc)
		got, deferred := quietHoursDelay(now, pref)
		if !deferred {
			t.Fatalf("expected deferred")
		}
		expected := time.Date(2026, 5, 23, 7, 0, 0, 0, loc)
		if !got.Equal(expected) {
			t.Fatalf("expected %v got %v", expected, got)
		}
	})

	t.Run("overnight window — outside", func(t *testing.T) {
		pref := makePref(true, "22:00", "07:00")
		now := time.Date(2026, 5, 23, 12, 0, 0, 0, loc)
		got, deferred := quietHoursDelay(now, pref)
		if deferred {
			t.Fatalf("expected !deferred")
		}
		if !got.Equal(now) {
			t.Fatalf("expected return = now")
		}
	})

	t.Run("invalid HHMM falls back to now", func(t *testing.T) {
		pref := makePref(true, "bad", "07:00")
		now := time.Date(2026, 5, 23, 23, 0, 0, 0, loc)
		got, deferred := quietHoursDelay(now, pref)
		if deferred {
			t.Fatalf("expected !deferred for invalid input")
		}
		if !got.Equal(now) {
			t.Fatalf("expected return = now")
		}
	})
}

// TestRecipientsFromStrings covers the dedup/parse contract used by every
// wiring helper.
func TestRecipientsFromStrings(t *testing.T) {
	good := "11111111-1111-1111-1111-111111111111"
	got := recipientsFromStrings([]string{good, good, "", "not-a-uuid"})
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 recipient, got %d", len(got))
	}
	if got[0].UserUUID.String() != good {
		t.Fatalf("expected %s, got %s", good, got[0].UserUUID.String())
	}
}

// TestDeliveryTime: a pause holds email until it ends, and quiet hours are
// then read at that moment, not at the time the email was raised.
func TestDeliveryTime(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return time.Date(2026, 10, 5, h, m, 0, 0, time.UTC) }
	ptr := func(v time.Time) *time.Time { return &v }
	str := func(s string) *string { return &s }
	cases := []struct {
		name     string
		pref     prefModels.UserNotificationPreference
		want     time.Time
		deferred bool
	}{
		{"nothing set", prefModels.UserNotificationPreference{}, now, false},
		{"pause ended", prefModels.UserNotificationPreference{NotificationsPausedUntil: ptr(at(9, 0))}, now, false},
		{"paused", prefModels.UserNotificationPreference{NotificationsPausedUntil: ptr(at(11, 30))}, at(11, 30), true},
		{"pause ends inside quiet hours", prefModels.UserNotificationPreference{
			NotificationsPausedUntil: ptr(at(12, 30)),
			QuietHoursEnabled:        true, QuietHoursStart: str("12:00"), QuietHoursEnd: str("13:00"),
		}, at(13, 0), true},
		{"empty quiet window", prefModels.UserNotificationPreference{
			QuietHoursEnabled: true, QuietHoursStart: str("09:00"), QuietHoursEnd: str("09:00"),
		}, now, false},
	}
	for _, c := range cases {
		got, deferred := deliveryTime(now, &c.pref)
		if !got.Equal(c.want) || deferred != c.deferred {
			t.Errorf("%s: got %v %v, want %v %v", c.name, got, deferred, c.want, c.deferred)
		}
	}
}
