package controllers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestInvitationLinkHasAScheme pins the bug. FE_HOST_DOMAIN as make install
// writes it is a bare host, and a bare host inside an href is a relative link to
// a mail client, so every invitation pointed nowhere. The link is built on the
// same base the reset email uses, which carries the scheme.
func TestInvitationLinkHasAScheme(t *testing.T) {
	link := invitationLink("https://onecamp.example.com", "tok123")
	if !strings.HasPrefix(link, "https://") {
		t.Fatalf("invitation link has no scheme: %q", link)
	}
	if link != "https://onecamp.example.com/signup?token=tok123" {
		t.Errorf("unexpected link %q", link)
	}
	// A trailing slash on the base must not double up.
	if got := invitationLink("https://onecamp.example.com/", "t"); strings.Contains(got, "com//") {
		t.Errorf("doubled slash in %q", got)
	}
}

// TestInvitationAnswerCarriesTheLink guards the honest response. Whether or not
// mail is set up, the admin gets the link they can hand over, and a flag that
// says whether an email is actually going out. "invitation sent successfully"
// on a server that cannot send was the first lie a new admin was told.
func TestInvitationAnswerCarriesTheLink(t *testing.T) {
	rec := httptest.NewRecorder()
	respondInvitation(rec, false, "https://onecamp.example.com/signup?token=abc")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if body["invite_link"] != "https://onecamp.example.com/signup?token=abc" {
		t.Errorf("link missing from the answer: %v", body)
	}
	if _, ok := body["email_sent"].(bool); !ok {
		t.Errorf("email_sent must be a boolean the client can branch on: %v", body["email_sent"])
	}
	msg, _ := body["msg"].(string)
	// In this test binary no mail key is configured, so the honest message is
	// the one that tells the admin to share the link.
	if body["email_sent"] == false && !strings.Contains(msg, "share the link") {
		t.Errorf("with email off the message must say to share the link, got %q", msg)
	}
}
