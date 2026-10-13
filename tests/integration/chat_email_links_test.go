//go:build integration
// +build integration

package integration_test

// A chat email opens the conversation it is about, for whoever reads it,
// through the real dispatcher and email queue.
//
// Every DM, group, thread-reply and call email linked /app/chat/<grouping
// id>. A DM's grouping id is both people's ids with a space between them,
// and the web app reads that segment as the other person's id, so "View
// conversation" opened a conversation with nobody (TN-BUG-7). The link is
// built per recipient now: a DM at the other person, a group at its route,
// a thread reply at its thread.
//
// Run: go test -tags=integration ./tests/integration/ -run TestChatEmailsOpenTheConversationForEachReader -v

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	notification "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestChatEmailsOpenTheConversationForEachReader(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	// Notification email on, as an install with a sending key has it; nothing
	// is sent, the queue is read.
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	t.Setenv("FRONTEND_DOMAIN", "https://team.example.test")
	const base = "https://team.example.test"

	people := map[string]uuid.UUID{"maya": uuid.New(), "jonas": uuid.New(), "sam": uuid.New()}
	for name, id := range people {
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, id, name+"@example.test", name, name); err != nil {
			t.Fatal(err)
		}
		// Email whether or not they're online: no device is connected here.
		if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token, email_only_when_offline)
			VALUES ($1, $2, false)`, id, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	maya, jonas, sam := people["maya"].String(), people["jonas"].String(), people["sam"].String()
	dm := helpers.GetGroupingId(maya, jonas)
	const group = "0123456789abcdef0123456789abcdef"

	// link waits for the email of this kind to who, the one whose dedup key
	// names about, and answers the link it opens; the html must carry it too.
	link := func(t *testing.T, who string, kind notification.EventType, about string) string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var cta sql.NullString
			var html string
			err := env.PG.QueryRow(`SELECT cta_url, html_body FROM notification_email_queue
				WHERE user_id = $1 AND event_type = $2 AND dedup_key LIKE '%' || $3 || '%'`, who, string(kind), about).Scan(&cta, &html)
			if err == nil {
				if !strings.Contains(html, cta.String) {
					t.Errorf("the %s email's body doesn't carry its link %q", kind, cta.String)
				}
				return cta.String
			}
			if err != sql.ErrNoRows {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("no %s email reached %s", kind, who)
		return ""
	}

	t.Run("each side of a DM email opens the conversation with the other person", func(t *testing.T) {
		toMaya, toJonas := uuid.NewString(), uuid.NewString()
		notification.DispatchChatDM(jonas, "Jonas", "", dm, toMaya, "the deck is ready", []string{maya}, false)
		notification.DispatchChatDM(maya, "Maya", "", dm, toJonas, "looking now", []string{jonas}, false)
		if got := link(t, maya, notification.EventChatDM, toMaya); got != base+"/app/chat/"+jonas {
			t.Errorf("Maya's DM email opens %q, want the conversation with Jonas", got)
		}
		if got := link(t, jonas, notification.EventChatDM, toJonas); got != base+"/app/chat/"+maya {
			t.Errorf("Jonas's DM email opens %q, want the conversation with Maya", got)
		}
	})

	t.Run("a group email opens the group", func(t *testing.T) {
		msg := uuid.NewString()
		notification.DispatchChatDM(sam, "Sam", "", group, msg, "launch at noon", []string{maya, jonas}, false)
		for _, who := range []string{maya, jonas} {
			if got := link(t, who, notification.EventChatDM, msg); got != base+"/app/chat/group/"+group {
				t.Errorf("a group email opens %q, want the group", got)
			}
		}
	})

	t.Run("a thread reply email opens its thread", func(t *testing.T) {
		parent, reply := uuid.NewString(), uuid.NewString()
		notification.DispatchChatComment(jonas, "Jonas", "", dm, parent, "replying here", reply, []string{maya})
		if got := link(t, maya, notification.EventChatComment, reply); got != base+"/app/chat/"+jonas+"/"+parent {
			t.Errorf("a DM thread reply email opens %q, want its thread", got)
		}
	})

	t.Run("a call email opens the conversation", func(t *testing.T) {
		notification.DispatchChatCall(sam, "Sam", "", helpers.GetGroupingId(sam, maya), []string{maya})
		if got := link(t, maya, notification.EventChatCall, sam); got != base+"/app/chat/"+sam {
			t.Errorf("a DM call email opens %q, want the conversation with Sam", got)
		}
	})
}
