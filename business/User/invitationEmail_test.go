package business

import (
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// A workspace that never changed the email gets today's, which says who
// invited them and where; one an admin wrote is sent as written.
func TestAnInvitationEmailKeepsWhatAnAdminWrote(t *testing.T) {
	for _, shipped := range shippedInviteTemplates {
		if InviteTemplate(shipped) != DefaultInviteTemplate {
			t.Errorf("the shipped template was kept: %q", shipped)
		}
		if InviteTemplate(strings.ReplaceAll(shipped, "<p>", "\n  <p>")) != DefaultInviteTemplate {
			t.Error("the shipped template with other whitespace was kept")
		}
	}
	custom := `<p>Join Acme at {{signup_link}}</p>`
	if InviteTemplate(custom) != custom || InviteTemplate("") != DefaultInviteTemplate {
		t.Error("a custom or empty template was not handled")
	}
	if InviteSubject("You're invited to OneCamp!") != DefaultInviteSubject || InviteSubject("{{inviter_name}} invited you to OneCamp") != DefaultInviteSubject ||
		InviteSubject("Join Acme") != "Join Acme" {
		t.Error("subjects")
	}
	// The shipped sender is OneCamp's domain, which a workspace's own key
	// cannot send from; the workspace's own sender is used instead.
	if got := InviteSender("noreply@onemana.dev", "noreply@team.example"); got != "noreply@team.example" {
		t.Errorf("shipped sender: %q", got)
	}
	if got := InviteSender("hello@acme.example", "noreply@team.example"); got != "hello@acme.example" {
		t.Errorf("an admin's sender: %q", got)
	}
}

// It names the inviter and the workspace, safely in the HTML, plainly in the
// subject and the text part.
func TestAnInvitationNamesWhoAndWhere(t *testing.T) {
	e := InvitationEmail{To: "ana@example.test", InviterName: "Sam <b>Rivera</b>", InviterEmail: "sam@example.test"}
	subject, html, text := RenderInvitation(e, DefaultInviteSubject, DefaultInviteTemplate,
		"https://team.example/signup?token=t", "team.example", "")
	// The inviter's name is in the body, never the subject, where a mail
	// client shows whatever a display name says before the email is opened.
	if subject != "You're invited to join team.example on OneCamp" {
		t.Errorf("subject %q", subject)
	}
	if subject, _, _ := RenderInvitation(e, "{{inviter_name}} invited you", DefaultInviteTemplate, "l", "w", ""); subject != "A teammate invited you" {
		t.Errorf("an admin's subject with {{inviter_name}}: %q", subject)
	}
	if !strings.Contains(html, "Sam &lt;b&gt;Rivera&lt;/b&gt; invited you to join them at team.example.") ||
		!strings.Contains(html, `href="https://team.example/signup?token=t"`) || strings.Contains(html, "{{") {
		t.Errorf("html %q", html)
	}
	if !strings.Contains(text, "team.example") || !strings.Contains(text, "https://team.example/signup?token=t") {
		t.Errorf("text %q", text)
	}
	if _, html, _ := RenderInvitation(InvitationEmail{}, DefaultInviteSubject, DefaultInviteTemplate, "l", "w", ""); !strings.Contains(html, "A teammate invited you") {
		t.Errorf("no inviter: %q", html)
	}
}

// Someone holding a link that no longer works is told why, and who can send
// another, rather than "invitation token has expired".
func TestADeadInvitationLinkSaysWhoToAsk(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	cases := []struct {
		inv     *userModels.Invitation
		inviter string
		want    string
	}{
		{nil, "", "This invitation link isn't valid. Ask whoever invited you to send it again."},
		{&userModels.Invitation{Status: InvitationSent, TokenExpiresAt: &future}, "Sam Rivera", ""},
		{&userModels.Invitation{Status: InvitationPending, CreatedAt: now.Add(-time.Hour)}, "", ""},
		{&userModels.Invitation{Status: InvitationPending, CreatedAt: now.Add(-8 * 24 * time.Hour)}, "", "This invitation has expired. Ask whoever invited you to send it again."},
		{&userModels.Invitation{Status: InvitationSent, TokenExpiresAt: &past}, "Sam Rivera", "This invitation has expired. Ask Sam Rivera to send it again."},
		{&userModels.Invitation{Status: InvitationExpired, TokenExpiresAt: &future}, "", "This invitation has expired. Ask whoever invited you to send it again."},
		{&userModels.Invitation{Status: InvitationJoined, TokenExpiresAt: &past}, "Sam Rivera", "This invitation has been used already. Sign in instead."},
	}
	for i, c := range cases {
		if got := InvitationLinkProblem(c.inv, now, c.inviter); got != c.want {
			t.Errorf("case %d: %q, want %q", i, got, c.want)
		}
	}
}

// An invitation names its inviter by their display name, else their full
// name: one rule, for the email and the sign-up page alike.
func TestAnInviterIsNamedOneWay(t *testing.T) {
	for _, c := range []struct {
		user dgraphStruct.DgraphUser
		want string
	}{
		{dgraphStruct.DgraphUser{UserName: " Sam ", UserFullName: "Samantha Rivera"}, "Sam"},
		{dgraphStruct.DgraphUser{UserName: "  ", UserFullName: "Samantha Rivera"}, "Samantha Rivera"},
		{dgraphStruct.DgraphUser{}, ""},
	} {
		if got := NameOnRecord(c.user); got != c.want {
			t.Errorf("NameOnRecord(%+v) = %q, want %q", c.user, got, c.want)
		}
	}
}
