//go:build integration
// +build integration

package integration_test

// Emails show what people wrote as plain text, through the real path: a
// notice told (Tell, its email), a chat message, a task's name and its status
// change go through the dispatcher, the email template and the email queue,
// and the queued email is read. What someone wrote is text in the HTML part,
// escaped once, and as written in the plain-text part and the subject.
//
// Run: go test -tags=integration ./tests/integration/ -run TestNoticeEmailsShowWhatPeopleWroteAsPlainText -v

import (
	"context"
	"database/sql"
	"html"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	notification "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestNoticeEmailsShowWhatPeopleWroteAsPlainText(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	// Notification email on, as an install with a sending key has it; nothing
	// is sent, the queue is read.
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	t.Setenv("FRONTEND_DOMAIN", "https://team.example.test")
	ana, ben := w.people["ana"].String(), w.people["ben"].String()
	// Ben is emailed whether or not he is online: no device is connected here.
	w.exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token, email_only_when_offline) VALUES ($1, $2, false)`, ben, uuid.NewString())

	const wrote = `<a href="https://x">y</a> & <img src=z> <style>p{}</style>`
	shown := template.HTMLEscapeString(wrote)

	type queued struct{ subject, html, text string }
	// email waits for ben's email of this kind whose dedup key names about.
	email := func(t *testing.T, kind notification.EventType, about string) queued {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var q queued
			var text sql.NullString
			err := w.env.PG.QueryRow(`SELECT subject, html_body, text_body FROM notification_email_queue
				WHERE user_id = $1 AND event_type = $2 AND dedup_key LIKE '%' || $3 || '%'`, ben, string(kind), about).Scan(&q.subject, &q.html, &text)
			if err == nil {
				q.text = text.String
				return q
			}
			if err != sql.ErrNoRows {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("no %s email reached ben", kind)
		return queued{}
	}
	// asWritten checks one email: the text is shown once-escaped in the HTML
	// part, never as HTML, never escaped twice, and as written in the text.
	asWritten := func(t *testing.T, q queued, inText string) {
		t.Helper()
		for _, raw := range []string{`<a href="https://x"`, "<img src=z>", "<style>p{}"} {
			if strings.Contains(q.html, raw) {
				t.Errorf("the HTML part holds %q as HTML:\n%s", raw, q.html)
			}
		}
		if !strings.Contains(q.html, shown) {
			t.Errorf("the HTML part doesn't show %q as text:\n%s", wrote, q.html)
		}
		for _, twice := range []string{"&amp;amp;", "&amp;lt;", "&amp;#34;"} {
			if strings.Contains(q.html, twice) {
				t.Errorf("the HTML part escapes something twice (%q):\n%s", twice, q.html)
			}
		}
		if !strings.Contains(q.text, inText) || strings.Contains(q.text, "&lt;") || strings.Contains(q.text, "&amp;") {
			t.Errorf("the plain-text part doesn't show %q as written:\n%s", inText, q.text)
		}
	}

	t.Run("a chat mention told as a notice", func(t *testing.T) {
		// The editor keeps what was typed escaped; the mention's line is its
		// text (business/Chat chatMentions.go).
		stored := "<p>" + html.EscapeString(wrote) + "</p>"
		key := "chat.mentioned|" + uuid.NewString()
		notification.Tell(ctx, notification.Notice{
			Kind: "chat.mentioned", ResourceKind: "chat", ResourceUUID: uuid.NewString(), ResourceTitle: "a conversation",
			Actor: &dgraphStruct.DgraphUser{Uuid: ana, UserName: "ana"}, DedupKey: key,
			Title: "ana mentioned you in a conversation", Body: helpers.HTMLToPlainText(stored),
			Path: "/app/chat", Email: notification.EventGroupChatMention,
		}, []notification.NoticeRecipient{{UserUUID: ben, Reason: "mention"}})
		asWritten(t, email(t, notification.EventGroupChatMention, key), wrote)
	})

	t.Run("a chat message", func(t *testing.T) {
		msg := uuid.NewString()
		notification.DispatchChatDM(ana, "ana", "", helpers.GetGroupingId(ana, ben), msg, wrote, []string{ben}, false)
		asWritten(t, email(t, notification.EventChatDM, msg), wrote)
	})

	t.Run("a task's name", func(t *testing.T) {
		notification.DispatchTaskAssignment(ana, "ana", "", w.mine, "Ship "+wrote, "R&D", ben)
		asWritten(t, email(t, notification.EventTaskAssigned, w.mine), "R&D · Ship "+wrote)
	})

	t.Run("a task's name in the subject", func(t *testing.T) {
		change := uuid.NewString()
		notification.DispatchTaskStatusChange(ana, "ana", "", w.mine, "Ship "+wrote, "R&D", "To do", "Done", change, []string{ben})
		q := email(t, notification.EventTaskStatus, change)
		if q.subject != "ana moved Ship "+wrote+" to Done" {
			t.Errorf("the subject isn't the name as written: %q", q.subject)
		}
		asWritten(t, q, "ana moved Ship "+wrote+" to Done")
	})
}
