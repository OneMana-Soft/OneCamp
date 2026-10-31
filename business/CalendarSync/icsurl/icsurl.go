// Package icsurl is calendar sync's adapter for an address another calendar
// publishes (calendar B8, SY-14): public holidays, a team's calendar in
// another tool, someone's secret link. Each read fetches the file whole
// through SafeFetch (no private or local addresses, a size limit, a time
// limit) and reads it with A4's one reader; the caller takes whatever left
// the file out of the calendar. A file unchanged since the last read changes
// nothing. Read only: OneCamp never writes back to an address.
//
// An address can be a secret (a calendar's private link), so nothing here
// puts it in an error: what can't be read says why in words of its own.
package icsurl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	"github.com/akashc777/OneCamp/helpers/safefetch"
)

const (
	// maxFileBytes is the largest file a subscription reads.
	maxFileBytes = 10 << 20
	// fetchTimeout bounds one read, redirects and body included.
	fetchTimeout = 20 * time.Second
)

// ErrReadOnly is a change asked of a calendar read from an address.
var ErrReadOnly = errors.New("A calendar read from an address can't be changed here.")

type fetcher func(ctx context.Context, rawURL string, opt safefetch.Options) (*safefetch.Response, error)

type adapter struct{ fetch fetcher }

func init() { calendarSyncBusiness.Adapters.Register(adapter{fetch: safefetch.Fetch}) }

func (adapter) Provider() string { return "ics" }

// Calendars: a subscription names its own one calendar.
func (adapter) Calendars(context.Context, calendarSyncBusiness.Link) ([]calendarSyncBusiness.Remote, error) {
	return nil, nil
}

// Address is what someone pasted, as it is read: webcal:// is https://,
// and an address SafeFetch would refuse (a private or local one, a scheme
// other than http and https) is refused here first, in words.
func Address(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if len(s) >= len("webcal://") && strings.EqualFold(s[:len("webcal://")], "webcal://") {
		s = "https://" + s[len("webcal://"):]
	}
	u, err := safefetch.CheckURL(s)
	switch {
	case err != nil && errors.Is(err, safefetch.ErrBlocked):
		return "", errors.New("That address is inside a private network, so it can't be read from here.")
	case err != nil || u.Host == "":
		return "", errors.New("That isn't an address a calendar can be read from: it starts with https://, http:// or webcal://.")
	}
	return u.String(), nil
}

// Read fetches the file at an address whole and reads it: the calendar, and
// the file's digest, which is the cursor Pull compares the next read with.
func Read(ctx context.Context, address string) (ICS.Calendar, string, error) {
	body, digest, err := fetchFile(ctx, safefetch.Fetch, address)
	if err != nil {
		return ICS.Calendar{}, "", err
	}
	file, err := ICS.Read(bytes.NewReader(body))
	return file, digest, err
}

// Pull reads the whole file at the calendar's address: every event in it as
// a change, and its digest as the cursor. A file whose digest is cursor
// changed nothing, and nothing comes back.
func (a adapter) Pull(ctx context.Context, cal calendarSyncBusiness.Remote, cursor string) ([]calendarSyncBusiness.Change, string, error) {
	body, next, err := fetchFile(ctx, a.fetch, cal.Source)
	if err != nil {
		return nil, "", err
	}
	if next == cursor {
		return nil, cursor, nil
	}
	file, err := ICS.Read(bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	return ChangesOf(file), next, nil
}

// fetchFile is the file at an address and its digest, or why it can't be
// read, in words that never hold the address.
func fetchFile(ctx context.Context, fetch fetcher, address string) ([]byte, string, error) {
	res, err := fetch(ctx, address, safefetch.Options{MaxBytes: maxFileBytes, Timeout: fetchTimeout, Accept: "text/calendar, */*;q=0.5"})
	switch {
	case err != nil && errors.Is(err, safefetch.ErrBlocked):
		return nil, "", errors.New("That address is inside a private network, so it can't be read from here.")
	case err != nil && safefetch.Refused(err):
		return nil, "", errors.New("That address can't be read from here.")
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)):
		return nil, "", errors.New("The address took too long to answer.")
	case err != nil:
		return nil, "", errors.New("The address couldn't be reached.")
	case res.Status == http.StatusUnauthorized || res.Status == http.StatusForbidden:
		return nil, "", errors.New("That address asks for a sign-in: use the calendar's public or secret address.")
	case res.Status == http.StatusNotFound || res.Status == http.StatusGone:
		return nil, "", errors.New("There's no calendar at that address. Check it, or copy it again from where it's published.")
	case res.Status >= 500:
		return nil, "", errors.New("The calendar's address couldn't give it just now. It's read again in 15 minutes.")
	case res.Status != http.StatusOK:
		return nil, "", errors.New("That address doesn't give a calendar. Copy it again from where it's published.")
	case res.Truncated:
		return nil, "", fmt.Errorf("The calendar there is larger than %d MB.", maxFileBytes>>20)
	}
	sum := sha256.Sum256(res.Body)
	return res.Body, hex.EncodeToString(sum[:]), nil
}

// ChangesOf is every event of a file as a change: a series or a single event
// by its UID, an instance changed on its own by its series' UID and when it
// was due. Pure.
func ChangesOf(file ICS.Calendar) []calendarSyncBusiness.Change {
	changes := make([]calendarSyncBusiness.Change, 0, len(file.Events))
	for _, ev := range file.Events {
		ch := calendarSyncBusiness.Change{ExternalID: ev.UID, ETag: versionOf(ev), Event: ev}
		if ev.RecurrenceID != nil {
			ch.SeriesID = ev.UID
			ch.ExternalID = ev.UID + "_" + ev.RecurrenceID.UTC().Format("20060102T150405Z")
		}
		changes = append(changes, ch)
	}
	return changes
}

// versionOf is an event's version as the file has it: what it holds, never
// when the file was written (many feeds stamp every event with the time of
// the read), so an event left alone keeps its version. Pure.
func versionOf(ev ICS.Event) string {
	ev.Stamp = time.Time{}
	// The one pointer it holds, by what it points at: an address isn't a version.
	rid := ""
	if ev.RecurrenceID != nil {
		rid = ev.RecurrenceID.UTC().Format(time.RFC3339)
	}
	ev.RecurrenceID = nil
	sum := sha256.Sum256([]byte(fmt.Sprintf("%+v|%s", ev, rid)))
	return hex.EncodeToString(sum[:12])
}

func (adapter) Push(context.Context, calendarSyncBusiness.Op) (calendarSyncBusiness.Ref, error) {
	return calendarSyncBusiness.Ref{}, ErrReadOnly
}

func (adapter) Watch(context.Context, calendarSyncBusiness.Remote) (calendarSyncBusiness.Watch, error) {
	return calendarSyncBusiness.Watch{}, errors.ErrUnsupported
}

func (adapter) Stop(context.Context, calendarSyncBusiness.Watch) error { return errors.ErrUnsupported }
