//go:build integration
// +build integration

package integration_test

// Outlook calendar sync over HTTP (calendar B7): each person's status and
// move are of their own link only, and Outlook is offered only once the
// admin has added a Microsoft app, whose secret is never answered back.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	authController "github.com/akashc777/OneCamp/controllers/Auth"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	connectorController "github.com/akashc777/OneCamp/controllers/Connector"
	integrationDomain "github.com/akashc777/OneCamp/domain/Integration"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestOutlookSyncIsEachPersonsOwnAndOfferedOnceTheAdminAddsAnApp(t *testing.T) {
	t.Setenv("MICROSOFT_CLIENT_ID", "")
	t.Setenv("MICROSOFT_CLIENT_SECRET", "")
	ctx := context.Background()
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	outlook := map[string]string{"provider": "microsoft"}

	status := func(who *userModels.UserInfo, provider string) (int, map[string]any) {
		rec := call(t, eventController.SyncStatus, http.MethodGet, "/calendar/sync/"+provider, who, "", map[string]string{"provider": provider})
		var body struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Data
	}
	if code, st := status(sam, "microsoft"); code != http.StatusOK || st["state"] != "none" || st["provider"] != "microsoft" || st["available"] != false {
		t.Fatalf("Outlook's status with no app: %d %v", code, st)
	}
	// The Google route answers as it did.
	if code, st := status(sam, "google"); code != http.StatusOK || st["provider"] != "google" {
		t.Fatalf("Google's status: %d %v", code, st)
	}
	if code, _ := status(sam, "ics"); code != http.StatusBadRequest {
		t.Fatalf("a feed's status: %d", code)
	}

	// Sam's Outlook link is his: Maya's move asks her to connect her own.
	tok, err := helpers.EncryptSecret("access-token")
	if err != nil {
		t.Fatal(err)
	}
	meta := `{"account_email":"sam@contoso.com"}`
	if err := integrationDomain.UpsertIntegration(ctx, "user", sam.UserPostgresInfo.Id, connectorBusiness.ProviderOutlookCalendar, &tok, nil, nil, &meta, nil); err != nil {
		t.Fatal(err)
	}
	rec := call(t, eventController.MoveFrom, http.MethodPost, "/calendar/sync/microsoft/move", maya, "", outlook)
	if rec.Code != http.StatusBadRequest || msgOf(rec) != "Connect Outlook first, then move your calendars." {
		t.Fatalf("Maya's move: %d %s", rec.Code, rec.Body.String())
	}
	if code, st := status(maya, "microsoft"); code != http.StatusOK || st["state"] != "none" || st["account"] != nil {
		t.Fatalf("Maya sees %d %v", code, st)
	}

	offered := func() bool {
		rec := call(t, connectorController.ListConnectors, http.MethodGet, "/connectors", sam, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("connectors: %d %s", rec.Code, rec.Body.String())
		}
		return strings.Contains(rec.Body.String(), `"`+connectorBusiness.ProviderOutlookCalendar+`"`)
	}
	if offered() {
		t.Fatal("Outlook is offered with no Microsoft app")
	}
	rec = call(t, authController.UpdateOAuthConfig, http.MethodPost, "/admin/auth/oauth-config", sam, `{"microsoft_client_id":"app-id","microsoft_client_secret":"app-secret"}`, nil)
	saved := func(rec *httptest.ResponseRecorder) map[string]any {
		var body struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Data
	}
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "app-secret") || saved(rec)["microsoft_configured"] != true {
		t.Fatalf("saving the Microsoft app: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(t, authController.GetOAuthConfig, http.MethodGet, "/admin/auth/oauth-config", sam, "", nil); strings.Contains(rec.Body.String(), "app-secret") || saved(rec)["microsoft_has_client_secret"] != true {
		t.Fatalf("the admin's view: %s", rec.Body.String())
	}
	if !offered() {
		t.Fatal("Outlook isn't offered once the admin added a Microsoft app")
	}
	if code, st := status(sam, "microsoft"); code != http.StatusOK || st["available"] != true {
		t.Fatalf("Outlook's status with an app: %d %v", code, st)
	}
}
