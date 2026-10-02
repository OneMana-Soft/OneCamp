package helpers

import (
	"os"
	"strings"
)

// DemoMode reports whether this server is the public demo (DEMO_MODE=true),
// where everyone signs in as the same shared visitor.
func DemoMode() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("DEMO_MODE")), "true")
}

// IsDemoVisitor reports whether email is the shared visitor account of the
// public demo (DEMO_USER_EMAIL on a DEMO_MODE server). Everyone who opens the
// demo signs in as that one account; the people who run the demo sign in as
// themselves and are not affected by anything keyed on this.
func IsDemoVisitor(email string) bool {
	visitor := strings.TrimSpace(os.Getenv("DEMO_USER_EMAIL"))
	return DemoMode() && visitor != "" && strings.EqualFold(strings.TrimSpace(email), visitor)
}

// DemoPersonalAccountMsg is why the demo refuses to connect a personal
// account, in words a visitor can act on.
const DemoPersonalAccountMsg = "The demo is shared by everyone who opens it, so it never connects anyone's own " +
	"Google or GitHub account: the next visitor would see it. Install OneCamp free to try this with yours."
