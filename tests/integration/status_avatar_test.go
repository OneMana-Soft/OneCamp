//go:build integration
// +build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A2-11 (backend optimisation H4): the mover's avatar is only for the
// notification, so it is worked out where the notification is made, never on
// the request's own path; the notification still shows it.
func TestStatusChangeLeavesTheAvatarToTheNotification(t *testing.T) {
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	integration.AcceptingOpenSearch(t)
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	t.Setenv("FRONTEND_DOMAIN", "https://team.example.test")
	ben := w.people["ben"].String()
	w.exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token, email_only_when_offline) VALUES ($1, $2, false)`, ben, uuid.NewString())
	calls := integration.CountStoreCalls(t, w.env, w.dg.Endpoint)
	setAvatar := func(key string) {
		w.dg.Mutate(t, []map[string]any{{"uid": w.account("ana").UserDgraphInfo.Uid, "user_profile_object_key": key}})
	}

	// An uploaded picture: its attachment is read for the notification only,
	// so the request doesn't wait on it. The attachments table is held locked
	// while the status changes: a request that reads the picture on its own
	// path waits for the lock, or for its statement's timeout (3 s here). The
	// read runs on a ctx of its own (models/postgres/Attachment), so the
	// counter can't see it; the wait can.
	setAvatar(uuid.NewString())
	integration.Settle(t)
	lock, err := w.env.PG.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(`LOCK TABLE attachments IN ACCESS EXCLUSIVE MODE`); err != nil {
		_ = lock.Rollback()
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"task_uuid": w.mine, "task_status": "inProgress", "task_project_uuid": w.project})
	start := time.Now()
	code, out := servePost(w, calls, "ana", taskController.UpdateTaskStatus, raw)
	took := time.Since(start)
	_ = lock.Rollback()
	integration.Settle(t)
	if code != http.StatusOK {
		t.Fatalf("the status change: %d %s", code, out)
	}
	if took >= 2*time.Second {
		t.Errorf("the status change took %s while the pictures were locked: it read the mover's avatar on its own path, want none (the notification reads it)", took.Round(time.Millisecond))
	}

	// The notification still shows the mover's picture.
	const picture = "https://avatars.example.test/ana.png"
	setAvatar(picture)
	if code, out := measuredPost(w, calls, "ana", taskController.UpdateTaskStatus, map[string]string{"task_uuid": w.mine, "task_status": "done", "task_project_uuid": w.project}); code != http.StatusOK {
		t.Fatalf("the second status change: %d %s", code, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var html string
		err := w.env.PG.QueryRow(`SELECT html_body FROM notification_email_queue WHERE user_id = $1 AND subject LIKE '% to Done' LIMIT 1`, ben).Scan(&html)
		if err == nil {
			if !strings.Contains(html, picture) {
				t.Errorf("ben's email doesn't show ana's picture: %.400s", html)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no status email reached ben: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
