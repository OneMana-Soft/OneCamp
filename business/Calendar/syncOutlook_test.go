package Calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	"github.com/akashc777/OneCamp/business/CalendarSync/microsoft"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// fakeOutlook is Microsoft Graph for one person with one calendar, which the
// real Outlook adapter talks to: its events, what changed since the last
// delta, and what OneCamp asked of it.
type fakeOutlook struct {
	mu      sync.Mutex
	srv     *httptest.Server
	events  map[string]string // id → the event as Graph sends it
	changed []string          // ids changed or removed since the last delta
	round   int
	made    []string
	// said is each create and update OneCamp sent, with its sensitivity.
	said    []string
	revoked bool
}

func newFakeOutlook(t *testing.T) *fakeOutlook {
	f := &fakeOutlook{events: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// outlookEvent is a one-hour event on 10 November as Graph sends it.
func outlookEvent(id, version, subject, sensitivity string) string {
	return fmt.Sprintf(`{"id":%q,"iCalUId":"%s-uid","changeKey":%q,"type":"singleInstance","subject":%q,"sensitivity":%q,"showAs":"busy",`+
		`"start":{"dateTime":"2026-11-10T12:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-10T13:00:00","timeZone":"UTC"},"originalStartTimeZone":"GMT Standard Time"}`,
		id, id, version, subject, sensitivity)
}

// put changes an event at Outlook; "" deletes it.
func (f *fakeOutlook) put(id, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if raw == "" {
		delete(f.events, id)
	} else {
		f.events[id] = raw
	}
	f.changed = append(f.changed, id)
}

func (f *fakeOutlook) revoke() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = true
}

// sent is what OneCamp asked Outlook to change since it was last asked.
func (f *fakeOutlook) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.made
	f.made = nil
	return out
}

// sensitivities is what each create and update said since it was last asked.
func (f *fakeOutlook) sensitivities() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.said
	f.said = nil
	return out
}

func (f *fakeOutlook) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.revoked || r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"InvalidAuthenticationToken"}}`)
		return
	}
	var body struct {
		Subject     string `json:"subject"`
		Sensitivity string `json:"sensitivity"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		f.said = append(f.said, body.Subject+" sensitivity="+body.Sensitivity)
	}
	p := r.URL.Path
	switch {
	case p == "/me/mailboxSettings/timeZone":
		_, _ = io.WriteString(w, `{"value":"GMT Standard Time"}`)
	case p == "/me/calendars":
		_, _ = io.WriteString(w, `{"value":[{"id":"cal1","name":"Calendar","canEdit":true,"isDefaultCalendar":true}]}`)
	case p == "/me/calendars/cal1/calendarView/delta":
		var items []string
		if r.URL.Query().Get("$deltatoken") == "" {
			for _, raw := range f.events {
				items = append(items, raw)
			}
		} else {
			for _, id := range f.changed {
				if raw, ok := f.events[id]; ok {
					items = append(items, raw)
				} else {
					items = append(items, `{"id":"`+id+`","@removed":{"reason":"deleted"}}`)
				}
			}
		}
		f.changed, f.round = nil, f.round+1
		_, _ = fmt.Fprintf(w, `{"value":[%s],"@odata.deltaLink":"%s/me/calendars/cal1/calendarView/delta?$deltatoken=d%d"}`, strings.Join(items, ","), f.srv.URL, f.round)
	case r.Method == http.MethodPost && p == "/me/calendars/cal1/events":
		f.made = append(f.made, "create "+body.Subject)
		_, _ = io.WriteString(w, `{"id":"new1","changeKey":"ck1"}`)
	case r.Method == http.MethodPatch && strings.HasPrefix(p, "/me/events/"):
		f.made = append(f.made, "update "+body.Subject)
		_, _ = io.WriteString(w, `{"id":"`+strings.TrimPrefix(p, "/me/events/")+`","changeKey":"ck2"}`)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/me/events/") && f.events[strings.TrimPrefix(p, "/me/events/")] != "":
		// A series' master, read whole with its changed instances.
		_, _ = io.WriteString(w, f.events[strings.TrimPrefix(p, "/me/events/")])
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/me/events/"):
		f.made = append(f.made, "delete "+strings.TrimPrefix(p, "/me/events/"))
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"ErrorItemNotFound"}}`)
	}
}

// outlookOn is the real Outlook adapter against f, signed in.
func outlookOn(f *fakeOutlook) microsoft.Adapter {
	return microsoft.Adapter{Base: f.srv.URL, Token: func(context.Context, string) (string, error) { return "tok", nil }, Client: f.srv.Client(),
		Now:   func() time.Time { return time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC) },
		Sleep: func(context.Context, time.Duration) error { return nil }}
}

// syncWithProviders moves calendars in through the adapters named, as the
// people's links are, and answers a sweep a minute later. Which adapters
// each sweep asked for is counted.
func syncWithProviders(t *testing.T, h *harness, adapters map[string]calendarSyncBusiness.Adapter, asked map[string]int) func() {
	t.Helper()
	ledger := newMemLedger(h.store, func() time.Time { return h.clock })
	h.s.ledger, h.s.moveNow = ledger, true
	prevProvider, prevGoogle, prevMicrosoft := syncProvider, googleAccountOf, microsoftAccountOf
	syncProvider = func(p string) (calendarSyncBusiness.Adapter, bool) {
		asked[p]++
		a, ok := adapters[p]
		return a, ok
	}
	googleAccountOf = func(context.Context, *model.UserInfo) (string, bool, error) { return "sam@outlook.com", true, nil }
	microsoftAccountOf = func(context.Context, *model.UserInfo) (string, bool, error) { return "sam@contoso.com", true, nil }
	t.Cleanup(func() { syncProvider, googleAccountOf, microsoftAccountOf = prevProvider, prevGoogle, prevMicrosoft })
	return func() {
		t.Helper()
		h.clock = h.clock.Add(time.Minute)
		if err := h.s.syncAll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func move(t *testing.T, h *harness, who *model.UserInfo, provider string) *SyncStatus {
	t.Helper()
	st, err := h.s.MoveFrom(context.Background(), who, provider)
	if err != nil || st.State != models.SyncSynced {
		t.Fatalf("move from %s: %+v %v", provider, st, err)
	}
	return st
}

// Outlook through the real adapter and the real Syncer: the move brings its
// events (a private one private), either side's change reaches the other, a
// deletion at Outlook puts the copy in the trash, and when Microsoft stops
// answering the link breaks and says so.
func TestTwoWaySyncWithOutlookThroughGraph(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	f := newFakeOutlook(t)
	f.put("lunch", outlookEvent("lunch", "k1", "Lunch", "normal"))
	f.put("1on1", outlookEvent("1on1", "k1", "One to one", "private"))
	sweep := syncWithProviders(t, h, map[string]calendarSyncBusiness.Adapter{"microsoft": outlookOn(f)}, map[string]int{})
	if st := move(t, h, sam, "microsoft"); st.Provider != "microsoft" || st.Moved != 2 || st.Account != "sam@contoso.com" {
		t.Fatalf("moved %+v", st)
	}
	if e := eventFromGoogle(h, "1on1-uid"); e == nil || e.Visibility != visibilityPrivate {
		t.Fatalf("a private event at Outlook isn't private here: %+v", e)
	}
	if e := eventFromGoogle(h, "lunch-uid"); e == nil || e.Visibility != visibilityDefault {
		t.Fatalf("lunch %+v", e)
	}

	sweep()
	if m := f.sent(); len(m) != 0 {
		t.Fatalf("an echo after the move: %v", m)
	}
	f.put("lunch", outlookEvent("lunch", "k2", "Lunch with Maya", "normal"))
	sweep()
	if e := eventFromGoogle(h, "lunch-uid"); e == nil || e.Title != "Lunch with Maya" {
		t.Fatalf("Outlook's change didn't come: %+v", e)
	}

	made := h.create(sam, "Pricing review")
	sweep()
	if m := f.sent(); !slices.Equal(m, []string{"create Pricing review"}) {
		t.Fatalf("sent %v", m)
	}
	sweep()
	if m := f.sent(); len(m) != 0 {
		t.Fatalf("the new event was sent twice: %v", m)
	}
	title, seq := "Pricing review, final", made.Sequence
	if _, err := h.s.Update(context.Background(), sam, made.ID, EventPatch{Title: &title, Sequence: &seq}); err != nil {
		t.Fatal(err)
	}
	sweep()
	if m := f.sent(); !slices.Equal(m, []string{"update Pricing review, final"}) {
		t.Fatalf("sent %v", m)
	}

	f.put("lunch", "")
	sweep()
	if e := eventFromGoogle(h, "lunch-uid"); e == nil || e.DeletedAt == nil {
		t.Fatalf("the event Outlook deleted isn't in the trash here: %+v", e)
	}

	f.revoke()
	sweep()
	st, err := h.s.SyncStatus(context.Background(), sam, "microsoft")
	if err != nil || st.State != models.SyncBroken || st.Error != "The Microsoft account isn't linked any more: connect it again from Calendars." {
		t.Fatalf("status %+v %v", st, err)
	}
	if st, err := h.s.SyncStatus(context.Background(), sam, "google"); err != nil || st.State != "none" {
		t.Fatalf("Google's status %+v %v", st, err)
	}
}

// One sweep keeps every two-way provider's links in step, each provider's
// adapter asked for once; a feed that is only read ("ics") has its own sweep.
func TestTheSweepKeepsEveryTwoWayProviderInStep(t *testing.T) {
	h := newHarness(t)
	sam, maya := h.member("Sam"), h.member("Maya")
	g := newLiveGoogle(func() time.Time { return h.clock })
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	g.put(calendarSyncBusiness.Change{ExternalID: "lunch", Event: ICS.Event{UID: "lunch@google.com", Summary: "Lunch", Start: start, End: start.Add(time.Hour), Zone: "Europe/London"}})
	f := newFakeOutlook(t)
	f.put("plan", outlookEvent("plan", "k1", "Plan", "normal"))
	asked := map[string]int{}
	sweep := syncWithProviders(t, h, map[string]calendarSyncBusiness.Adapter{"google": g, "microsoft": outlookOn(f)}, asked)
	move(t, h, sam, "google")
	move(t, h, maya, "microsoft")
	clear(asked)
	g.put(calendarSyncBusiness.Change{ExternalID: "lunch", Event: ICS.Event{UID: "lunch@google.com", Summary: "Lunch with Maya", Start: start, End: start.Add(time.Hour), Zone: "Europe/London"}})
	f.put("plan", outlookEvent("plan", "k2", "Plan, final", "normal"))
	sweep()
	if len(asked) != 2 || asked["google"] != 1 || asked["microsoft"] != 1 {
		t.Fatalf("a sweep asked for %v", asked)
	}
	if e := eventFromGoogle(h, "lunch@google.com"); e == nil || e.Title != "Lunch with Maya" {
		t.Fatalf("Google's change didn't come: %+v", e)
	}
	if e := eventFromGoogle(h, "plan-uid"); e == nil || e.Title != "Plan, final" {
		t.Fatalf("Outlook's change didn't come: %+v", e)
	}
	if got := calendarSyncBusiness.Adapters.TwoWay(); !slices.Equal(got, []string{"google", "microsoft"}) {
		t.Fatalf("two-way providers %v", got)
	}
}

// A private event from any provider is private here, whatever it was here
// before, so who may only see its calendar sees it as busy; made public
// there, it is public here.
func TestAPrivateEventFromGoogleIsPrivateHere(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	g.put(calendarSyncBusiness.Change{ExternalID: "dr", Event: ICS.Event{UID: "dr@google.com", Summary: "Doctor", Start: start, End: start.Add(time.Hour), Zone: "Europe/London", Class: "CONFIDENTIAL"}})
	_, sweep := syncWithGoogle(t, h, sam, g)
	e := eventFromGoogle(h, "dr@google.com")
	if e == nil || e.Visibility != visibilityPrivate {
		t.Fatalf("a confidential event at Google isn't private here: %+v", e)
	}
	if !hiddenFrom(e, nil, uuid.New(), accessBusiness.Decision{Level: accessBusiness.LevelView}) {
		t.Fatal("someone who may only see the calendar sees the private event")
	}
	g.put(calendarSyncBusiness.Change{ExternalID: "dr", Event: ICS.Event{UID: "dr@google.com", Summary: "Doctor", Start: start, End: start.Add(time.Hour), Zone: "Europe/London", Class: "PUBLIC"}})
	sweep()
	if e := eventFromGoogle(h, "dr@google.com"); e == nil || e.Visibility != visibilityPublic {
		t.Fatalf("made public at Google, it isn't here: %+v", e)
	}
}

// Privacy made here reaches every two-way provider through the Syncer: a
// private event is private at Google and sensitivity "private" at Outlook,
// and made default again here, it is cleared at both.
func TestPrivacyMadeHereReachesEveryTwoWayProvider(t *testing.T) {
	h := newHarness(t)
	sam, maya := h.member("Sam"), h.member("Maya")
	g := newLiveGoogle(func() time.Time { return h.clock })
	f := newFakeOutlook(t)
	sweep := syncWithProviders(t, h, map[string]calendarSyncBusiness.Adapter{"google": g, "microsoft": outlookOn(f)}, map[string]int{})
	move(t, h, sam, "google")
	move(t, h, maya, "microsoft")
	doctor, dentist := h.create(sam, "Doctor"), h.create(maya, "Dentist")
	mark := func(who *model.UserInfo, e *Event, visibility string) *Event {
		t.Helper()
		seq := e.Sequence
		next, err := h.s.Update(context.Background(), who, e.ID, EventPatch{Visibility: &visibility, Sequence: &seq})
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	doctor, dentist = mark(sam, doctor, visibilityPrivate), mark(maya, dentist, visibilityPrivate)
	sweep()
	if got := g.said(); !slices.Equal(got, []string{"create Doctor class=PRIVATE"}) {
		t.Fatalf("Google was sent %v", got)
	}
	if got := f.sensitivities(); !slices.Equal(got, []string{"Dentist sensitivity=private"}) {
		t.Fatalf("Outlook was sent %v", got)
	}

	sweep()
	mark(sam, doctor, visibilityDefault)
	mark(maya, dentist, visibilityDefault)
	sweep()
	if got := g.said(); !slices.Equal(got, []string{"update Doctor class="}) {
		t.Fatalf("made default here, Google was sent %v", got)
	}
	g.mu.Lock()
	cleared := g.events["made1"].Event.Class == ""
	g.mu.Unlock()
	if !cleared {
		t.Fatal("made default here, the event is still private at Google")
	}
	if got := f.sensitivities(); !slices.Equal(got, []string{"Dentist sensitivity=normal"}) {
		t.Fatalf("made default here, Outlook was sent %v", got)
	}
}

// Only a provider calendars sync with is moved from or asked about; Outlook
// not linked says so in words.
func TestAMoveIsFromAProviderThePersonLinked(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	h.s.ledger, h.s.moveNow = newMemLedger(h.store, func() time.Time { return h.clock }), true
	if _, err := h.s.MoveFrom(context.Background(), sam, "ics"); err == nil {
		t.Fatal("a move from a feed that is only read")
	}
	if _, err := h.s.SyncStatus(context.Background(), sam, "yahoo"); err == nil {
		t.Fatal("a status for a provider nothing syncs with")
	}
	prev := microsoftAccountOf
	microsoftAccountOf = func(context.Context, *model.UserInfo) (string, bool, error) { return "", false, nil }
	t.Cleanup(func() { microsoftAccountOf = prev })
	if _, err := h.s.MoveFrom(context.Background(), sam, "microsoft"); err == nil || err.Error() != "Connect Outlook first, then move your calendars." {
		t.Fatalf("not linked: %v", err)
	}
}
