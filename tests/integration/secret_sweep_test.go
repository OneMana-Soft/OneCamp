//go:build integration
// +build integration

package integration

import (
	"context"
	"testing"

	aiAdapter "github.com/akashc777/OneCamp/adapter/AI"
	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	commandBusiness "github.com/akashc777/OneCamp/business/Command"
	dataSourceBusiness "github.com/akashc777/OneCamp/business/DataSource"
	transcriptionBusiness "github.com/akashc777/OneCamp/business/Transcription"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	"github.com/google/uuid"
)

// Every other place that keeps a secret beside an address it is sent to holds
// it to the same rule as MCP servers: a new address without the secret
// entered again is refused, in words, and nothing is saved.
func TestEverySavedSecretStaysWithItsAddress(t *testing.T) {
	t.Setenv("AI_CONFIG_KEK", "integration-ai-kek-0123456789abcdef")
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

	// A custom model endpoint and its API key.
	p, err := aiModels.CreateCustomProvider(ctx, "Own model", "https://models.example.com/v1", "sk-own", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = aiBusiness.UpdateProvider(ctx, p.ID, aiAdapter.UpdateProviderRequest{BaseURL: str("https://elsewhere.example.net/v1")})
	refused("a model endpoint moved without its key", err)
	if again, _ := aiModels.GetProvider(ctx, p.ID); again == nil || again.BaseURL != "https://models.example.com/v1" {
		t.Errorf("the refused save moved the endpoint: %+v", again)
	}

	// Web search, the sandbox runner and the code-PR runner.
	key, _ := aiModels.EncryptAPIKey("search-key")
	if err := aiModels.SetWebSearch(ctx, "searxng", "https://search.example.com", true, key, true); err != nil {
		t.Fatal(err)
	}
	refused("web search moved without its key", aiBusiness.SetWebSearch(ctx, aiAdapter.SetWebSearchRequest{
		Provider: "searxng", BaseURL: "https://elsewhere.example.net", Enabled: true,
	}))
	refused("web search switched provider without its key", aiBusiness.SetWebSearch(ctx, aiAdapter.SetWebSearchRequest{
		Provider: "tavily", Enabled: true,
	}))
	tok, _ := aiModels.EncryptAPIKey("runner-token")
	if err := aiModels.SetSandboxConfig(ctx, true, "https://runner.example.com", "", 0, 0, 0, 0, tok, true); err != nil {
		t.Fatal(err)
	}
	refused("the sandbox runner moved without its token", aiBusiness.SetSandboxConfig(ctx, aiAdapter.SetSandboxConfigRequest{
		Enabled: true, RunnerURL: "https://elsewhere.example.net",
	}))
	if err := aiModels.SetCodePRConfig(ctx, true, "https://coder.example.com", "[]", "flag_open", false, false, 0, 0, 0, 0, 0, tok, true); err != nil {
		t.Fatal(err)
	}
	refused("the code-PR runner moved without its token", aiBusiness.SetCodePRConfig(ctx, aiAdapter.SetCodePRConfigRequest{
		Enabled: true, RunnerURL: "https://elsewhere.example.net",
	}))
	s, _ := aiModels.GetSettings(ctx)
	if s.WebSearchBaseURL != "https://search.example.com" || s.SandboxRunnerURL != "https://runner.example.com" || s.CodePRRunnerURL != "https://coder.example.com" {
		t.Errorf("a refused save changed the settings: %q %q %q", s.WebSearchBaseURL, s.SandboxRunnerURL, s.CodePRRunnerURL)
	}

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
	owner := seedPerson(t, env, true)
	actor := dataSourceBusiness.Actor{UserID: owner.UserPostgresInfo.Id, IsAdmin: true}
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
	}, owner.UserPostgresInfo.Id)
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
