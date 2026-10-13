//go:build integration
// +build integration

package integration_test

// Focus time, against real Postgres: while one of your own focus events runs,
// the push gate holds your pushes, and a teammate is told so (TN-BUG-8). The
// status read for the "held" bar and Notify anyway left focus time out: during
// Maya's focus block Jonas's DM showed no bar, and Notify anyway answered
// "Their notifications aren't paused". Someone with no preferences row is in
// focus time all the same: the gate reads the calendar, not the row.
//
// Run: go test -tags=integration ./tests/integration/ -run TestFocusTimeIsHeldAsThePushGateHoldsIt -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	fcmDomain "github.com/akashc777/OneCamp/domain/UserFCMToken"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestFocusTimeIsHeldAsThePushGateHoldsIt(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}

	person := func(name string, withRow bool) uuid.UUID {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'web', $2)`, id, "token-"+name); err != nil {
			t.Fatal(err)
		}
		if withRow {
			if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2)`, id, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	focus := func(who uuid.UUID, from, to time.Duration) time.Time {
		end := time.Now().Add(to).UTC().Truncate(time.Second)
		if _, err := env.PG.Exec(`INSERT INTO calendar_events (title, start_time, end_time, created_by, is_focus)
			VALUES ('Focus', $1, $2, $3, true)`, time.Now().Add(from), end, who); err != nil {
			t.Fatal(err)
		}
		return end
	}

	maya := person("maya", true)   // has preferences
	ravi := person("ravi", false)  // never opened settings: no row
	jonas := person("jonas", true) // focus event over
	sam := person("sam", false)    // no row, no focus time
	mayaEnds := focus(maya, -30*time.Minute, 90*time.Minute)
	raviEnds := focus(ravi, -5*time.Minute, 25*time.Minute)
	focus(jonas, -2*time.Hour, -time.Hour)

	for _, c := range []struct {
		name   string
		id     uuid.UUID
		reason string
		until  time.Time
	}{
		{"in focus time", maya, "focus", mayaEnds},
		{"in focus time with no preferences row", ravi, "focus", raviEnds},
		{"after focus time", jonas, "", time.Time{}},
		{"no preferences row, no focus time", sam, "", time.Time{}},
	} {
		got, err := notificationBusiness.HeldStatusOf(c.id, time.Now())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Reason != c.reason {
			t.Fatalf("%s: reason %q, want %q", c.name, got.Reason, c.reason)
		}
		if c.reason != "" && (got.Until == nil || !got.Until.Equal(c.until)) {
			t.Fatalf("%s: held until %v, want %v", c.name, got.Until, c.until)
		}
		// What the sender is told is what the push gate does.
		tokens, err := fcmDomain.GetFCMTokenByUserId(ctx, c.id.String())
		if err != nil {
			t.Fatal(err)
		}
		if held := len(tokens) == 0; held != (c.reason != "") {
			t.Fatalf("%s: the push gate holds %v, the status says %q", c.name, held, c.reason)
		}
	}
}

// One rule for every hold: whatever holds a person's pushes holds their email
// and shows in the status a teammate reads, for each source there is (a pause
// they set, a focus-time event in their calendar). The push gate, email's hold
// and the status each kept a copy of the rule, and a copy that drifts holds one
// and not the others.
func TestEveryHoldIsReadAlikeByPushEmailAndStatus(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	person := func(name string) uuid.UUID {
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, id.String()+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'web', $2)`, id, "token-"+name); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2)`, id, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		return id
	}
	end := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	paused := person("paused")
	if _, err := env.PG.Exec(`UPDATE users_notification_preferences SET notifications_paused_until = $2 WHERE user_id = $1`, paused, end); err != nil {
		t.Fatal(err)
	}
	focused := person("focused")
	if _, err := env.PG.Exec(`INSERT INTO calendar_events (title, start_time, end_time, created_by, is_focus)
		VALUES ('Focus', NOW() - interval '10 minutes', $1, $2, true)`, end, focused); err != nil {
		t.Fatal(err)
	}
	// A plain meeting is not focus time.
	meeting := person("meeting")
	if _, err := env.PG.Exec(`INSERT INTO calendar_events (title, start_time, end_time, created_by, is_focus)
		VALUES ('Standup', NOW() - interval '10 minutes', $1, $2, false)`, end, meeting); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name   string
		id     uuid.UUID
		reason string
	}{
		{"a pause they set", paused, "paused"},
		{"a focus-time event", focused, "focus"},
		{"a meeting", meeting, ""},
	} {
		held := c.reason != ""
		tokens, err := fcmDomain.GetFCMTokenByUserId(ctx, c.id.String())
		if err != nil {
			t.Fatal(err)
		}
		if gate := len(tokens) == 0; gate != held {
			t.Errorf("%s: the push gate holds %v, want %v", c.name, gate, held)
		}
		pref, err := prefModels.GetByUserID(c.id)
		if err != nil {
			t.Fatal(err)
		}
		until := pref.QuietUntil(time.Now())
		if (until != nil) != held || (held && !until.Equal(end)) {
			t.Errorf("%s: email waits until %v, want held %v until %v", c.name, until, held, end)
		}
		status, err := notificationBusiness.HeldStatusOf(c.id, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if status.Reason != c.reason {
			t.Errorf("%s: the status says %q, want %q", c.name, status.Reason, c.reason)
		}
	}
}
