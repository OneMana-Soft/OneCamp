package business

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// calendarClient.go — per-user Google Calendar operations for the AI. Reuses
// the same calendar integration row as the existing calendar-sync feature
// (provider "google_calendar"), so connecting once serves both.

func calendarService(ctx context.Context, userUUID uuid.UUID) (*calendar.Service, error) {
	access, err := validAccessToken(ctx, userUUID, ProviderCalendar)
	if err != nil {
		return nil, err
	}
	if access == "" {
		return nil, nil
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: access})
	return calendar.NewService(ctx, option.WithTokenSource(ts))
}

// CalendarEvent is a compact view of an event for the AI.
type CalendarEvent struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Location string `json:"location,omitempty"`
	Link     string `json:"link,omitempty"`
}

// CalendarList returns events between now and `days` ahead. Read-only.
func CalendarList(ctx context.Context, userUUID uuid.UUID, days int, max int64) ([]CalendarEvent, error) {
	svc, err := calendarService(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, ErrNotConnected
	}
	if days <= 0 {
		days = 7
	}
	if max <= 0 || max > 50 {
		max = 20
	}
	now := time.Now()
	timeMin := now.Format(time.RFC3339)
	timeMax := now.AddDate(0, 0, days).Format(time.RFC3339)

	res, err := svc.Events.List("primary").
		TimeMin(timeMin).TimeMax(timeMax).
		SingleEvents(true).OrderBy("startTime").
		MaxResults(max).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("calendar list: %w", err)
	}

	out := make([]CalendarEvent, 0, len(res.Items))
	for _, e := range res.Items {
		ce := CalendarEvent{ID: e.Id, Title: e.Summary, Location: e.Location, Link: e.HtmlLink}
		if e.Start != nil {
			ce.Start = helpers.FirstNonEmpty(e.Start.DateTime, e.Start.Date)
		}
		if e.End != nil {
			ce.End = helpers.FirstNonEmpty(e.End.DateTime, e.End.Date)
		}
		out = append(out, ce)
	}
	return out, nil
}

// CalendarCreate creates an event. WRITE — runs only after user confirmation.
// startRFC3339/endRFC3339 must be RFC3339 timestamps.
func CalendarCreate(ctx context.Context, userUUID uuid.UUID, title, startRFC3339, endRFC3339, description string) (string, error) {
	svc, err := calendarService(ctx, userUUID)
	if err != nil {
		return "", err
	}
	if svc == nil {
		return "", ErrNotConnected
	}
	if _, perr := time.Parse(time.RFC3339, startRFC3339); perr != nil {
		return "", fmt.Errorf("invalid start time (need RFC3339)")
	}
	if endRFC3339 == "" {
		// Default to a 30-minute event.
		st, _ := time.Parse(time.RFC3339, startRFC3339)
		endRFC3339 = st.Add(30 * time.Minute).Format(time.RFC3339)
	}
	ev := &calendar.Event{
		Summary:     title,
		Description: description,
		Start:       &calendar.EventDateTime{DateTime: startRFC3339},
		End:         &calendar.EventDateTime{DateTime: endRFC3339},
	}
	created, err := svc.Events.Insert("primary", ev).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("calendar insert: %w", err)
	}
	return created.HtmlLink, nil
}
