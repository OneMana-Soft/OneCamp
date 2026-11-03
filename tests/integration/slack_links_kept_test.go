//go:build integration
// +build integration

package integration_test

// A Slack channel link stays on record when Slack is switched to another
// workspace or disconnected (V-273), as when an admin unlinks one (V-184
// head 4b): every live link is marked taken away, by whom and when, a link
// taken away before keeps its own who and when, the message id map (derived)
// goes, and the same channels can be linked again.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	slackBridgeController "github.com/akashc777/OneCamp/controllers/SlackBridge"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	model "github.com/akashc777/OneCamp/models/postgres/SlackBridge"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestSlackLinksStayOnRecordThroughASwitchOrADisconnect(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("APP_SECRET_KEK", "slack-links-kept-integration-kek")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	ada, bo := uuid.New(), uuid.New()
	connect := func(team string, by uuid.UUID) {
		t.Helper()
		if err := model.SaveBridge(ctx, model.Bridge{TeamID: team, TeamName: team, BotUserID: "U1", BotID: "B1",
			BotTokenEnc: "sealed-token", SigningSecretEnc: "sealed-secret", CreatedBy: &by}); err != nil {
			t.Fatal(err)
		}
	}
	link := func(slackChannel string, channel uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if err := model.CreateLink(ctx, model.Link{ID: id, SlackChannelID: slackChannel, SlackChannelName: slackChannel,
			ChannelUUID: channel, CreatedBy: &ada}); err != nil {
			t.Fatalf("link %s: %v", slackChannel, err)
		}
		if _, err := model.ClaimSlackMessage(ctx, slackChannel, "1700000000."+slackChannel, channel); err != nil {
			t.Fatal(err)
		}
		return id
	}
	live := func() int {
		t.Helper()
		links, err := model.ListLinks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(links)
	}
	// takenBy is who the kept link id was taken away by ("" for no person),
	// and whether it is kept and marked at all.
	takenBy := func(id uuid.UUID) (string, bool) {
		t.Helper()
		var by *string
		var marked bool
		if err := env.PG.QueryRow(`SELECT deleted_by::text, deleted_at IS NOT NULL FROM slack_bridge_links WHERE id = $1`, id).Scan(&by, &marked); err != nil {
			t.Fatalf("link %s is not kept: %v", id, err)
		}
		if by == nil {
			return "", marked
		}
		return *by, marked
	}
	messages := func() int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM slack_bridge_messages`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	general, random, ops := uuid.New(), uuid.New(), uuid.New()

	connect("T1", ada)
	early := link("C0", ops)
	if ok, err := model.DeleteLink(ctx, early, ada); err != nil || !ok {
		t.Fatalf("unlink: %v %v", ok, err)
	}
	var earlyAt string
	if err := env.PG.QueryRow(`SELECT deleted_at::text FROM slack_bridge_links WHERE id = $1`, early).Scan(&earlyAt); err != nil {
		t.Fatal(err)
	}
	first, second := link("C1", general), link("C2", random)

	t.Run("new credentials for the same workspace change no link", func(t *testing.T) {
		connect("T1", bo)
		if n := live(); n != 2 {
			t.Fatalf("%d live links, want 2", n)
		}
	})
	t.Run("switching workspace keeps every link, taken away by the admin who switched", func(t *testing.T) {
		connect("T2", bo)
		if n := live(); n != 0 {
			t.Fatalf("%d live links after the switch, want 0", n)
		}
		for _, id := range []uuid.UUID{first, second} {
			if by, marked := takenBy(id); !marked || by != bo.String() {
				t.Errorf("link %s: marked=%v by %q, want marked by the admin who switched (%s)", id, marked, by, bo)
			}
		}
		if by, _ := takenBy(early); by != ada.String() {
			t.Errorf("the link unlinked before now reads taken by %q, want its own %s", by, ada)
		}
		var at string
		if err := env.PG.QueryRow(`SELECT deleted_at::text FROM slack_bridge_links WHERE id = $1`, early).Scan(&at); err != nil || at != earlyAt {
			t.Errorf("the link unlinked before moved its time from %s to %s (%v)", earlyAt, at, err)
		}
		if n := messages(); n != 0 {
			t.Errorf("%d bridged message ids outlived the switch, want 0", n)
		}
	})
	// disconnect is DELETE /admin/slack-bridge, as the admin who (nil: no
	// person in the request).
	disconnect := func(t *testing.T, who *uuid.UUID) {
		t.Helper()
		rctx := ctx
		if who != nil {
			rctx = context.WithValue(ctx, helpers.UserInfoContextKey,
				userModels.UserInfo{UserPostgresInfo: userModels.User{Id: *who, EmailID: "admin@example.test"}})
		}
		rec := httptest.NewRecorder()
		slackBridgeController.Disconnect(rec, httptest.NewRequest(http.MethodDelete, "/admin/slack-bridge", nil).WithContext(rctx))
		if rec.Code != http.StatusOK {
			t.Fatalf("disconnect = %d %s", rec.Code, rec.Body.String())
		}
	}
	again := link("C1", general)
	t.Run("disconnecting keeps every link, taken away by the admin who disconnected", func(t *testing.T) {
		disconnect(t, &ada)
		if b, err := model.GetBridge(ctx); err != nil || b != nil {
			t.Fatalf("the bridge outlived its disconnect: %+v %v", b, err)
		}
		if by, marked := takenBy(again); !marked || by != ada.String() {
			t.Errorf("link %s: marked=%v by %q, want marked by the admin who disconnected (%s)", again, marked, by, ada)
		}
		if by, _ := takenBy(first); by != bo.String() {
			t.Errorf("a link the switch ended now reads taken by %q, want %s", by, bo)
		}
		var kept int
		if err := env.PG.QueryRow(`SELECT count(*) FROM slack_bridge_links`).Scan(&kept); err != nil || kept != 4 {
			t.Errorf("%d links on record, want all 4 (%v)", kept, err)
		}
		if n := messages(); n != 0 {
			t.Errorf("%d bridged message ids outlived the disconnect, want 0", n)
		}
	})
	t.Run("a disconnect by no person marks the links with nobody", func(t *testing.T) {
		connect("T3", ada)
		last := link("C2", random)
		disconnect(t, nil)
		if by, marked := takenBy(last); !marked || by != "" {
			t.Errorf("link %s: marked=%v by %q, want marked by nobody", last, marked, by)
		}
	})
}
