package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	ICS "github.com/akashc777/OneCamp/business/ICS"
)

// fakeGraph answers each "METHOD /path" with the next of its replies (the
// last repeats), and keeps what it was asked.
type fakeGraph struct {
	mu      sync.Mutex
	srv     *httptest.Server
	replies map[string][]reply
	asked   []string
	bodies  map[string]string
}

type reply struct {
	status int
	body   string
	header map[string]string
}

func newFakeGraph(t *testing.T) *fakeGraph {
	f := &fakeGraph{replies: map[string][]reply{}, bodies: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		f.asked = append(f.asked, key+"?"+r.URL.RawQuery)
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"InvalidAuthenticationToken"}}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.bodies[key] = string(b)
		rs := f.replies[key]
		if len(rs) == 0 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"ErrorItemNotFound"}}`)
			return
		}
		rp := rs[0]
		if len(rs) > 1 {
			f.replies[key] = rs[1:]
		}
		for k, v := range rp.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		if rp.status == 0 {
			rp.status = http.StatusOK
		}
		w.WriteHeader(rp.status)
		_, _ = io.WriteString(w, strings.ReplaceAll(rp.body, "{base}", f.srv.URL))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) on(key string, rs ...reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[key] = append(f.replies[key], rs...)
}

func (f *fakeGraph) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.asked
	f.asked = nil
	return out
}

// adapterOn is the real adapter against f, signed in as "tok", its waits
// kept rather than slept.
func adapterOn(f *fakeGraph, waits *[]time.Duration) Adapter {
	return Adapter{Base: f.srv.URL, Token: func(context.Context, string) (string, error) { return "tok", nil },
		Client: f.srv.Client(), Now: func() time.Time { return at("2026-11-05T12:00:00Z") },
		Sleep: func(_ context.Context, d time.Duration) error { *waits = append(*waits, d); return nil }}
}

var cal1 = calendarSyncBusiness.Remote{Link: calendarSyncBusiness.Link{PersonUUID: "p1", Provider: "microsoft"}, ID: "c1", Zone: "Europe/London"}

const single = `{"id":"e1","iCalUId":"uid-1","changeKey":"k1","type":"singleInstance","subject":"Plan","start":{"dateTime":"2026-11-10T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-10T10:00:00","timeZone":"UTC"},"originalStartTimeZone":"UTC"}`

// A pull pages through the delta to its deltaLink, which is where the next
// pull starts; only Graph's own address is followed.
func TestAPullPagesThroughTheDeltaAndStartsNextFromItsDeltaLink(t *testing.T) {
	f := newFakeGraph(t)
	var waits []time.Duration
	a := adapterOn(f, &waits)
	f.on("GET /me/calendars/c1/calendarView/delta",
		reply{body: `{"value":[` + single + `],"@odata.nextLink":"{base}/me/calendars/c1/calendarView/delta?$skiptoken=p2"}`},
		reply{body: `{"value":[{"id":"e2","iCalUId":"uid-2","changeKey":"k2","type":"singleInstance","subject":"Lunch","isAllDay":true,"start":{"dateTime":"2026-11-11T00:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-12T00:00:00","timeZone":"UTC"}}],"@odata.deltaLink":"{base}/me/calendars/c1/calendarView/delta?$deltatoken=d1"}`},
		reply{body: `{"value":[{"id":"e1","@removed":{"reason":"deleted"}}],"@odata.deltaLink":"{base}/me/calendars/c1/calendarView/delta?$deltatoken=d2"}`})
	changes, cursor, err := a.Pull(context.Background(), cal1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].ExternalID != "e1" || changes[0].ETag != "k1" || changes[1].Event.StartDate != "2026-11-11" {
		t.Fatalf("changes %+v", changes)
	}
	asked := f.log()
	if len(asked) != 2 || !strings.Contains(asked[0], "startDateTime=2026-05-09T00%3A00%3A00Z") || !strings.Contains(asked[0], "endDateTime=2028-05-09T00%3A00%3A00Z") || !strings.Contains(asked[1], "skiptoken=p2") {
		t.Fatalf("asked %v", asked)
	}
	if !strings.Contains(cursor, "deltatoken=d1") {
		t.Fatalf("cursor %q", cursor)
	}
	changes, next, err := a.Pull(context.Background(), cal1, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].ExternalID != "e1" || !changes[0].Deleted {
		t.Fatalf("after the delta link: %+v", changes)
	}
	if asked := f.log(); len(asked) != 1 || !strings.Contains(asked[0], "deltatoken=d1") || !strings.Contains(next, "deltatoken=d2") {
		t.Fatalf("asked %v, next %q", asked, next)
	}
	// A cursor naming anywhere but Graph is never followed: it starts over.
	evil := strings.ReplaceAll(cursor, f.srv.URL, "https://evil.example")
	if _, _, err := a.Pull(context.Background(), cal1, evil); !errors.Is(err, calendarSyncBusiness.ErrFullPull) {
		t.Fatalf("a foreign cursor: %v", err)
	}
}

// A page whose next or delta link points anywhere but Graph fails the pull,
// and that server is never asked: the person's token never leaves Graph.
func TestALinkOffGraphFailsThePullAndIsNeverAsked(t *testing.T) {
	for _, link := range []string{"@odata.nextLink", "@odata.deltaLink"} {
		var mu sync.Mutex
		var foreign []string
		elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			foreign = append(foreign, r.URL.String()+" with "+r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"value":[]}`)
		}))
		t.Cleanup(elsewhere.Close)
		f := newFakeGraph(t)
		var waits []time.Duration
		a := adapterOn(f, &waits)
		f.on("GET /me/calendars/c1/calendarView/delta", reply{body: `{"value":[` + single + `],"` + link + `":"` + elsewhere.URL + `/me/calendars/c1/calendarView/delta?$deltatoken=d1"}`})
		changes, cursor, err := a.Pull(context.Background(), cal1, "")
		if err == nil {
			t.Fatalf("%s off Graph: the pull took %d changes and the cursor %q", link, len(changes), cursor)
		}
		mu.Lock()
		asked := foreign
		mu.Unlock()
		if len(asked) != 0 {
			t.Fatalf("%s off Graph was asked: %v", link, asked)
		}
	}
}

// A delta Graph no longer knows asks for a full pull, by status or by code.
func TestAGoneDeltaAsksForAFullPull(t *testing.T) {
	for _, rp := range []reply{{status: http.StatusGone, body: `{"error":{"code":"SyncStateNotFound"}}`}, {status: http.StatusBadRequest, body: `{"error":{"code":"resyncRequired"}}`}} {
		f := newFakeGraph(t)
		var waits []time.Duration
		a := adapterOn(f, &waits)
		f.on("GET /me/calendars/c1/calendarView/delta", rp)
		cursor := `{"from":"2026-11-01T00:00:00Z","delta":"` + f.srv.URL + `/me/calendars/c1/calendarView/delta?$deltatoken=old"}`
		if _, _, err := a.Pull(context.Background(), cal1, cursor); !errors.Is(err, calendarSyncBusiness.ErrFullPull) {
			t.Fatalf("%d: %v", rp.status, err)
		}
	}
}

// Throttled or busy, Graph is asked again after the wait it named.
func TestThrottlingWaitsAsGraphSaysThenAsksAgain(t *testing.T) {
	f := newFakeGraph(t)
	var waits []time.Duration
	a := adapterOn(f, &waits)
	f.on("GET /me/calendars/c1/calendarView/delta",
		reply{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "7"}, body: `{"error":{"code":"ApplicationThrottled"}}`},
		reply{status: http.StatusServiceUnavailable, header: map[string]string{"Retry-After": "2"}},
		reply{body: `{"value":[` + single + `],"@odata.deltaLink":"{base}/d?$deltatoken=d1"}`})
	changes, _, err := a.Pull(context.Background(), cal1, "")
	if err != nil || len(changes) != 1 {
		t.Fatalf("%v %+v", err, changes)
	}
	if len(waits) != 2 || waits[0] != 7*time.Second || waits[1] != 2*time.Second {
		t.Fatalf("waits %v", waits)
	}
	// Still throttled after its tries, it is left for the next sweep: not a refusal, not a revoked link.
	f.on("GET /me/calendars/c1/calendarView/delta", reply{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "1"}})
	f.replies["GET /me/calendars/c1/calendarView/delta"] = []reply{{status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "1"}}}
	_, _, err = a.Pull(context.Background(), cal1, "")
	if err == nil || errors.Is(err, calendarSyncBusiness.ErrRefused) || errors.Is(err, calendarSyncBusiness.ErrRevoked) {
		t.Fatalf("still throttled: %v", err)
	}
}

// A consent revoked at Microsoft breaks the link, in words.
func TestARevokedConsentSaysTheAccountIsntLinked(t *testing.T) {
	f := newFakeGraph(t)
	var waits []time.Duration
	a := adapterOn(f, &waits)
	a.Token = func(context.Context, string) (string, error) { return "stale", nil }
	_, _, err := a.Pull(context.Background(), cal1, "")
	if !errors.Is(err, calendarSyncBusiness.ErrRevoked) || !strings.Contains(err.Error(), "The Microsoft account isn't linked any more: connect it again from Calendars.") {
		t.Fatalf("a 401: %v", err)
	}
	for _, tok := range []func(context.Context, string) (string, error){
		func(context.Context, string) (string, error) { return "", nil },
		func(context.Context, string) (string, error) {
			return "", errors.New(`refresh token: token endpoint 400: {"error":"invalid_grant"}`)
		},
	} {
		a.Token = tok
		if _, err := a.Push(context.Background(), calendarSyncBusiness.Op{Kind: "delete", Remote: cal1, ExternalID: "e1"}); !errors.Is(err, calendarSyncBusiness.ErrRevoked) {
			t.Fatalf("no token: %v", err)
		}
	}
	// A refresh that fails for now is tried again later, not a broken link.
	a.Token = func(context.Context, string) (string, error) {
		return "", errors.New("refresh token: dial tcp: i/o timeout")
	}
	if _, err := a.Push(context.Background(), calendarSyncBusiness.Op{Kind: "delete", Remote: cal1, ExternalID: "e1"}); err == nil || errors.Is(err, calendarSyncBusiness.ErrRevoked) {
		t.Fatalf("a refresh down for now: %v", err)
	}
}

// Push makes, changes and deletes; guests are left to OneCamp; a calendar
// shared to read refuses as read-only; a deletion already gone is done.
func TestPushMakesChangesAndDeletes(t *testing.T) {
	f := newFakeGraph(t)
	var waits []time.Duration
	a := adapterOn(f, &waits)
	f.on("POST /me/calendars/c1/events", reply{status: http.StatusCreated, body: `{"id":"n1","changeKey":"nk1"}`})
	f.on("PATCH /me/events/n1", reply{body: `{"id":"n1","changeKey":"nk2"}`})
	f.on("DELETE /me/events/n1", reply{status: http.StatusNoContent})
	ev := ICS.Event{UID: "oc-1", Summary: "Plan", Start: at("2026-11-10T09:00:00Z"), End: at("2026-11-10T10:00:00Z"), Zone: "UTC",
		Attendees: []ICS.Attendee{{Address: ICS.Address{Email: "bo@contoso.com"}}}}
	ref, err := a.Push(context.Background(), calendarSyncBusiness.Op{Kind: "create", Remote: cal1, Event: ev})
	if err != nil || ref.ExternalID != "n1" || ref.ETag != "nk1" {
		t.Fatalf("create %+v %v", ref, err)
	}
	if b := f.bodies["POST /me/calendars/c1/events"]; strings.Contains(b, "attendees") || !strings.Contains(b, `"subject":"Plan"`) {
		t.Fatalf("created %s", b)
	}
	ev.Summary = "Plan (moved)"
	ref, err = a.Push(context.Background(), calendarSyncBusiness.Op{Kind: "update", Remote: cal1, ExternalID: "n1", Event: ev})
	if err != nil || ref.ETag != "nk2" || !strings.Contains(f.bodies["PATCH /me/events/n1"], "Plan (moved)") {
		t.Fatalf("update %+v %v", ref, err)
	}
	if _, err := a.Push(context.Background(), calendarSyncBusiness.Op{Kind: "delete", Remote: cal1, ExternalID: "n1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Push(context.Background(), calendarSyncBusiness.Op{Kind: "delete", Remote: cal1, ExternalID: "gone"}); err != nil {
		t.Fatalf("already gone: %v", err)
	}
	f.on("PATCH /me/events/ro", reply{status: http.StatusForbidden, body: `{"error":{"code":"ErrorAccessDenied"}}`})
	if _, err := a.Push(context.Background(), calendarSyncBusiness.Op{Kind: "update", Remote: cal1, ExternalID: "ro", Event: ev}); !errors.Is(err, calendarSyncBusiness.ErrReadOnly) {
		t.Fatalf("read-only: %v", err)
	}
	if _, err := a.Push(context.Background(), calendarSyncBusiness.Op{Kind: "update", Remote: cal1, ExternalID: "n1", Event: ICS.Event{Summary: "x", Start: ev.Start, End: ev.End, RRule: "FREQ=HOURLY"}}); !errors.Is(err, calendarSyncBusiness.ErrRefused) {
		t.Fatalf("a rule Outlook can't take: %v", err)
	}
}

// A weekly series with one instance moved and one cancelled: the delta names
// its instances, the series is read once, and its rule, the skipped date
// and the moved instance come in. An instance changed here is found by its
// original start and changed there; a cancelled one is deleted there.
func TestAWeeklySeriesWithOneMovedAndOneCancelledInstance(t *testing.T) {
	f := newFakeGraph(t)
	var waits []time.Duration
	a := adapterOn(f, &waits)
	occ := func(id, typ, start string) string {
		return `{"id":"` + id + `","type":"` + typ + `","seriesMasterId":"m1","changeKey":"c` + id + `","iCalUId":"per-occurrence-` + id + `","subject":"Standup","start":{"dateTime":"` + start + `","timeZone":"UTC"},"end":{"dateTime":"` + start + `","timeZone":"UTC"}}`
	}
	f.on("GET /me/calendars/c1/calendarView/delta", reply{body: `{"value":[` + occ("o1", "occurrence", "2026-11-09T09:00:00") + `,` + occ("x1", "exception", "2026-11-16T10:00:00") + `,` + occ("o3", "occurrence", "2026-11-30T09:00:00") + `],"@odata.deltaLink":"{base}/d?$deltatoken=d1"}`})
	f.on("GET /me/events/m1", reply{body: `{"id":"m1","type":"seriesMaster","changeKey":"mk1","iCalUId":"uid-standup","subject":"Standup",
		"start":{"dateTime":"2026-11-09T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-09T09:15:00","timeZone":"UTC"},"originalStartTimeZone":"GMT Standard Time",
		"recurrence":{"pattern":{"type":"weekly","interval":1,"daysOfWeek":["monday"],"firstDayOfWeek":"sunday"},"range":{"type":"numbered","startDate":"2026-11-09","numberOfOccurrences":4}},
		"cancelledOccurrences":["OID.m1.2026-11-23"],
		"exceptionOccurrences":[{"id":"x1","type":"exception","seriesMasterId":"m1","changeKey":"xk1","subject":"Standup (later)","originalStart":"2026-11-16T09:00:00Z",
			"start":{"dateTime":"2026-11-16T10:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-16T10:15:00","timeZone":"UTC"},"originalStartTimeZone":"GMT Standard Time"}]}`})
	changes, cursor, err := a.Pull(context.Background(), cal1, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(f.log(), "\n"), "GET /me/events/m1"); n != 1 {
		t.Fatalf("the series was read %d times", n)
	}
	if len(changes) != 2 {
		t.Fatalf("changes %+v", changes)
	}
	master, moved := changes[0], changes[1]
	if master.ExternalID != "m1" || master.SeriesID != "" || master.Event.UID != "uid-standup" || master.Event.RRule != "FREQ=WEEKLY;INTERVAL=1;BYDAY=MO;WKST=SU;COUNT=4" ||
		len(master.Event.ExDates) != 1 || !master.Event.ExDates[0].Equal(at("2026-11-23T09:00:00Z")) || !strings.HasPrefix(master.ETag, "mk1") {
		t.Fatalf("master %+v", master)
	}
	if moved.ExternalID != "x1" || moved.SeriesID != "m1" || moved.Event.UID != "uid-standup" || moved.Event.RecurrenceID == nil ||
		!moved.Event.RecurrenceID.Equal(at("2026-11-16T09:00:00Z")) || !moved.Event.Start.Equal(at("2026-11-16T10:00:00Z")) || moved.Event.Summary != "Standup (later)" {
		t.Fatalf("moved %+v", moved)
	}
	// The series is known now: when the delta only names a removed
	// occurrence, the series is read again and its deletion seen.
	f.on("GET /d", reply{body: `{"value":[{"id":"o1","@removed":{"reason":"deleted"}}],"@odata.deltaLink":"{base}/d?$deltatoken=d2"}`})
	f.replies["GET /me/events/m1"] = []reply{{status: http.StatusNotFound, body: `{"error":{"code":"ErrorItemNotFound"}}`}}
	changes, _, err = a.Pull(context.Background(), cal1, cursor)
	if err != nil {
		t.Fatal(err)
	}
	var gone bool
	for _, c := range changes {
		gone = gone || (c.ExternalID == "m1" && c.Deleted)
	}
	if !gone {
		t.Fatalf("the series' deletion wasn't seen: %+v", changes)
	}

	// Changing and cancelling one instance from here.
	f.on("GET /me/events/m1/instances", reply{body: `{"value":[{"id":"o2","originalStart":"2026-11-16T09:00:00Z"},{"id":"o9","originalStart":"2026-11-23T09:00:00Z"}]}`})
	f.on("PATCH /me/events/o2", reply{body: `{"id":"o2","changeKey":"ok2"}`})
	f.on("DELETE /me/events/o9", reply{status: http.StatusNoContent})
	rid := at("2026-11-16T09:00:00Z")
	if _, err := a.Push(context.Background(), calendarSyncBusiness.Op{Kind: "update", Remote: cal1, ExternalID: "m1",
		Event: ICS.Event{Summary: "Standup (moved here)", Start: at("2026-11-16T11:00:00Z"), End: at("2026-11-16T11:15:00Z"), Zone: "Europe/London", RecurrenceID: &rid}}); err != nil {
		t.Fatal(err)
	}
	if b := f.bodies["PATCH /me/events/o2"]; !strings.Contains(b, "Standup (moved here)") || strings.Contains(b, "recurrence") {
		t.Fatalf("instance patch %s", b)
	}
	rid2 := at("2026-11-23T09:00:00Z")
	if _, err := a.Push(context.Background(), calendarSyncBusiness.Op{Kind: "update", Remote: cal1, ExternalID: "m1",
		Event: ICS.Event{Summary: "Standup", Status: "CANCELLED", Start: rid2, End: rid2.Add(15 * time.Minute), RecurrenceID: &rid2}}); err != nil {
		t.Fatal(err)
	}
	if asked := strings.Join(f.log(), "\n"); !strings.Contains(asked, "DELETE /me/events/o9") {
		t.Fatalf("the cancelled instance wasn't deleted: %s", asked)
	}
}

// Calendars: the default first, named by the account, read-only when the
// link can't edit it, each in the mailbox's zone.
func TestCalendarsAreTheMailboxsWithTheDefaultFirst(t *testing.T) {
	f := newFakeGraph(t)
	var waits []time.Duration
	a := adapterOn(f, &waits)
	f.on("GET /me/calendars", reply{body: `{"value":[{"id":"shared","name":"Team (shared)","canEdit":false},{"id":"main","name":"Calendar","canEdit":true,"isDefaultCalendar":true}]}`})
	f.on("GET /me/mailboxSettings/timeZone", reply{body: `{"value":"Pacific Standard Time"}`})
	cals, err := a.Calendars(context.Background(), calendarSyncBusiness.Link{PersonUUID: "p1", Provider: "microsoft", Account: "sam@contoso.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 2 || cals[0].ID != "main" || !cals[0].Primary || cals[0].Name != "sam@contoso.com" || cals[0].ReadOnly ||
		cals[1].ID != "shared" || !cals[1].ReadOnly || cals[0].Zone != "America/Los_Angeles" || cals[1].Zone != "America/Los_Angeles" {
		t.Fatalf("calendars %+v", cals)
	}
	var w calendarSyncBusiness.Watch
	if _, err := a.Watch(context.Background(), cals[0]); !errors.Is(err, errors.ErrUnsupported) || a.Stop(context.Background(), w) == nil {
		t.Fatal("watching isn't asked of Microsoft yet: the sweep asks every minute")
	}
	_ = json.Valid
}
