//go:build integration
// +build integration

package integration_test

// An app answers its command later, and a press on its card reaches it only
// after the server checks it (platform P-API part 3; API_CONTRACT.md §3.4,
// §3.5, §3.8), against real Postgres and Redis:
//   - a command reaches the app signed, with a response URL;
//   - the app answers at that URL; the invoker is sent the answer, its button
//     sealed for them;
//   - the invoker's press reaches the app signed, with the action, the
//     presser and a fresh response URL; someone else's press, or a press of
//     an action the card hasn't, never reaches it;
//   - a response URL answers five times, then is gone.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAnAppAnswersLaterAndOnlyACheckedPressReachesIt -v

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

	MQTT "github.com/eclipse/paho.mqtt.golang"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	cardsBusiness "github.com/akashc777/OneCamp/business/Cards"
	commandBusiness "github.com/akashc777/OneCamp/business/Command"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

// recordingMqtt keeps what the server publishes (the invoker's late answers)
// and otherwise behaves as a client with no broker.
type recordingMqtt struct {
	MQTT.Client
	mu  sync.Mutex
	out [][]byte
}

func (r *recordingMqtt) Publish(topic string, qos byte, retained bool, payload interface{}) MQTT.Token {
	if b, ok := payload.([]byte); ok {
		r.mu.Lock()
		r.out = append(r.out, b)
		r.mu.Unlock()
	}
	return r.Client.Publish(topic, qos, retained, payload)
}

// answers are the late command answers published so far.
func (r *recordingMqtt) answers() []commandAdapter.CommandResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	var got []commandAdapter.CommandResponse
	for _, b := range r.out {
		var m struct {
			Data commandAdapter.CommandResponse `json:"data"`
		}
		if json.Unmarshal(b, &m) == nil && m.Data.ResponseType != "" {
			got = append(got, m.Data)
		}
	}
	return got
}

type appHeard struct {
	body                 map[string]any
	raw                  []byte
	timestamp, signature string
}

func TestAnAppAnswersLaterAndOnlyACheckedPressReachesIt(t *testing.T) {
	t.Setenv("JWT_SECRET", "command-responses-integration-secret")
	t.Setenv("BACKEND_DOMAIN", "https://api.example.test")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	rec := &recordingMqtt{Client: MQTT.NewClient(MQTT.NewClientOptions().AddBroker("tcp://127.0.0.1:1"))}
	integration.UseMqttClient(t, rec)
	integration.SetupDgraph(t)
	defer commandBusiness.ForwardToThisNetworkForTest()()

	adminID, _, err := userBusiness.CreateUserWithMethod(ctx, "cmd-admin@example.test", "Cmd Admin", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.CreateAdminUser(ctx, "cmd-admin@example.test"); err != nil {
		t.Fatal(err)
	}

	// The app: hears every request, answers nothing at once.
	var mu sync.Mutex
	var heard []appHeard
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		heard = append(heard, appHeard{m, b, r.Header.Get("X-OneCamp-Timestamp"), r.Header.Get("X-OneCamp-Signature")})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer app.Close()
	waitHeard := func(n int, why string) appHeard {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			mu.Lock()
			if len(heard) >= n {
				h := heard[n-1]
				mu.Unlock()
				return h
			}
			mu.Unlock()
			if time.Now().After(deadline) {
				t.Fatalf("%s: the app heard %d requests, not %d", why, len(heard), n)
			}
		}
	}
	heardCount := func() int { mu.Lock(); defer mu.Unlock(); return len(heard) }

	const secret = "deploy-bot-signing-secret"
	view, err := commandBusiness.CreateApp(ctx, commandAdapter.CreateAppRequest{
		Slug: "deploy-bot", Name: "Deploy bot", Kind: "external", HandlerURL: "https://93.184.216.34/cmd", SigningSecret: secret,
		Commands: []commandAdapter.AppCommandInput{{Command: "deploy", Description: "Ship it", ScopeType: "org"}},
	}, adminID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE apps SET handler_url = $1 WHERE id = $2`, app.URL, view.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`UPDATE slash_commands SET handler_url = NULL WHERE app_id = $1`, view.ID); err != nil {
		t.Fatal(err)
	}

	var sam userModels.UserInfo
	sam.UserDgraphInfo.Uuid = adminID.String()

	// 1. The command reaches the app signed, with a response URL.
	if _, err := commandBusiness.Execute(ctx, sam, commandAdapter.ExecuteCommandRequest{Command: "deploy", Text: "web", TriggerID: "trig-deploy"}); err != nil {
		t.Fatal(err)
	}
	first := waitHeard(1, "the command")
	if !webhookBusiness.VerifySignatureV1(secret, first.raw, first.timestamp, first.signature) {
		t.Fatal("the command wasn't signed with the app's secret")
	}
	responseURL, _ := first.body["response_url"].(string)
	if !strings.HasPrefix(responseURL, "https://api.example.test/command-responses/") {
		t.Fatalf("the command's response URL: %q", responseURL)
	}
	path := strings.TrimPrefix(responseURL, "https://api.example.test")

	routes := router.Routes()
	respond := func(body string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		return w.Code
	}

	// 2. The app answers at its URL; the invoker gets it, sealed for them.
	card := `{"response_type": "ephemeral", "blocks": [{"type": "section", "text": {"type": "plain_text", "text": "Deploy web?"}}, {"type": "actions", "elements": [{"type": "button", "text": "Ship", "action_id": "ship", "value": "web"}]}]}`
	if code := respond(card); code != http.StatusOK {
		t.Fatalf("the app's answer: %d", code)
	}
	var token string
	for deadline := time.Now().Add(5 * time.Second); token == "" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		for _, a := range rec.answers() {
			if a.CardToken != "" && a.TriggerID == "trig-deploy" {
				token = a.CardToken
			}
		}
	}
	if token == "" {
		t.Fatalf("the invoker wasn't sent a sealed card: %+v", rec.answers())
	}

	// 3. Someone else's press, and an action the card hasn't, never reach it.
	before := heardCount()
	if _, err := cardsBusiness.Act(ctx, cardsBusiness.Who{UserUUID: "c0ffee00-0000-4000-8000-0000000000aa"}, token, "ship", "web"); !errors.Is(err, cardsBusiness.ErrNotYours) {
		t.Fatalf("someone else's press: %v", err)
	}
	if _, err := cardsBusiness.Act(ctx, cardsBusiness.Who{UserUUID: adminID.String()}, token, "delete-prod", ""); !errors.Is(err, cardsBusiness.ErrNoAction) {
		t.Fatalf("an action the card hasn't: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if heardCount() != before {
		t.Fatal("a refused press reached the app")
	}

	// 4. The invoker's press reaches it signed, with what was pressed.
	if _, err := cardsBusiness.Act(ctx, cardsBusiness.Who{UserUUID: adminID.String()}, token, "ship", "web"); err != nil {
		t.Fatal(err)
	}
	press := waitHeard(before+1, "the press")
	if !webhookBusiness.VerifySignatureV1(secret, press.raw, press.timestamp, press.signature) {
		t.Fatal("the press wasn't signed with the app's secret")
	}
	if press.body["type"] != "block_actions" || press.body["action_id"] != "ship" || press.body["value"] != "web" || press.body["user_id"] != adminID.String() {
		t.Fatalf("the press the app heard: %v", press.body)
	}
	if u, _ := press.body["response_url"].(string); u == "" || u == responseURL {
		t.Fatalf("the press's response URL: %q", u)
	}

	// 5. The first URL answers five times in all, then is gone.
	for i := 2; i <= 5; i++ {
		if code := respond(`{"text": "still going"}`); code != http.StatusOK {
			t.Fatalf("answer %d: %d", i, code)
		}
	}
	if code := respond(`{"text": "one too many"}`); code != http.StatusGone {
		t.Fatalf("a sixth answer: %d, want 410", code)
	}
}
