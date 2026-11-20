package Calendar

// A calendar that can't reach its provider says so (gcal-18): a read that
// fails for now keeps the link synced, keeps why as a class, and leaves when
// it was last in step, so the person's status can say since when their
// calendar stopped following; and the admin's system checks say which store
// the events are in and when they moved.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	"google.golang.org/api/googleapi"
)

func TestAProviderThatCantBeReadSaysSoAndTheLinkCarriesOn(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	ledger, sweep := syncWithGoogle(t, h, sam, g)
	link := func() models.SyncLink {
		ledger.mu.Lock()
		defer ledger.mu.Unlock()
		return *ledger.links[personOf(sam).String()+"google"]
	}
	sweep()
	before := link()
	if before.LastSyncedAt == nil || before.Error != "" {
		t.Fatalf("a clean pass left %+v", before)
	}

	// Google can't be reached: the link stays synced, says why as a class,
	// and isn't stamped in step.
	g.pullErr = &url.Error{Op: "Get", URL: "https://www.googleapis.com/calendar/v3/calendars/sam/events?syncToken=s3cret", Err: errors.New("dial tcp: connection refused")}
	sweep()
	got := link()
	if got.State != models.SyncSynced || got.Error != "unreachable" || got.LastSyncedAt == nil || !got.LastSyncedAt.Equal(*before.LastSyncedAt) {
		t.Fatalf("after a failed read the link is %s %q %v, want synced \"unreachable\" %v", got.State, got.Error, got.LastSyncedAt, before.LastSyncedAt)
	}
	st, err := h.s.SyncStatus(context.Background(), sam, "google")
	if err != nil || st.State != models.SyncSynced || st.ReadError != "unreachable" || st.Error != "" {
		t.Fatalf("status %+v %v", st, err)
	}
	body, _ := json.Marshal(st)
	for _, leak := range []string{"googleapis", "s3cret", "refused", "dial"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("the status says the provider's own words (%s): %s", leak, body)
		}
	}

	// The next clean pass clears it and stamps the link in step.
	g.pullErr = nil
	sweep()
	after := link()
	if after.Error != "" || after.LastSyncedAt == nil || !after.LastSyncedAt.After(*before.LastSyncedAt) {
		t.Fatalf("a clean pass after a failed one left %q %v", after.Error, after.LastSyncedAt)
	}
	if st, _ := h.s.SyncStatus(context.Background(), sam, "google"); st.ReadError != "" || st.StaleSince != nil {
		t.Fatalf("status after a clean pass %+v", st)
	}
}

// A calendar deleted or unshared at the provider is refused on every read,
// for good (Google's 404 or 403, Graph's 404 or 403, as each adapter's syncErr
// sorts them): it's skipped, as it was before a read said why, and never
// holds the link's other calendars back, so the link is stamped in step and
// the person's status never goes stale for it (CR-356 F1). A provider that
// can't be reached for any calendar still holds the link back and says so.
func TestACalendarGoneAtTheProviderNeverHoldsTheLinkBack(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	g.shared = []sharedCalendar{{Remote: calendarSyncBusiness.Remote{ID: "team", Name: "Team offsites"}}}
	ledger, sweep := syncWithGoogle(t, h, sam, g)
	link := func() models.SyncLink {
		ledger.mu.Lock()
		defer ledger.mu.Unlock()
		return *ledger.links[personOf(sam).String()+"google"]
	}
	sweep()
	if l := link(); l.LastSyncedAt == nil || l.Error != "" {
		t.Fatalf("a clean pass left %+v", l)
	}
	for _, gone := range []error{
		fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRefused, &googleapi.Error{Code: 404, Message: "Not Found"}),
		fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRefused, &googleapi.Error{Code: 403, Message: "Forbidden"}),
		fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRefused, errors.New("microsoft graph: 404 ErrorItemNotFound")),
		fmt.Errorf("%w: %w", calendarSyncBusiness.ErrReadOnly, errors.New("microsoft graph: 403 ErrorAccessDenied")),
	} {
		// Sam's own calendar is gone at the provider; the shared one reads.
		g.pullErr = gone
		h.clock = h.clock.Add(syncStaleAfter)
		sweep()
		got := link()
		if got.State != models.SyncSynced || got.Error != "" || got.LastSyncedAt == nil || !got.LastSyncedAt.Equal(h.clock) {
			t.Fatalf("with a calendar gone (%v) the link is %s %q %v, want synced and in step at %v", gone, got.State, got.Error, got.LastSyncedAt, h.clock)
		}
		if st, err := h.s.SyncStatus(context.Background(), sam, "google"); err != nil || st.ReadError != "" || st.StaleSince != nil {
			t.Fatalf("status with a calendar gone (%v): %+v %v", gone, st, err)
		}
	}

	// Google can't be reached for any calendar: the link isn't stamped, says
	// why, and its status goes stale from its last pass in step.
	g.down = &url.Error{Op: "Get", URL: "https://www.googleapis.com/calendar/v3/calendars/team/events", Err: errors.New("dial tcp: connection refused")}
	last := link().LastSyncedAt
	h.clock = h.clock.Add(syncStaleAfter)
	sweep()
	if got := link(); got.State != models.SyncSynced || got.Error != "unreachable" || got.LastSyncedAt == nil || !got.LastSyncedAt.Equal(*last) {
		t.Fatalf("with Google down the link is %s %q %v, want synced \"unreachable\" %v", got.State, got.Error, got.LastSyncedAt, last)
	}
	if st, err := h.s.SyncStatus(context.Background(), sam, "google"); err != nil || st.ReadError != "unreachable" || st.StaleSince == nil || !st.StaleSince.Equal(*last) {
		t.Fatalf("status with Google down: %+v %v", st, err)
	}
}

// Google and Microsoft are read through the one pass, so one classifier sorts
// both: a provider that can't be reached, one that asks to be called less,
// and anything else it answered.
func TestAReadErrorIsAClassNeverTheProvidersWords(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{&url.Error{Op: "Get", URL: "https://graph.microsoft.com/v1.0/me/calendars/x/events/delta?$deltatoken=429", Err: errors.New("i/o timeout")}, "unreachable"},
		{fmt.Errorf("pull: %w", context.DeadlineExceeded), "unreachable"},
		{&googleapi.Error{Code: 429}, "rate_limited"},
		{&googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}}, "rate_limited"},
		{errors.New("microsoft graph: 429 TooManyRequests"), "rate_limited"},
		{&googleapi.Error{Code: 503, Message: "Backend Error"}, "provider_error"},
		{errors.New("microsoft graph: 500 generalException"), "provider_error"},
		// A calendar refused for good is skipped, not the link's to say.
		{fmt.Errorf("%w: %w", calendarSyncBusiness.ErrRefused, &googleapi.Error{Code: 404}), ""},
		{fmt.Errorf("%w: %w", calendarSyncBusiness.ErrReadOnly, errors.New("microsoft graph: 403 ErrorAccessDenied")), ""},
	} {
		if got := syncReadError(c.err); got != c.want {
			t.Errorf("syncReadError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestSyncStatusSaysSinceWhenALinkStoppedFollowing(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	synced := func(last *time.Time, why string) *models.SyncLink {
		return &models.SyncLink{Provider: "google", State: models.SyncSynced, LastSyncedAt: last, Error: why}
	}
	long := ago(syncStaleAfter + time.Minute)
	st := statusOf("google", synced(long, "rate_limited"), now)
	if st.StaleSince == nil || !st.StaleSince.Equal(*long) || st.ReadError != "rate_limited" || st.Error != "" {
		t.Fatalf("a link out of step for longer than the bound: %+v", st)
	}
	body, _ := json.Marshal(st)
	if !strings.Contains(string(body), `"stale_since":"2026-11-16T`) || !strings.Contains(string(body), `"read_error":"rate_limited"`) {
		t.Fatalf("its JSON: %s", body)
	}
	// One read failed a minute ago: why shows, but it hasn't stopped following.
	if st := statusOf("google", synced(ago(time.Minute), "unreachable"), now); st.StaleSince != nil || st.ReadError != "unreachable" {
		t.Fatalf("a recent failed read: %+v", st)
	}
	// In step, and never in step yet.
	for _, last := range []*time.Time{ago(time.Minute), nil} {
		st := statusOf("google", synced(last, ""), now)
		body, _ := json.Marshal(st)
		if st.StaleSince != nil || st.ReadError != "" || !strings.Contains(string(body), `"stale_since":null`) || strings.Contains(string(body), "read_error") {
			t.Fatalf("a link in step (%v): %s", last, body)
		}
	}
	// A broken link says why in words, as it did, and no class.
	broken := &models.SyncLink{Provider: "google", State: models.SyncBroken, Error: syncBrokenWhy, LastSyncedAt: ago(2 * syncStaleAfter)}
	if st := statusOf("google", broken, now); st.Error != syncBrokenWhy || st.ReadError != "" || st.StaleSince != nil {
		t.Fatalf("a broken link: %+v", st)
	}
	if st := statusOf("google", nil, now); st.State != "none" || st.StaleSince != nil {
		t.Fatalf("no link: %+v", st)
	}
}

func TestTheCalendarStoreCheckSaysWhereEventsAreAndWhenTheyMoved(t *testing.T) {
	moved := &configModel.SystemConfig{Key: movedKey, Value: "done: moved 12 events", UpdatedAt: time.Date(2026, 11, 2, 9, 14, 0, 0, time.UTC)}
	var record *configModel.SystemConfig
	var readErr error
	reads := 0
	var readOn context.Context
	prev := movedRecord
	movedRecord = func(ctx context.Context) (*configModel.SystemConfig, error) {
		reads++
		readOn = ctx
		return record, readErr
	}
	t.Cleanup(func() { movedRecord = prev })
	type asked struct{}
	says := func(want ...string) {
		t.Helper()
		reads = 0
		ctx := context.WithValue(context.Background(), asked{}, "the admin's request")
		err := storeProbe(ctx)
		if err == nil {
			t.Fatal("the check says nothing")
		}
		if reads != 1 || readOn != ctx {
			t.Fatalf("the check read the move's record %d times, on the request's ctx %v, want once on it", reads, readOn == ctx)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("the check says %q, want it to say %q", err, w)
			}
		}
	}
	result := func() helpers.SystemCheckResult {
		t.Helper()
		for _, r := range helpers.RunSystemChecks(context.Background(), 2*time.Second) {
			if r.Name == "calendar store" {
				if r.Kind != helpers.CheckKindBehaviour || r.Describe == "" {
					t.Fatalf("the check %+v", r)
				}
				return r
			}
		}
		t.Fatal("the calendar store check isn't registered where an admin sees system checks")
		return helpers.SystemCheckResult{}
	}

	t.Setenv(helpers.CalendarStoreEnv, "")
	record = moved
	says("kept in the Event Model and read from it", "moved into it on 2 Nov 2026 09:14 UTC", "moved 12 events")
	// Not moved yet: a note, not a failure.
	record = nil
	says("kept in the Event Model", "reads still come from the graph", "not been moved")
	if r := result(); !r.Healthy || !strings.Contains(r.Detail, "reads still come from the graph") {
		t.Fatalf("reads not yet from the store: %+v", r)
	}

	t.Setenv(helpers.CalendarStoreEnv, helpers.CalendarOldStores)
	says("kept in the graph (CALENDAR_STORE=graph)", "not been moved")
	record = moved
	says("kept in the graph (CALENDAR_STORE=graph)", "moved into it on 2 Nov 2026 09:14 UTC")

	// A record that can't be read is the one failure.
	readErr = errors.New("connection refused")
	if r := result(); r.Healthy || !strings.Contains(r.Detail, "couldn't be read") {
		t.Fatalf("an unreadable record: %+v", r)
	}
}
