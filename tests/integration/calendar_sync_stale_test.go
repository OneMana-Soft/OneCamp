//go:build integration
// +build integration

package integration_test

// A calendar that can't reach its provider says so (gcal-18): when Outlook
// can't be read, the sync pass keeps the link synced, and the person's own
// status says why, as a class, and since when their calendar stopped
// following. Nobody else's status says it, and no provider words, address or
// token reach it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	integrationDomain "github.com/akashc777/OneCamp/domain/Integration"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// graphDown is a network on which Microsoft Graph can't be reached; anything
// else goes through.
type graphDown struct{ next http.RoundTripper }

func (g graphDown) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Hostname(), "microsoft.com") || strings.HasSuffix(r.URL.Hostname(), "microsoftonline.com") {
		return nil, errors.New("dial tcp: connection refused")
	}
	return g.next.RoundTrip(r)
}

func TestASyncThatCantReadItsProviderSaysSoOnlyOnTheOwnersStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	synced := func(who *userModels.UserInfo, account string) *models.SyncLink {
		t.Helper()
		link, _, err := models.StartMove(ctx, uuid.New(), who.UserPostgresInfo.Id, "microsoft", account, time.Hour)
		must(err)
		must(models.FinishMove(ctx, link.ID, models.SyncSynced, 0, 0, nil, ""))
		return link
	}
	// Sam's Outlook is connected and synced, with a calendar, last in step
	// two hours ago.
	tok, err := helpers.EncryptSecret("access-token")
	must(err)
	meta := `{"account_email":"sam@contoso.com"}`
	must(integrationDomain.UpsertIntegration(ctx, "user", sam.UserPostgresInfo.Id, connectorBusiness.ProviderOutlookCalendar, &tok, nil, nil, &meta, nil))
	samLink := synced(sam, "sam@contoso.com")
	must(models.KeepSyncCalendar(ctx, models.SyncCalendar{LinkID: samLink.ID, RemoteID: "AAMkSam", Name: "Calendar", Primary: true,
		Cursor: "https://graph.microsoft.com/v1.0/me/calendars/AAMkSam/events/delta?$deltatoken=s3cret"}))
	_, err = env.PG.Exec(`UPDATE calendar_sync_links SET last_synced_at = NOW() - interval '2 hours' WHERE id = $1`, samLink.ID)
	must(err)
	// Maya's link has nothing to read: its pass is clean.
	synced(maya, "maya@contoso.com")

	prev := http.DefaultTransport
	http.DefaultTransport = graphDown{next: prev}
	t.Cleanup(func() { http.DefaultTransport = prev })
	sweepBusiness.Jobs.StartAll(ctx)
	sweepBusiness.Jobs.Wake("calendar.sync.google")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		l, err := models.SyncLinkOf(ctx, sam.UserPostgresInfo.Id, "microsoft")
		must(err)
		if l.Error != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sync pass never recorded that Outlook couldn't be read: %+v", l)
		}
	}

	status := func(who *userModels.UserInfo) (string, map[string]any) {
		rec := call(t, eventController.SyncStatus, http.MethodGet, "/calendar/sync/microsoft", who, "", map[string]string{"provider": "microsoft"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data map[string]any `json:"data"`
		}
		must(json.Unmarshal(rec.Body.Bytes(), &body))
		return rec.Body.String(), body.Data
	}
	raw, st := status(sam)
	if st["state"] != models.SyncSynced || st["read_error"] != "unreachable" || st["error"] != nil {
		t.Fatalf("Sam's status: %s", raw)
	}
	since, _ := st["stale_since"].(string)
	at, err := time.Parse(time.RFC3339Nano, since)
	if err != nil || time.Since(at) < 110*time.Minute || time.Since(at) > 130*time.Minute {
		t.Fatalf("Sam's calendar stopped following %q, want two hours ago: %s", since, raw)
	}
	for _, leak := range []string{"graph.microsoft.com", "deltatoken", "s3cret", "access-token", "refused", "dial"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("Sam's status says %q: %s", leak, raw)
		}
	}
	// Maya sees her own link, in step: no read error, no staleness, and
	// nothing of Sam's.
	raw, st = status(maya)
	if st["state"] != models.SyncSynced || st["account"] != "maya@contoso.com" || st["stale_since"] != nil || strings.Contains(raw, "read_error") || strings.Contains(raw, "sam@") {
		t.Fatalf("Maya's status: %s", raw)
	}
}

// A link whose last pass couldn't read its provider takes its turn from that
// pass (updated_at), not from when it was last in step, so a link that keeps
// failing never holds the first place of every sweep (CR-356 F2a).
func TestAFailingLinkTakesItsTurnFromItsLastPass(t *testing.T) {
	ctx := context.Background()
	env, dg := calendarStores(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	synced := func(name, account string) *models.SyncLink {
		t.Helper()
		who := seedCalendarPerson(t, env.PG, dg, name)
		link, _, err := models.StartMove(ctx, uuid.New(), who.UserPostgresInfo.Id, "microsoft", account, time.Hour)
		must(err)
		must(models.FinishMove(ctx, link.ID, models.SyncSynced, 0, 0, nil, ""))
		return link
	}
	// Sam's link was last in step three hours ago and its last pass, just
	// now, couldn't reach Outlook; Maya's was in step an hour ago.
	failing, healthy := synced("Sam", "sam@contoso.com"), synced("Maya", "maya@contoso.com")
	_, err := env.PG.Exec(`UPDATE calendar_sync_links SET last_synced_at = NOW() - interval '3 hours', updated_at = NOW(), error = 'unreachable' WHERE id = $1`, failing.ID)
	must(err)
	_, err = env.PG.Exec(`UPDATE calendar_sync_links SET last_synced_at = NOW() - interval '1 hour', updated_at = NOW() - interval '1 hour', error = '' WHERE id = $1`, healthy.ID)
	must(err)

	first, err := models.SyncedLinks(ctx, "microsoft", 1)
	if err != nil || len(first) != 1 || first[0].ID != healthy.ID {
		t.Fatalf("the first place went to %+v (%v), want Maya's in-step link %s before Sam's failing one %s", first, err, healthy.ID, failing.ID)
	}
	both, err := models.SyncedLinks(ctx, "microsoft", 2)
	if err != nil || len(both) != 2 || both[0].ID != healthy.ID || both[1].ID != failing.ID {
		t.Fatalf("a sweep with room for both reads %+v (%v), want Maya's then Sam's", both, err)
	}
}

// A pass that ends on a link that went moving meanwhile (the person connected
// again) leaves the move's own state: no read error and no stamp from the
// pass (CR-356 F2b).
func TestAPassEndingOnAMovingLinkLeavesTheMovesState(t *testing.T) {
	ctx := context.Background()
	env, dg := calendarStores(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	link, _, err := models.StartMove(ctx, uuid.New(), sam.UserPostgresInfo.Id, "microsoft", "sam@contoso.com", time.Hour)
	must(err)
	must(models.FinishMove(ctx, link.ID, models.SyncSynced, 0, 0, nil, ""))
	_, err = env.PG.Exec(`UPDATE calendar_sync_links SET last_synced_at = NOW() - interval '2 hours' WHERE id = $1`, link.ID)
	must(err)
	before, err := models.SyncLinkOf(ctx, sam.UserPostgresInfo.Id, "microsoft")
	must(err)

	// Sam connects Outlook again while a pass is reading the link; the pass
	// then ends on it, unable to read.
	moving, started, err := models.StartMove(ctx, uuid.New(), sam.UserPostgresInfo.Id, "microsoft", "sam@contoso.com", time.Hour)
	if err != nil || !started || moving.ID != link.ID {
		t.Fatalf("the move again: %+v %v %v", moving, started, err)
	}
	must(models.MarkSynced(ctx, link.ID, "unreachable"))
	after, err := models.SyncLinkOf(ctx, sam.UserPostgresInfo.Id, "microsoft")
	must(err)
	if after.State != models.SyncMoving || after.Error != "" || after.LastSyncedAt == nil || before.LastSyncedAt == nil || !after.LastSyncedAt.Equal(*before.LastSyncedAt) {
		t.Fatalf("after the pass the moving link is %s %q %v, want moving \"\" %v", after.State, after.Error, after.LastSyncedAt, before.LastSyncedAt)
	}
}
