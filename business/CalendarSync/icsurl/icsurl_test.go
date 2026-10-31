package icsurl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	"github.com/akashc777/OneCamp/helpers/safefetch"
)

// file is a published calendar: a weekly review with one week moved, and a
// holiday, stamped at stamp (feeds stamp every event with the time of the read).
func file(stamp string) string {
	return strings.Join([]string{"BEGIN:VCALENDAR", "X-WR-CALNAME:Team",
		"BEGIN:VEVENT", "UID:review", "DTSTAMP:" + stamp, "SUMMARY:Review", "DTSTART:20261021T090000Z", "DTEND:20261021T100000Z", "RRULE:FREQ=WEEKLY", "END:VEVENT",
		"BEGIN:VEVENT", "UID:review", "DTSTAMP:" + stamp, "RECURRENCE-ID:20261028T090000Z", "SUMMARY:Review (moved)", "DTSTART:20261028T140000Z", "DTEND:20261028T150000Z", "END:VEVENT",
		"BEGIN:VEVENT", "UID:diwali", "DTSTAMP:" + stamp, "SUMMARY:Diwali", "DTSTART;VALUE=DATE:20261108", "END:VEVENT",
		"END:VCALENDAR", ""}, "\r\n")
}

func answering(body string, status int, truncated bool) adapter {
	return adapter{fetch: func(_ context.Context, rawURL string, opt safefetch.Options) (*safefetch.Response, error) {
		if rawURL != "https://calendar.example/team.ics" || opt.MaxBytes != maxFileBytes {
			return nil, errors.New("asked for the wrong thing")
		}
		return &safefetch.Response{Status: status, Body: []byte(body), Truncated: truncated}, nil
	}}
}

var team = calendarSyncBusiness.Remote{ID: "sub-1", Source: "https://calendar.example/team.ics"}

// Calendar B8: a published calendar read whole, each event a change, its
// instances named for their series; read again unchanged, nothing; stamped
// anew but otherwise unchanged, every event keeps its version.
func TestAnAddressIsReadWholeAndOnlyAgainWhenItChanges(t *testing.T) {
	a := answering(file("20261020T100000Z"), 200, false)
	changes, cursor, err := a.Pull(context.Background(), team, "")
	if err != nil || cursor == "" || len(changes) != 3 {
		t.Fatalf("%d changes, cursor %q, %v", len(changes), cursor, err)
	}
	ids := map[string]string{}
	versions := map[string]string{}
	for _, c := range changes {
		ids[c.ExternalID] = c.SeriesID
		versions[c.ExternalID] = c.ETag
	}
	if _, ok := ids["review"]; !ok || ids["review_20261028T090000Z"] != "review" || ids["diwali"] != "" {
		t.Fatalf("ids %v", ids)
	}
	if again, same, err := a.Pull(context.Background(), team, cursor); err != nil || len(again) != 0 || same != cursor {
		t.Fatalf("read again unchanged: %d changes, %q, %v", len(again), same, err)
	}
	restamped, next, err := answering(file("20261020T110000Z"), 200, false).Pull(context.Background(), team, cursor)
	if err != nil || next == cursor || len(restamped) != 3 {
		t.Fatalf("restamped: %d, %q, %v", len(restamped), next, err)
	}
	for _, c := range restamped {
		if versions[c.ExternalID] != c.ETag {
			t.Errorf("%s changed version when only its stamp did", c.ExternalID)
		}
	}
}

// What can't be read says why, in words; and nothing is ever written back.
func TestAnAddressThatCantBeReadSaysWhy(t *testing.T) {
	for _, c := range []struct {
		a    adapter
		want string
	}{
		{answering("", 404, false), "no calendar at that address"},
		{answering("", 503, false), "read again in 15 minutes"},
		{answering(file("20261020T100000Z"), 200, true), "larger than 10 MB"},
		{answering("<html>Sign in</html>", 200, false), "calendar"},
	} {
		if _, _, err := c.a.Pull(context.Background(), team, ""); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v, want %q", err, c.want)
		}
	}
	if _, err := (adapter{}).Push(context.Background(), calendarSyncBusiness.Op{}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("a push: %v", err)
	}
}

// What can't be read never says the address, which a private calendar's link
// is a secret in: a sign-in asked for, a network that failed with the
// address in its error, a time limit.
func TestWhyAnAddressCantBeReadNeverHoldsIt(t *testing.T) {
	secret := team
	secret.Source = "https://calendar.example/private-7rKq2mX9vN4p/basic.ics"
	for _, c := range []struct {
		fetch fetcher
		want  string
	}{
		{func(context.Context, string, safefetch.Options) (*safefetch.Response, error) {
			return &safefetch.Response{Status: 403}, nil
		}, "asks for a sign-in"},
		{func(_ context.Context, rawURL string, _ safefetch.Options) (*safefetch.Response, error) {
			return nil, fmt.Errorf("Get %q: dial tcp: lookup calendar.example: no such host", rawURL)
		}, "couldn't be reached"},
		{func(context.Context, string, safefetch.Options) (*safefetch.Response, error) {
			return nil, fmt.Errorf("read: %w", context.DeadlineExceeded)
		}, "too long"},
		{func(context.Context, string, safefetch.Options) (*safefetch.Response, error) {
			return nil, fmt.Errorf("dial: %w", safefetch.ErrResolvesInside)
		}, "private network"},
	} {
		_, _, err := adapter{fetch: c.fetch}.Pull(context.Background(), secret, "")
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "private-7rKq2mX9vN4p") || strings.Contains(err.Error(), "calendar.example") {
			t.Errorf("%v, want %q and never the address", err, c.want)
		}
	}
}

// An address as it was pasted: webcal is https; a local or private address,
// or another scheme, is refused before anything is asked.
func TestAnAddressIsCheckedBeforeItIsRead(t *testing.T) {
	if got, err := Address(" webcal://calendar.example/team.ics "); err != nil || got != "https://calendar.example/team.ics" {
		t.Fatalf("webcal: %q %v", got, err)
	}
	for _, bad := range []string{"ftp://calendar.example/team.ics", "http://127.0.0.1/team.ics", "http://localhost/team.ics", "not an address"} {
		if _, err := Address(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}
