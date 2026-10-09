package authService

// Where someone who has just joined starts.
//
// A new member is put in the workspace's default channels as they join
// (business/Channel.JoinDefaultChannels), and opens on the one they land in,
// with the message box ready, rather than on an empty Home. Everyone else
// keeps Home.

import (
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// NewMemberLanding is the web app path a new member opens on: the channel,
// with compose=1, which the channel page reads as "focus the message box"
// (lib/landing.ts in the web app). "" for no channel. Pure.
func NewMemberLanding(channelID uuid.UUID) string {
	if channelID == uuid.Nil {
		return ""
	}
	return "/app/channel/" + channelID.String() + "?compose=1"
}

// LandingAfterSignIn is where a sign-in that ends in a redirect sends someone:
// a new member's landing instead of Home, when the sign-in was headed for
// Home. A more specific destination they asked for is kept, and target is
// returned unchanged when there is no landing.
func LandingAfterSignIn(target string, landing uuid.UUID) string {
	if landing == uuid.Nil || !IsAppHome(target) {
		return target
	}
	return FrontendBaseURL() + NewMemberLanding(landing)
}

// IsAppHome reports whether target is the web app's Home on its own origin:
// /app, /app/ or /app/home, with nothing more specific asked for.
func IsAppHome(target string) bool {
	if !IsRedirectAllowed(target) {
		return false
	}
	u, err := url.Parse(target)
	if err != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	path := strings.TrimRight(u.Path, "/")
	return path == "/app" || path == "/app/home"
}
