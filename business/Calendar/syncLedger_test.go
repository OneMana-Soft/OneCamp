package Calendar

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// An admin's ledger row is link metadata and counts: its JSON has exactly
// these keys, so no token, refresh token, channel secret, event title, note,
// attendee or provider's own words can ride along on it (gcal-8 H1).
func TestASyncLedgerRowCarriesOnlyLinkMetadataAndCounts(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	last := now.Add(-2 * syncStaleAfter)
	sam := uuid.New()
	r := models.SyncLedgerRow{Link: models.SyncLink{ID: uuid.New(), PersonID: sam, Provider: "google", Account: "sam@example.com",
		State: models.SyncSynced, Error: "provider_error", LastSyncedAt: &last, Skipped: json.RawMessage(`[{"title":"Doctor"}]`)},
		Refusals: 2, DeadSends: 1}
	names := map[string]*dgraphStruct.DgraphUser{sam.String(): {UserName: "sam", UserFullName: "Sam Rao"}}
	row := ledgerRowOf(r, names, now)
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	slices.Sort(got)
	want := []string{"account", "dead_sends", "error_class", "id", "last_synced_at", "person", "provider", "refusals", "stale_since", "state"}
	if !slices.Equal(got, want) {
		t.Fatalf("a ledger row's keys are %v, want %v: %s", got, want, raw)
	}
	for _, never := range []string{"token", "secret", "title", "Doctor", "notes", "attendee", "skipped"} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(never)) {
			t.Errorf("a ledger row carries %q: %s", never, raw)
		}
	}
	if row.ID != r.Link.ID.String() || row.Person.UUID != sam.String() || row.Person.FullName != "Sam Rao" || row.Provider != "google" ||
		row.Account != "sam@example.com" || row.State != models.SyncSynced || row.ErrorClass != readProviderError ||
		row.Refusals != 2 || row.DeadSends != 1 || row.StaleSince == nil || !row.StaleSince.Equal(last) {
		t.Errorf("the row is shaped wrong: %+v", row)
	}
	// A link in step a moment ago isn't stale.
	fresh := now.Add(-time.Minute)
	r.Link.LastSyncedAt, r.Link.Error = &fresh, ""
	if row := ledgerRowOf(r, names, now); row.StaleSince != nil || row.ErrorClass != "" {
		t.Errorf("a link in step: %+v", row)
	}
}

// The ledger names why a link isn't in step as a class, never the words
// stored on it.
func TestASyncLinksErrorIsAClass(t *testing.T) {
	for _, c := range []struct {
		state, stored, want string
	}{
		{models.SyncSynced, "", ""},
		{models.SyncSynced, readUnreachable, readUnreachable},
		{models.SyncSynced, readRateLimited, readRateLimited},
		{models.SyncSynced, readProviderError, readProviderError},
		{models.SyncSynced, "googleapi: Error 500: backend error for sam@example.com", readProviderError},
		{models.SyncBroken, syncBrokenWhy, "disconnected"},
		{models.SyncBroken, syncBrokenWhyMicrosoft, "disconnected"},
		{models.SyncBroken, syncAdminBrokeWhy, "admin_disconnected"},
		{models.SyncFailed, "Your Google calendars couldn't be read. Connect Google again, then try once more.", "move_failed"},
		{models.SyncMoving, "", ""},
	} {
		l := &models.SyncLink{Provider: "google", State: c.state, Error: c.stored}
		if got := syncErrorClass(statusOf("google", l, time.Now())); got != c.want {
			t.Errorf("%s link with %q: class %q, want %q", c.state, c.stored, got, c.want)
		}
	}
}

// The ledger's filters: a state and a provider it knows, at most 100 rows a
// page, from the cursor it handed out.
func TestSyncLedgerFilters(t *testing.T) {
	for _, c := range []struct {
		name string
		q    url.Values
		want models.SyncLedgerFilter
		bad  bool
	}{
		{"nothing asked", url.Values{}, models.SyncLedgerFilter{Limit: 50}, false},
		{"broken Outlook links, the third page of ten", url.Values{"state": {"broken"}, "provider": {"microsoft"}, "limit": {"10"}, "cursor": {helpers.OffsetCursor(20)}},
			models.SyncLedgerFilter{State: models.SyncBroken, Provider: "microsoft", Limit: 10, Offset: 20}, false},
		{"synced Google links", url.Values{"state": {"synced"}, "provider": {"google"}}, models.SyncLedgerFilter{State: models.SyncSynced, Provider: "google", Limit: 50}, false},
		{"too many", url.Values{"limit": {"500"}}, models.SyncLedgerFilter{Limit: 100}, false},
		{"a state links don't have", url.Values{"state": {"stale"}}, models.SyncLedgerFilter{}, true},
		{"a provider that doesn't sync both ways", url.Values{"provider": {"caldav"}}, models.SyncLedgerFilter{}, true},
		{"an address read", url.Values{"provider": {"ics"}}, models.SyncLedgerFilter{}, true},
	} {
		got, err := ledgerFilterOf(c.q)
		if (err != nil) != c.bad || (!c.bad && got != c.want) {
			t.Errorf("%s: %+v, %v; want %+v (refused %v)", c.name, got, err, c.want, c.bad)
		}
	}
}
