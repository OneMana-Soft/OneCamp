package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// sceneModel answers each chat with the next of its answers; with hang it
// answers nothing until the request's context ends, and with fail it answers
// fail's error for what it was sent.
type sceneModel struct {
	streamModel
	answers []string
	hang    bool
	fail    func(msgs []ai.ChatMessage) error
	chats   int
}

func (m *sceneModel) Chat(ctx context.Context, msgs []ai.ChatMessage, _ ai.ChatOptions) (string, error) {
	m.chats++
	m.asked = msgs
	if m.fail != nil {
		return "", m.fail(msgs)
	}
	if m.hang {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if m.chats > len(m.answers) {
		return "", ai.ErrCircuitOpen
	}
	return m.answers[m.chats-1], nil
}

// sceneRuntime is fakeRuntime with a model that answers in scenes; cb, when
// set, is the model's breaker, and with gone there is no model to resolve.
type sceneRuntime struct {
	fakeRuntime
	llm  *sceneModel
	cb   *ai.CircuitBreaker
	gone bool
}

func (f sceneRuntime) Resolve(ctx context.Context, user string) (ai.LLMProvider, *ai.CircuitBreaker, ai.ModelLimits, ai.DocModel) {
	if f.gone {
		return nil, nil, ai.ModelLimits{}, ai.DocModel{}
	}
	_, cb, limits, model := f.fakeRuntime.Resolve(ctx, user)
	if f.cb != nil {
		cb = f.cb
	}
	return f.llm, cb, limits, model
}

const madeScene = `{"palette":["#0B1D3A","#2A4B7C","#F4E3B1"],"layout":"lake","extras":["stars"]}`

// askForBackground posts words as a signed-in member and reads the answer.
func askForBackground(t *testing.T, rt sceneRuntime, words string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	saved := docAIRun
	t.Cleanup(func() { docAIRun = saved })
	docAIRun = rt
	rec := httptest.NewRecorder()
	MakeCallBackground(rec, docAIRequest(t, "/ai/call-background", map[string]string{"words": words}))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer isn't JSON (%v): %s", err, rec.Body.String())
	}
	return rec, body
}

// keepLogs sends the server's log lines to a buffer for the test's length.
func keepLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	saved := helpers.Logger
	t.Cleanup(func() { helpers.Logger = saved })
	helpers.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf
}

func TestAMemberGetsASceneFromTheirModel(t *testing.T) {
	model := &sceneModel{answers: []string{madeScene}}
	rec, body := askForBackground(t, sceneRuntime{llm: model}, "  Misty pine forest at dawn  ")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %v", rec.Code, body)
	}
	data, _ := body["data"].(map[string]any)
	recipe, _ := data["recipe"].(map[string]any)
	if palette, _ := recipe["palette"].([]any); len(palette) != 3 || recipe["layout"] != "lake" {
		t.Errorf("the recipe didn't come back: %v", data)
	}
	if m, _ := data["model"].(map[string]any); m["label"] != "qwen3 4B" || m["local"] != true {
		t.Errorf("the answer should name the model that made it: %v", data["model"])
	}
	if model.chats != 1 || !strings.Contains(model.asked[len(model.asked)-1].Content, "Misty pine forest at dawn") {
		t.Errorf("the words should reach the member's model once, trimmed: %d chats, %+v", model.chats, model.asked)
	}
}

func TestTheWordsAreCheckedBeforeAnyModelIsAsked(t *testing.T) {
	for name, c := range map[string]struct {
		words string
		code  int
		msg   string
	}{
		"empty":                {"   ", http.StatusBadRequest, "Describe the background you want."},
		"201 characters":       {strings.Repeat("é", 201), http.StatusBadRequest, "Keep it to 200 characters."},
		"200 characters is ok": {strings.Repeat("é", 200), http.StatusOK, ""},
	} {
		model := &sceneModel{answers: []string{madeScene}}
		rec, body := askForBackground(t, sceneRuntime{llm: model}, c.words)
		if rec.Code != c.code || (c.msg != "" && body["msg"] != c.msg) {
			t.Errorf("%s: %d %v, want %d %q", name, rec.Code, body["msg"], c.code, c.msg)
		}
		if c.code != http.StatusOK && model.chats != 0 {
			t.Errorf("%s: the model was asked %d times", name, model.chats)
		}
	}
}

func TestAIOffOrTooManyAsksRefuseInWords(t *testing.T) {
	model := &sceneModel{answers: []string{madeScene}}
	rec, body := askForBackground(t, sceneRuntime{fakeRuntime: fakeRuntime{off: true}, llm: model}, "Warm library")
	if rec.Code != http.StatusServiceUnavailable || body["msg"] != "AI is switched off in this workspace." {
		t.Errorf("AI off: %d %v", rec.Code, body["msg"])
	}
	rec, body = askForBackground(t, sceneRuntime{fakeRuntime: fakeRuntime{limited: true}, llm: model}, "Warm library")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("rate-limited: %d %v", rec.Code, body["msg"])
	}
	if model.chats != 0 {
		t.Errorf("a refused member's words reached the model %d times", model.chats)
	}
}

func TestAnUnreadableSceneIs503InWordsAndTheLogHasNoWords(t *testing.T) {
	logs := keepLogs(t)
	model := &sceneModel{answers: []string{"A quiet secretive orchard.", "Still an orchard, in words."}}
	rec, body := askForBackground(t, sceneRuntime{llm: model}, "Quiet secretive orchard")
	if rec.Code != http.StatusServiceUnavailable || body["msg"] != "Couldn't make one just now." {
		t.Errorf("unreadable twice: %d %v, want 503 (Cloudflare turns a 502 into Network Error)", rec.Code, body["msg"])
	}
	if model.chats != 2 {
		t.Errorf("the model was asked %d times, want 2", model.chats)
	}
	if !strings.Contains(logs.String(), "unreadable") {
		t.Errorf("the failure should be in the log: %q", logs.String())
	}
	if strings.Contains(strings.ToLower(logs.String()), "orchard") {
		t.Errorf("the log keeps the person's words or the model's: %q", logs.String())
	}
}

func TestASlowModelIsCutOffAt503InWords(t *testing.T) {
	saved := callBackgroundDeadline
	t.Cleanup(func() { callBackgroundDeadline = saved })
	callBackgroundDeadline = 50 * time.Millisecond
	logs := keepLogs(t)
	start := time.Now()
	rec, body := askForBackground(t, sceneRuntime{llm: &sceneModel{hang: true}}, "Night sky over quiet hills")
	if rec.Code != http.StatusServiceUnavailable || body["msg"] != "Couldn't make one just now." {
		t.Errorf("a timeout: %d %v", rec.Code, body["msg"])
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the deadline didn't stop the model: %v", time.Since(start))
	}
	if strings.Contains(strings.ToLower(logs.String()), "quiet hills") {
		t.Errorf("the log keeps the person's words: %q", logs.String())
	}
}

func TestTheServerDeadlineIsUnderCloudflares100Seconds(t *testing.T) {
	if callBackgroundDeadline <= 0 || callBackgroundDeadline >= 100*time.Second {
		t.Errorf("deadline %v: a model call must end before Cloudflare's 100 s", callBackgroundDeadline)
	}
}

// A person over their own or the workspace's daily budget is told so, in the
// words doc AI uses, as a 429: a 503 would have them retry in vain.
func TestAPersonOverTheDailyBudgetIsToldSoIn429(t *testing.T) {
	for _, over := range []error{ai.ErrUserTokenBudgetExceeded, ai.ErrWorkspaceTokenBudgetExceeded} {
		model := &sceneModel{fail: func([]ai.ChatMessage) error { return fmt.Errorf("metering: %w", over) }}
		rec, body := askForBackground(t, sceneRuntime{llm: model}, "Warm library")
		if want := ai.FriendlyProviderError(over); rec.Code != http.StatusTooManyRequests || body["msg"] != want {
			t.Errorf("%v: %d %v, want 429 %q", over, rec.Code, body["msg"], want)
		}
	}
}

// An open breaker answers at once: the model it guards isn't asked.
func TestAnOpenBreakerAnswersWithoutAskingTheModel(t *testing.T) {
	saved := helpers.MessageLogs
	t.Cleanup(func() { helpers.MessageLogs = saved })
	quiet := log.New(io.Discard, "", 0)
	helpers.MessageLogs = &helpers.Message{InfoLog: quiet, ErrorLog: quiet}
	cb := ai.NewCircuitBreaker(1, time.Hour)
	cb.RecordFailure()
	model := &sceneModel{answers: []string{madeScene}}
	rec, body := askForBackground(t, sceneRuntime{llm: model, cb: cb}, "Warm library")
	if want := ai.FriendlyProviderError(ai.ErrCircuitOpen); rec.Code != http.StatusServiceUnavailable || body["msg"] != want {
		t.Errorf("open breaker: %d %v, want 503 %q", rec.Code, body["msg"], want)
	}
	if model.chats != 0 {
		t.Errorf("the model behind an open breaker was asked %d times", model.chats)
	}
}

// Ruling 10: a provider's error can quote what it was sent, and no log line
// may carry the person's words, so the failure is logged in our own words.
func TestAProviderErrorQuotingTheWordsNeverReachesTheLog(t *testing.T) {
	logs := keepLogs(t)
	model := &sceneModel{fail: func(msgs []ai.ChatMessage) error {
		return fmt.Errorf("status 400: model said: cannot parse %q", msgs[len(msgs)-1].Content)
	}}
	rec, body := askForBackground(t, sceneRuntime{llm: model}, "Velvet cathedral orchard")
	if rec.Code != http.StatusServiceUnavailable || body["msg"] != "Couldn't make one just now." {
		t.Errorf("a provider error: %d %v", rec.Code, body["msg"])
	}
	if !strings.Contains(logs.String(), "no recipe") {
		t.Errorf("the failure should be in the log: %q", logs.String())
	}
	if strings.Contains(strings.ToLower(logs.String()), "velvet") {
		t.Errorf("the log keeps the person's words: %q", logs.String())
	}
}

// Resolve answers no model when AI is switched off between the gate and the
// ask: the person is told so, as doc AI tells them, and nothing crashes.
func TestAModelGoneAfterTheGateIsAIOffNotACrash(t *testing.T) {
	rec, body := askForBackground(t, sceneRuntime{gone: true}, "Warm library")
	if rec.Code != http.StatusServiceUnavailable || body["msg"] != "AI is switched off in this workspace." {
		t.Errorf("no model after the gate: %d %v", rec.Code, body["msg"])
	}
}
