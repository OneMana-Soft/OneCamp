package authService

import (
	"testing"

	"github.com/google/uuid"
)

// A new member is sent to the channel they were put in, with the message box
// ready, instead of Home. Anything more specific they were headed for, or a
// sign-in with no landing, is left alone.
func TestANewMemberLandsInTheirChannel(t *testing.T) {
	t.Setenv("FRONTEND_DOMAIN", "https://team.example.com")
	ch := uuid.MustParse("6f1c3a52-9d3e-4c9e-a7b1-2f0e5d4c3b2a")
	want := "https://team.example.com/app/channel/6f1c3a52-9d3e-4c9e-a7b1-2f0e5d4c3b2a?compose=1"
	for _, home := range []string{"https://team.example.com/app", "https://team.example.com/app/", "https://team.example.com/app/home"} {
		if got := LandingAfterSignIn(home, ch); got != want {
			t.Errorf("LandingAfterSignIn(%q) = %q, want %q", home, got, want)
		}
	}
	for _, kept := range []string{
		"https://team.example.com/app/doc/123",
		"https://team.example.com/app?open=x",
		"https://elsewhere.example/app",
	} {
		if got := LandingAfterSignIn(kept, ch); got != kept {
			t.Errorf("LandingAfterSignIn(%q) = %q, want it kept", kept, got)
		}
	}
	if got := LandingAfterSignIn("https://team.example.com/app", uuid.Nil); got != "https://team.example.com/app" {
		t.Errorf("a member signing in again was sent to %q", got)
	}
	if NewMemberLanding(uuid.Nil) != "" {
		t.Error("no channel has a landing")
	}
}
