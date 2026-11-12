// Package microsoft is Outlook and Microsoft 365 as a calendar sync
// provider (calendar B7, #42), through Microsoft Graph v1.0: the calendars of
// a person's Microsoft link, each calendar's events read through a delta of
// its calendar view, and OneCamp's changes made there, so the Syncer
// (business/Calendar syncGoogle.go) keeps both sides in step as it does
// Google's.
//
// Graph's v1.0 delta is of a calendar view: a window of instances, series
// expanded (https://learn.microsoft.com/en-us/graph/api/event-delta?view=graph-rest-1.0;
// events/delta, which has masters, is beta only). So a series is read once
// by its master, whose pattern is its rule, whose cancelled occurrences are
// the dates it skips, and whose exceptions are its instances changed on
// their own; and the series a pull has seen are kept with the cursor, as the
// delta names only an occurrence's id when a series or one of its
// occurrences goes.
package microsoft

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	"github.com/google/uuid"
)

func init() {
	calendarSyncBusiness.Adapters.Register(Adapter{Base: "https://graph.microsoft.com/v1.0", Token: linkedToken,
		Client: &http.Client{Timeout: 30 * time.Second}, Now: time.Now, Sleep: sleep})
}

// How much a pull reads, how long Graph is waited for, and the window.
const (
	maxPullEvents  = 50000
	maxSeriesReads = 500
	pageSize       = 200
	maxTries       = 4
	maxWait        = 30 * time.Second
	// The window a calendar's delta covers, from windowBack ago; a window
	// older than windowRoll starts over, so it moves on with time.
	windowBack   = 180 * 24 * time.Hour
	windowLength = 2
	windowRoll   = 30 * 24 * time.Hour
	// maxExDatesPushed bounds the occurrences one push deletes for the dates
	// a series skips.
	maxExDatesPushed = 20
)

// errNoLink is a sync asked of someone whose Microsoft link is gone.
var errNoLink error = words("The Microsoft account isn't linked any more: connect it again from Calendars.")

// words is an error a person reads as it is, a sentence.
type words string

func (w words) Error() string { return string(w) }

// Adapter is Microsoft Graph as calendar sync's "microsoft" provider. Its
// fields are where Graph is and how a person's token is read; tests point
// them at a fake Graph.
type Adapter struct {
	// Base is Graph's v1.0 address; only addresses under it are asked.
	Base string
	// Token is a person's access token, refreshed when it has expired; ""
	// when they have no link.
	Token  func(ctx context.Context, person string) (string, error)
	Client *http.Client
	Now    func() time.Time
	// Sleep waits as Graph asked (429 and 503 with Retry-After).
	Sleep func(ctx context.Context, d time.Duration) error
}

// linkedToken is the person's Microsoft Calendar connector token.
func linkedToken(ctx context.Context, person string) (string, error) {
	id, err := uuid.Parse(person)
	if err != nil {
		return "", nil
	}
	return connectorBusiness.AccessToken(ctx, id, connectorBusiness.ProviderOutlookCalendar)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (Adapter) Provider() string { return "microsoft" }

// graphError is what Graph answered other than success.
type graphError struct {
	Status int
	Code   string
}

func (e *graphError) Error() string {
	return fmt.Sprintf("microsoft graph: %d %s", e.Status, e.Code)
}

// graphs says when an address isn't Graph's: none but Graph's is asked or
// kept, so a link in an answer or a cursor can't send the token elsewhere.
func (a Adapter) graphs(address string) error {
	if !strings.HasPrefix(address, a.Base+"/") {
		return fmt.Errorf("microsoft graph: %q isn't Graph's", address)
	}
	return nil
}

// errRevoked is the link Microsoft no longer answers for, in words.
var errRevoked = fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRevoked, errNoLink)

// call asks Graph once per try, waiting as it says when throttled or busy,
// and reads its answer into out. Only Graph's own address is asked, so a
// cursor can't send the token anywhere else.
func (a Adapter) call(ctx context.Context, person, method, address string, body, out any) error {
	if err := a.graphs(address); err != nil {
		return err
	}
	token, err := a.Token(ctx, person)
	if err != nil {
		if strings.Contains(err.Error(), "invalid_grant") {
			return errRevoked // the refresh token is revoked or expired
		}
		return err // down for now: tried again later
	}
	if token == "" {
		return errRevoked
	}
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for try := 1; ; try++ {
		req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Prefer", `outlook.timezone="UTC", odata.maxpagesize=`+strconv.Itoa(pageSize))
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := a.Client.Do(req)
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout:
			if try >= maxTries {
				return &graphError{Status: resp.StatusCode, Code: codeOf(raw)}
			}
			if err := a.Sleep(ctx, retryAfter(resp.Header.Get("Retry-After"))); err != nil {
				return err
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized:
			return errRevoked // a fresh token refused: the consent is gone
		case resp.StatusCode >= 300:
			return &graphError{Status: resp.StatusCode, Code: codeOf(raw)}
		}
		if out != nil && len(raw) > 0 {
			return json.Unmarshal(raw, out)
		}
		return nil
	}
}

// retryAfter is the wait Graph named, in seconds, within maxWait. Pure.
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return time.Second
	}
	return min(time.Duration(n)*time.Second, maxWait)
}

func codeOf(raw []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Error.Code
}

// syncErr is a Graph failure as calendar sync tells them apart: a calendar
// the link may only read, and a change Graph won't take as it is. Anything
// else (throttled, down) stays as it is and is tried again later. Pure.
func syncErr(err error) error {
	var gerr *graphError
	if err == nil || !errors.As(err, &gerr) {
		return err
	}
	switch gerr.Status {
	case http.StatusForbidden:
		return fmt.Errorf("%w: %w", calendarSyncBusiness.ErrReadOnly, err)
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusGone, http.StatusPreconditionFailed:
		return fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRefused, err)
	}
	return err
}

// Calendars are the mailbox's calendars, its default first (named by the
// account), each in the mailbox's zone, and read-only when the link can't
// change it.
func (a Adapter) Calendars(ctx context.Context, link calendarSyncBusiness.Link) ([]calendarSyncBusiness.Remote, error) {
	var tz struct {
		Value string `json:"value"`
	}
	// A mailbox whose settings can't be read keeps each event's own zones.
	if err := a.call(ctx, link.PersonUUID, http.MethodGet, a.Base+"/me/mailboxSettings/timeZone", nil, &tz); errors.Is(err, calendarSyncBusiness.ErrRevoked) {
		return nil, err
	}
	zone := ianaZone(tz.Value)
	var out []calendarSyncBusiness.Remote
	next := a.Base + "/me/calendars?$select=id,name,canEdit,isDefaultCalendar"
	for next != "" {
		var page struct {
			Value []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				CanEdit   bool   `json:"canEdit"`
				IsDefault bool   `json:"isDefaultCalendar"`
			} `json:"value"`
			Next string `json:"@odata.nextLink"`
		}
		if err := a.call(ctx, link.PersonUUID, http.MethodGet, next, nil, &page); err != nil {
			return nil, syncErr(err)
		}
		for _, c := range page.Value {
			r := calendarSyncBusiness.Remote{Link: link, ID: c.ID, Name: c.Name, Primary: c.IsDefault, Zone: zone, ReadOnly: !c.CanEdit}
			if r.Primary {
				if link.Account != "" {
					r.Name = link.Account
				}
				out = append([]calendarSyncBusiness.Remote{r}, out...)
			} else {
				out = append(out, r)
			}
		}
		next = page.Next
	}
	return out, nil
}

// pullCursor is where a calendar's pull got to: its window's start, Graph's
// deltaLink, and the series it has seen.
type pullCursor struct {
	From   time.Time `json:"from"`
	Delta  string    `json:"delta"`
	Series []string  `json:"series,omitempty"`
}

// masterFields are what a series' master is read with: its occurrences
// cancelled and changed on their own come only when named.
const masterFields = "$select=id,iCalUId,changeKey,type,subject,body,start,end,originalStartTimeZone,isAllDay,isCancelled,showAs,sensitivity,isReminderOn,reminderMinutesBeforeStart,location,organizer,attendees,onlineMeeting,lastModifiedDateTime,recurrence,cancelledOccurrences,exceptionOccurrences&$expand=exceptionOccurrences"

// Pull is what changed in a calendar since cursor ("" for everything), and
// the cursor to ask from next. A cursor Graph no longer knows, one that
// isn't Graph's, or a window gone stale is ErrFullPull.
func (a Adapter) Pull(ctx context.Context, cal calendarSyncBusiness.Remote, cursor string) ([]calendarSyncBusiness.Change, string, error) {
	now := a.Now().UTC()
	var cur pullCursor
	next := ""
	if cursor == "" {
		from := now.Add(-windowBack).Truncate(24 * time.Hour)
		cur.From = from
		q := url.Values{"startDateTime": {from.Format(time.RFC3339)}, "endDateTime": {from.AddDate(windowLength, 0, 0).Format(time.RFC3339)}}
		next = a.Base + "/me/calendars/" + url.PathEscape(cal.ID) + "/calendarView/delta?" + q.Encode()
	} else {
		if err := json.Unmarshal([]byte(cursor), &cur); err != nil || a.graphs(cur.Delta) != nil || now.Sub(cur.From.Add(windowBack)) > windowRoll {
			return nil, "", calendarSyncBusiness.ErrFullPull
		}
		next = cur.Delta
	}
	var out []calendarSyncBusiness.Change
	var series []string
	removed := false
	for next != "" {
		var page struct {
			Value []graphEvent `json:"value"`
			Next  string       `json:"@odata.nextLink"`
			Delta string       `json:"@odata.deltaLink"`
		}
		err := a.call(ctx, cal.Link.PersonUUID, http.MethodGet, next, nil, &page)
		var gerr *graphError
		if errors.As(err, &gerr) && (gerr.Status == http.StatusGone || slices.Contains([]string{"syncstatenotfound", "resyncrequired", "syncstateinvalid"}, strings.ToLower(gerr.Code))) {
			return nil, "", calendarSyncBusiness.ErrFullPull
		}
		if err != nil {
			return nil, "", syncErr(err)
		}
		for _, g := range page.Value {
			switch {
			case len(out) >= maxPullEvents:
			case g.Removed != nil:
				removed = true
				out = append(out, calendarSyncBusiness.Change{ExternalID: g.ID, Deleted: true})
			case g.SeriesMasterID != "":
				if !slices.Contains(series, g.SeriesMasterID) {
					series = append(series, g.SeriesMasterID)
				}
			case g.Type == "seriesMaster":
				if !slices.Contains(series, g.ID) {
					series = append(series, g.ID)
				}
			default:
				out = append(out, calendarSyncBusiness.Change{ExternalID: g.ID, ETag: g.ChangeKey, Event: eventOf(g, cal.Zone)})
			}
		}
		next = page.Next
		if page.Delta != "" {
			if err := a.graphs(page.Delta); err != nil {
				return nil, "", err
			}
			cur.Delta = page.Delta
		}
	}
	// A removal may be of a whole series or one of its occurrences: every
	// series seen is read again.
	if removed {
		for _, id := range cur.Series {
			if !slices.Contains(series, id) {
				series = append(series, id)
			}
		}
	}
	for i, id := range series {
		if i >= maxSeriesReads {
			break
		}
		changes, err := a.readSeries(ctx, cal, id)
		if err != nil && goneIsDone(err) == nil {
			out = append(out, calendarSyncBusiness.Change{ExternalID: id, Deleted: true}) // gone
			cur.Series = slices.DeleteFunc(cur.Series, func(s string) bool { return s == id })
			continue
		}
		if err != nil {
			return nil, "", syncErr(err)
		}
		out = append(out, changes...)
		if !slices.Contains(cur.Series, id) {
			cur.Series = append(cur.Series, id)
		}
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return nil, "", err
	}
	return out, string(b), nil
}

// readSeries is a series as changes: its master, with its rule and the dates
// it skips, then each instance changed on its own, carrying the series' UID.
func (a Adapter) readSeries(ctx context.Context, cal calendarSyncBusiness.Remote, id string) ([]calendarSyncBusiness.Change, error) {
	var m graphEvent
	if err := a.call(ctx, cal.Link.PersonUUID, http.MethodGet, a.Base+"/me/events/"+url.PathEscape(id)+"?"+masterFields, nil, &m); err != nil {
		return nil, err
	}
	master := eventOf(m, cal.Zone)
	// The master's version, and the dates it skips: an occurrence
	// cancelled may leave the master's own changeKey as it was.
	cancelled := slices.Clone(m.CancelledOccurrences)
	slices.Sort(cancelled)
	sum := sha256.Sum256([]byte(strings.Join(cancelled, "\n")))
	out := []calendarSyncBusiness.Change{{ExternalID: m.ID, ETag: m.ChangeKey + "|" + hex.EncodeToString(sum[:4]), Event: master}}
	for _, x := range m.ExceptionOccurrences {
		ev := eventOf(x, cal.Zone)
		ev.UID = master.UID
		out = append(out, calendarSyncBusiness.Change{ExternalID: x.ID, ETag: x.ChangeKey, SeriesID: m.ID, Event: ev})
	}
	return out, nil
}

// Push makes one of OneCamp's changes in Outlook: an event made, changed or
// deleted, or one instance of a series changed or cancelled, found by its
// original start. A series edit is made on its master. Guests aren't sent:
// OneCamp tells them itself.
func (a Adapter) Push(ctx context.Context, op calendarSyncBusiness.Op) (calendarSyncBusiness.Ref, error) {
	ref, err := a.push(ctx, op)
	return ref, syncErr(err)
}

func (a Adapter) push(ctx context.Context, op calendarSyncBusiness.Op) (calendarSyncBusiness.Ref, error) {
	person := op.Remote.Link.PersonUUID
	var made struct {
		ID        string `json:"id"`
		ChangeKey string `json:"changeKey"`
	}
	if op.Kind == "delete" {
		err := a.call(ctx, person, http.MethodDelete, a.Base+"/me/events/"+url.PathEscape(op.ExternalID), nil, nil)
		return calendarSyncBusiness.Ref{ExternalID: op.ExternalID}, goneIsDone(err)
	}
	if op.Kind != "create" && op.Kind != "update" {
		return calendarSyncBusiness.Ref{}, fmt.Errorf("calendar sync: no such change as %q", op.Kind)
	}
	ev, err := graphEventOf(op.Event)
	if err != nil {
		return calendarSyncBusiness.Ref{}, fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRefused, err)
	}
	if rid := op.Event.RecurrenceID; rid != nil && op.ExternalID != "" {
		occ, err := a.occurrence(ctx, person, op.ExternalID, *rid, op.Event.StartDate != "")
		if err != nil {
			return calendarSyncBusiness.Ref{}, err
		}
		if strings.EqualFold(op.Event.Status, "CANCELLED") {
			if occ == "" {
				return calendarSyncBusiness.Ref{ExternalID: op.ExternalID}, nil // not there already
			}
			err := a.call(ctx, person, http.MethodDelete, a.Base+"/me/events/"+url.PathEscape(occ), nil, nil)
			return calendarSyncBusiness.Ref{ExternalID: op.ExternalID}, goneIsDone(err)
		}
		if occ == "" {
			return calendarSyncBusiness.Ref{}, &graphError{Status: http.StatusNotFound, Code: "occurrenceNotFound"}
		}
		if err := a.call(ctx, person, http.MethodPatch, a.Base+"/me/events/"+url.PathEscape(occ), ev, &made); err != nil {
			return calendarSyncBusiness.Ref{}, err
		}
		return calendarSyncBusiness.Ref{ExternalID: op.ExternalID, ETag: made.ChangeKey}, nil
	}
	if op.Kind == "create" {
		err = a.call(ctx, person, http.MethodPost, a.Base+"/me/calendars/"+url.PathEscape(op.Remote.ID)+"/events", ev, &made)
	} else {
		made.ID = op.ExternalID
		err = a.call(ctx, person, http.MethodPatch, a.Base+"/me/events/"+url.PathEscape(op.ExternalID), ev, &made)
	}
	if err != nil {
		return calendarSyncBusiness.Ref{}, err
	}
	// The dates a series skips are its occurrences deleted there.
	if op.Event.RRule != "" {
		now := a.Now()
		pushed := 0
		for _, d := range op.Event.ExDates {
			if d.Before(now.Add(-24*time.Hour)) || pushed >= maxExDatesPushed {
				continue
			}
			pushed++
			if occ, err := a.occurrence(ctx, person, made.ID, d, op.Event.StartDate != ""); err == nil && occ != "" {
				_ = goneIsDone(a.call(ctx, person, http.MethodDelete, a.Base+"/me/events/"+url.PathEscape(occ), nil, nil))
			}
		}
	}
	return calendarSyncBusiness.Ref{ExternalID: made.ID, ETag: made.ChangeKey}, nil
}

// occurrence is the id of a series' instance whose original start is rid
// (its day, for an all-day series), or "" when it has none then.
func (a Adapter) occurrence(ctx context.Context, person, master string, rid time.Time, allDay bool) (string, error) {
	q := url.Values{"startDateTime": {rid.Add(-24 * time.Hour).UTC().Format(time.RFC3339)}, "endDateTime": {rid.Add(48 * time.Hour).UTC().Format(time.RFC3339)},
		"$select": {"id,originalStart"}}
	var page struct {
		Value []struct {
			ID            string `json:"id"`
			OriginalStart string `json:"originalStart"`
		} `json:"value"`
	}
	if err := a.call(ctx, person, http.MethodGet, a.Base+"/me/events/"+url.PathEscape(master)+"/instances?"+q.Encode(), nil, &page); err != nil {
		return "", err
	}
	for _, o := range page.Value {
		t, err := time.Parse(time.RFC3339, o.OriginalStart)
		if err != nil {
			continue
		}
		if t.Equal(rid) || (allDay && t.UTC().Format("2006-01-02") == rid.UTC().Format("2006-01-02")) {
			return o.ID, nil
		}
	}
	return "", nil
}

// goneIsDone is a deletion of what is gone there already, done.
func goneIsDone(err error) error {
	var gerr *graphError
	if errors.As(err, &gerr) && (gerr.Status == http.StatusNotFound || gerr.Status == http.StatusGone) {
		return nil
	}
	return err
}

// Watch isn't asked of Microsoft yet: a subscription needs an address Graph
// can reach, which a server behind a firewall hasn't, so the Syncer asks each
// calendar for its changes every minute instead, as it does Google's.
func (Adapter) Watch(context.Context, calendarSyncBusiness.Remote) (calendarSyncBusiness.Watch, error) {
	return calendarSyncBusiness.Watch{}, errors.ErrUnsupported
}

func (Adapter) Stop(context.Context, calendarSyncBusiness.Watch) error {
	return errors.ErrUnsupported
}
