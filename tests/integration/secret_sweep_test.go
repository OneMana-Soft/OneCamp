//go:build integration
// +build integration

package integration

import (
	"context"
	"testing"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	commandBusiness "github.com/akashc777/OneCamp/business/Command"
	dataSourceBusiness "github.com/akashc777/OneCamp/business/DataSource"
	transcriptionBusiness "github.com/akashc777/OneCamp/business/Transcription"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	"github.com/google/uuid"
)

// Every place that keeps a secret beside an address it is sent to holds it to
// one rule: a new address without the secret entered again is refused, in
// words, and nothing is saved.
func TestEverySavedSecretStaysWithItsAddress(t *testing.T) {
	t.Setenv("APP_SECRET_KEK", "integration-secretbox-kek-0123456789")
	env := SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	refused := func(what string, err error) {
		t.Helper()
		if !helpers.IsSecretReentry(err) {
			t.Errorf("%s: %v; want the re-entry refusal", what, err)
		}
	}
	str := func(s string) *string { return &s }

	// Speech to text.
	if err := transcriptionBusiness.Save(transcriptionBusiness.SaveInput{
		STTProvider: str("openai"), STTBaseURL: str("https://whisper.example.com/v1"), STTAPIKey: str("stt-key"),
	}); err != nil {
		t.Fatal(err)
	}
	refused("speech to text moved without its key", transcriptionBusiness.Save(transcriptionBusiness.SaveInput{
		STTBaseURL: str("https://elsewhere.example.net/v1"),
	}))
	if transcriptionBusiness.STTBaseURL() != "https://whisper.example.com/v1" {
		t.Errorf("the refused save moved speech to text to %q", transcriptionBusiness.STTBaseURL())
	}
	// A provider whose key the form doesn't manage is never stopped by one:
	// Google takes its own credentials, and the bundled server's key is
	// cleared by the controller. Both switches save.
	if err := transcriptionBusiness.Save(transcriptionBusiness.SaveInput{
		STTProvider: str(transcriptionBusiness.STTDeepgram), STTBaseURL: str(""), STTAPIKey: str("dg-key"),
	}); err != nil {
		t.Fatalf("to Deepgram with its key: %v", err)
	}
	if err := transcriptionBusiness.Save(transcriptionBusiness.SaveInput{
		STTProvider: str(transcriptionBusiness.STTGoogle), STTBaseURL: str(""),
	}); err != nil {
		t.Errorf("from Deepgram, with a key stored, to Google: %v", err)
	}
	if err := transcriptionBusiness.Save(transcriptionBusiness.SaveInput{
		STTProvider: str(transcriptionBusiness.STTDeepgram), STTBaseURL: str(""), STTAPIKey: str("dg-key"),
	}); err != nil {
		t.Fatalf("back to Deepgram with its key: %v", err)
	}
	if err := transcriptionBusiness.Save(transcriptionBusiness.SaveInput{
		STTProvider: str(transcriptionBusiness.STTLocal), STTBaseURL: str(""), STTAPIKey: str(""),
	}); err != nil {
		t.Errorf("from Deepgram, with a key stored, to the bundled server: %v", err)
	}

	// A data source's password.
	ownerID := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, ownerID, "owner-"+ownerID.String()[:8]+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	actor := dataSourceBusiness.Actor{UserID: ownerID, IsAdmin: true}
	view, err := dataSourceBusiness.Create(ctx, dataSourceBusiness.Input{
		Name: "Warehouse", Engine: "postgres", Host: "93.184.216.34", Port: 5432, Database: "dw",
		Username: "reader", Password: str("db-password"), SSLMode: "require",
	}, actor)
	if err != nil {
		t.Fatalf("create data source: %v", err)
	}
	_, err = dataSourceBusiness.Update(ctx, uuid.MustParse(view.Id), dataSourceBusiness.Input{
		Name: "Warehouse", Engine: "postgres", Host: "93.184.216.35", Port: 5432, Database: "dw",
		Username: "reader", SSLMode: "require",
	}, actor)
	refused("a data source moved without its password", err)
	_, err = dataSourceBusiness.Update(ctx, uuid.MustParse(view.Id), dataSourceBusiness.Input{
		Name: "Warehouse", Engine: "postgres", Host: "93.184.216.34", Port: 5432, Database: "dw",
		Username: "reader", SSLMode: "disable",
	}, actor)
	refused("a data source's password sent without TLS", err)

	// A slash-command app's OAuth client secret.
	app, err := commandBusiness.CreateApp(ctx, commandAdapter.CreateAppRequest{
		Slug: "tracker", Name: "Tracker", Kind: "oauth",
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			ClientID: "id", ClientSecret: "client-secret",
			AuthURL: "https://tracker.example.com/authorize", TokenURL: "https://tracker.example.com/token",
		},
	}, ownerID)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	_, err = commandBusiness.UpdateApp(ctx, uuid.MustParse(app.ID), commandAdapter.UpdateAppRequest{
		OAuthConfig: &commandAdapter.AppOAuthConfig{
			ClientID: "id", AuthURL: "https://tracker.example.com/authorize", TokenURL: "https://elsewhere.example.net/token",
		},
	})
	refused("an app's token URL moved without its client secret", err)
	stored, _ := slashModel.GetAppByID(ctx, uuid.MustParse(app.ID))
	if stored == nil || stored.OAuthConfig == nil || !contains(*stored.OAuthConfig, "tracker.example.com/token") {
		t.Errorf("the refused save moved the token URL: %+v", stored)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
