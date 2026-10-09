package business

import (
	"strings"
	"testing"
	"time"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The link has a scheme. FE_HOST_DOMAIN as make install writes it is a bare
// host, and a bare host inside an href is a relative link to a mail client, so
// every invitation pointed nowhere. Moved here from the controller, so the
// email and the admin's list build it the same way.
func TestInvitationLinkHasAScheme(t *testing.T) {
	if link := invitationLinkOn("https://onecamp.example.com", "tok123"); link != "https://onecamp.example.com/signup?token=tok123" {
		t.Errorf("unexpected link %q", link)
	}
	if got := invitationLinkOn("https://onecamp.example.com/", "t"); strings.Contains(got, "com//") {
		t.Errorf("doubled slash in %q", got)
	}
}

// An invitation used to read "Sent" until someone opened its dead link. Its
// state is now worked out when it is read: expired once its link has run out,
// with the days left and the link to copy while it can still be used, and
// nothing to copy once it is expired or used.
func TestAnInvitationSaysWhereItStands(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	token := "tok"
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	link := func(tok string) string { return "https://team.example/signup?token=" + tok }
	inv := func(status string, expires *time.Time) *userModels.Invitation {
		return &userModels.Invitation{Id: uuid.New(), Email: "ana@example.test", Status: status, Token: &token, TokenExpiresAt: expires, CreatedAt: now.Add(-time.Hour)}
	}
	cases := []struct {
		name     string
		inv      *userModels.Invitation
		status   string
		daysLeft int // 0: none shown
		link     bool
	}{
		{"fresh", inv(InvitationSent, at(7*24*time.Hour)), InvitationSent, 7, true},
		{"three hours left", inv(InvitationSent, at(3*time.Hour)), InvitationSent, 1, true},
		{"a day and a bit", inv(InvitationSent, at(25*time.Hour)), InvitationSent, 2, true},
		{"run out, row still says sent", inv(InvitationSent, at(-time.Minute)), InvitationExpired, 0, false},
		{"run out exactly now", inv(InvitationSent, at(0)), InvitationExpired, 0, false},
		{"from before tokens, no expiry, made an hour ago", inv("", nil), InvitationPending, 7, true},
		{"used", inv(InvitationJoined, at(time.Hour)), InvitationJoined, 0, false},
		{"marked expired by the sign-up page", inv(InvitationExpired, at(time.Hour)), InvitationExpired, 0, false},
	}
	for _, c := range cases {
		v := InvitationViewAt(c.inv, now, link)
		if v.Status != c.status {
			t.Errorf("%s: status %q, want %q", c.name, v.Status, c.status)
		}
		got := 0
		if v.ExpiresInDays != nil {
			got = *v.ExpiresInDays
		}
		if got != c.daysLeft {
			t.Errorf("%s: %d days left, want %d", c.name, got, c.daysLeft)
		}
		if (v.InviteLink != "") != c.link {
			t.Errorf("%s: link %q, want one: %v", c.name, v.InviteLink, c.link)
		}
	}
}

// An invitation made before links had an expiry lasts a week from when it was
// made, in the list and at the sign-up link alike (the one predicate,
// userModels.Invitation.LiveAt). The list called it live forever.
func TestAnInvitationWithoutAnExpiryLastsAWeek(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	token := "legacy"
	old := &userModels.Invitation{Id: uuid.New(), Email: "old@example.test", Status: InvitationPending, Token: &token, CreatedAt: now.Add(-8 * 24 * time.Hour)}
	if view := InvitationViewAt(old, now, invitationLinkOnTest); view.Status != InvitationExpired || view.InviteLink != "" {
		t.Errorf("eight days old, no expiry: %+v", view)
	}
	if problem := InvitationLinkProblem(old, now, "Sam"); problem != "This invitation has expired. Ask Sam to send it again." {
		t.Errorf("its link: %q", problem)
	}
	recent := &userModels.Invitation{Id: uuid.New(), Email: "new@example.test", Status: InvitationPending, Token: &token, CreatedAt: now.Add(-2 * 24 * time.Hour)}
	view := InvitationViewAt(recent, now, invitationLinkOnTest)
	if view.Status != InvitationPending || view.ExpiresInDays == nil || *view.ExpiresInDays != 5 || view.ExpiresAt == nil {
		t.Errorf("two days old, no expiry: %+v", view)
	}
}

func invitationLinkOnTest(token string) string {
	return "https://team.example.test/signup?token=" + token
}
