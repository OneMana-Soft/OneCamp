package business

import (
	"context"
	"encoding/json"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"github.com/akashc777/OneCamp/helpers"
)

// calendarClient.go — the Google Calendar connector. Linking Google Calendar
// goes through this package's OAuth flow, and writes the same integrations row
// (provider "google_calendar") that the calendar's own Google sync reads
// (business/Integration), so connecting once serves both. The assistant reads
// and writes the calendar through business/Calendar, never Google directly.

// calendarAccountMetadata is the address of the Google account just linked
// (the id of its primary calendar), and what the link keeps about it, so the
// calendar can say "Connected as maya@outlook.com". The address is empty when
// Google won't give it, so linking again never shows the account linked
// before. Best effort: the link works without it.
func calendarAccountMetadata(ctx context.Context, accessToken string) (email, metadata string) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: accessToken})
	if svc, err := calendar.NewService(cctx, option.WithTokenSource(ts)); err == nil {
		if cal, gerr := svc.Calendars.Get("primary").Context(cctx).Do(); gerr == nil && cal != nil {
			email = cal.Id
		} else if gerr != nil {
			helpers.LogInfoWithContext(ctx, "connector: the linked Google Calendar account's address couldn't be read: %v", gerr)
		}
	}
	b, _ := json.Marshal(map[string]string{"account_email": email})
	return email, string(b)
}
