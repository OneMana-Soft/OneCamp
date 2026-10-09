package controllers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	emailService "github.com/akashc777/OneCamp/services/Email"
)

func answerFor(t *testing.T, resent bool, sendErr error) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	respondInvitation(rec, resent, "https://onecamp.example.com/signup?token=abc", sendErr)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	return body
}

// The answer says what happened to the email, not whether a key is set: the
// provider took it, or here is why not and the link to send yourself. It said
// "Invitation sent" whenever a key was set, and dropped the send's error.
func TestTheInvitationAnswerSaysWhatHappenedToTheEmail(t *testing.T) {
	sent := answerFor(t, false, nil)
	if sent["email_sent"] != true || sent["email_error"] != "" || sent["invite_link"] != "https://onecamp.example.com/signup?token=abc" {
		t.Errorf("a sent invitation answered %v", sent)
	}

	refused := answerFor(t, false, &emailService.SendError{StatusCode: 403, Detail: "The onemana.dev domain is not verified."})
	if refused["email_sent"] != false || refused["invite_link"] == "" {
		t.Errorf("a refused email answered %v", refused)
	}
	if reason := refused["email_error"].(string); reason != "the email provider refused it (The onemana.dev domain is not verified)" {
		t.Errorf("reason %q", reason)
	}
	if msg := refused["msg"].(string); !strings.Contains(msg, "couldn't be emailed") || !strings.Contains(msg, "Copy the link") {
		t.Errorf("message %q", msg)
	}

	off := answerFor(t, false, emailService.ErrNotConfigured)
	if off["email_error"] != "email isn't set up on this server" {
		t.Errorf("with email off the reason is %v", off["email_error"])
	}

	again := answerFor(t, true, nil)
	if msg := again["msg"].(string); !strings.Contains(msg, "old link no longer works") {
		t.Errorf("sending again says %q", msg)
	}
}
