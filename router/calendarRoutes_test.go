package router

import (
	"strings"
	"testing"
)

// Google Calendar links only through the connectors flow, whose one-shot
// state is bound to the person and the provider. Its own callbacks took the
// person's user id as the state, which anyone can read off a profile, so one
// link could tie a teammate's account to the sender's Google (calendar
// CAL-BUG-8). They must not come back.
func TestGoogleCalendarLinksOnlyThroughTheConnectorsFlow(t *testing.T) {
	src := strings.Join(routerSource(t), "\n")
	for _, gone := range []string{
		`"/integration/google-calendar/callback"`,
		`"/google-calendar/callback"`,
		"GoogleCalendarCallback",
	} {
		if strings.Contains(src, gone) {
			t.Errorf("router.go still registers %s", gone)
		}
	}
	for _, kept := range []string{
		`router.Get("/connector/oauth/callback", connectorController.HandleCallback)`,
		`Get("/google-calendar/auth-url", integrationController.GetGoogleCalendarAuthUrl)`,
	} {
		if !strings.Contains(src, kept) {
			t.Errorf("router.go no longer has %s", kept)
		}
	}
}

// Focus time ends on the server, wherever it was started (23 Oct 2026, the
// owner's "End focus button is not working"): the web's End and the phone's
// call this one route.
func TestFocusTimeEndsOnTheServer(t *testing.T) {
	if !strings.Contains(strings.Join(routerSource(t), "\n"), `r.Post("/endFocus", eventController.EndFocusController)`) {
		t.Error("POST /event/endFocus is not registered")
	}
}

// An event opens by its id, in any month (calendar CAL-BUG-7).
func TestOneEventCanBeRead(t *testing.T) {
	if !strings.Contains(strings.Join(routerSource(t), "\n"), `r.Get("/getEvent/{eventId}", eventController.GetEventController)`) {
		t.Error("GET /event/getEvent/{eventId} is not registered")
	}
}
