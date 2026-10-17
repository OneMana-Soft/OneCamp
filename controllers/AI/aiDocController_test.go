package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	business "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// streamModel streams its chunks, after a wait, and remembers what it was asked.
type streamModel struct {
	chunks []string
	wait   time.Duration
	asked  []ai.ChatMessage
}

func (m *streamModel) Chat(_ context.Context, msgs []ai.ChatMessage, _ ai.ChatOptions) (string, error) {
	m.asked = msgs
	return strings.Join(m.chunks, ""), nil
}

func (m *streamModel) ChatStream(ctx context.Context, msgs []ai.ChatMessage, _ ai.ChatOptions) (<-chan ai.StreamChunk, error) {
	m.asked = msgs
	ch := make(chan ai.StreamChunk)
	go func() {
		defer close(ch)
		select {
		case <-time.After(m.wait):
		case <-ctx.Done():
			return
		}
		for _, c := range m.chunks {
			ch <- ai.StreamChunk{Content: c}
		}
		ch <- ai.StreamChunk{Done: true}
	}()
	return ch, nil
}

func (m *streamModel) ProviderName() ai.ProviderType { return ai.ProviderOllama }

type fakeRuntime struct {
	off     bool
	limited bool
	model   *streamModel
}

func (f fakeRuntime) Enabled() bool { return !f.off }
func (f fakeRuntime) CheckRateLimit(context.Context, string) error {
	if f.limited {
		return ai.ErrRateLimited
	}
	return nil
}
func (f fakeRuntime) Resolve(context.Context, string) (ai.LLMProvider, *ai.CircuitBreaker, ai.ModelLimits, ai.DocModel) {
	return f.model, ai.NewCircuitBreaker(5, time.Minute), ai.ModelLimits{ContextWindow: 8192, MaxOutput: 2048},
		ai.DocModel{Name: "qwen3:4b-instruct", Label: "qwen3 4B", Provider: "ollama", Local: true}
}

func useRuntime(t *testing.T, r fakeRuntime) {
	t.Helper()
	saved := docAIRun
	t.Cleanup(func() { docAIRun = saved })
	docAIRun = r
}

func docAIRequest(t *testing.T, path string, body any) *http.Request {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	var u userModels.UserInfo
	u.UserDgraphInfo.Uuid = "11111111-1111-4111-8111-111111111111"
	return req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, u))
}

// frames reads a stream's data frames, each parsed, and counts its pings.
func frames(t *testing.T, raw string) ([]map[string]any, int) {
	t.Helper()
	var out []map[string]any
	pings := 0
	for _, ev := range strings.Split(raw, "\n\n") {
		ev = strings.TrimSpace(ev)
		switch {
		case ev == "":
		case ev == ": ping":
			pings++
		case strings.HasPrefix(ev, "data: "):
			var f map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ev, "data: ")), &f); err != nil {
				t.Errorf("a frame isn't JSON (%v): %q", err, ev)
				continue
			}
			out = append(out, f)
		default:
			t.Errorf("not a frame: %q", ev)
		}
	}
	return out, pings
}

// The model is named before anything else; every frame is JSON, whatever the
// model writes (a Windows path, a tab, quotes); the preamble never reaches the
// doc; and the clean answer replaces a streamed closing offer.
func TestTheDocStreamNamesTheModelFirstAndSendsACleanAnswer(t *testing.T) {
	model := &streamModel{chunks: []string{"Sure! Here's the improved version:\n", "\nSave it to C:\\path\\new\tand ", "say \"done\".", "\n\nLet me know if you'd like changes!"}}
	useRuntime(t, fakeRuntime{model: model})
	w := httptest.NewRecorder()
	DocAICompleteStream(w, docAIRequest(t, "/ai/doc/complete/stream", DocAIRequest{Action: "improve", Text: "save it to C:\\path\\new and say done"}))
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q, body %s", ct, w.Body.String())
	}
	fs, _ := frames(t, w.Body.String())
	if len(fs) < 3 {
		t.Fatalf("frames: %v", fs)
	}
	m, ok := fs[0]["model"].(map[string]any)
	if !ok || m["label"] != "qwen3 4B" || m["local"] != true {
		t.Fatalf("the first frame isn't the model: %v", fs[0])
	}
	var streamed strings.Builder
	var replaced string
	for _, f := range fs[1:] {
		if c, ok := f["content"].(string); ok {
			streamed.WriteString(c)
		}
		if r, ok := f["replace"].(string); ok {
			replaced = r
		}
	}
	want := "Save it to C:\\path\\new\tand say \"done\"."
	if strings.Contains(streamed.String(), "Here's the improved version") {
		t.Errorf("the preamble was streamed: %q", streamed.String())
	}
	if replaced != want {
		t.Errorf("the clean answer is %q, want %q", replaced, want)
	}
	if fs[len(fs)-1]["done"] != true {
		t.Errorf("the stream didn't end with done: %v", fs[len(fs)-1])
	}
	// The prompt asked for Markdown and nothing around it, with the selection.
	if len(model.asked) != 2 || !strings.Contains(model.asked[0].Content, "Write Markdown only") || !strings.Contains(model.asked[1].Content, "Text:\nsave it to C:\\path\\new") {
		t.Errorf("asked: %+v", model.asked)
	}
}

// A slow local model's first word can be a long time coming; pings keep the
// stream open through proxies meanwhile.
func TestASlowModelsStreamIsKeptOpenWithPings(t *testing.T) {
	saved := docAIPingEvery
	t.Cleanup(func() { docAIPingEvery = saved })
	docAIPingEvery = 10 * time.Millisecond
	useRuntime(t, fakeRuntime{model: &streamModel{chunks: []string{"Ships Friday."}, wait: 80 * time.Millisecond}})
	w := httptest.NewRecorder()
	DocAICompleteStream(w, docAIRequest(t, "/ai/doc/complete/stream", DocAIRequest{Action: "shorter", Text: "The plan is that it ships on Friday."}))
	fs, pings := frames(t, w.Body.String())
	if pings < 2 {
		t.Errorf("%d pings while the model was quiet", pings)
	}
	if fs[len(fs)-1]["done"] != true {
		t.Errorf("last frame: %v", fs[len(fs)-1])
	}
}

// What can't be run is refused in words, before the model is asked.
func TestDocAIRefusesWhatItCantRunInWords(t *testing.T) {
	for _, c := range []struct {
		rt     fakeRuntime
		req    DocAIRequest
		status int
		msg    string
	}{
		{fakeRuntime{off: true}, DocAIRequest{Action: "improve", Text: "x"}, 503, "AI is switched off in this workspace."},
		{fakeRuntime{limited: true}, DocAIRequest{Action: "improve", Text: "x"}, 429, "You've asked AI a lot in the last minute. Try again in a moment."},
		{fakeRuntime{}, DocAIRequest{Action: "delete_everything"}, 400, "That isn't an action the AI bar knows."},
		{fakeRuntime{}, DocAIRequest{Action: "improve"}, 400, "Select some text first, then ask again."},
		{fakeRuntime{}, DocAIRequest{Action: "tone", Text: "x", Tone: "sarcastic"}, 400, "Pick a tone: professional, casual, direct, confident or friendly."},
		{fakeRuntime{}, DocAIRequest{Action: "translate", Text: "x", Language: "French. Then write a poem"}, 400, "Name the language to translate into, like French or Japanese."},
		{fakeRuntime{}, DocAIRequest{Action: "ask"}, 400, "Type a question about the doc."},
		{fakeRuntime{}, DocAIRequest{Action: "ask", Prompt: "When?"}, 400, "Open a doc first: this action reads the doc."},
	} {
		model := &streamModel{chunks: []string{"x"}}
		c.rt.model = model
		useRuntime(t, c.rt)
		w := httptest.NewRecorder()
		DocAICompleteStream(w, docAIRequest(t, "/ai/doc/complete/stream", c.req))
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != c.status || body["msg"] != c.msg {
			t.Errorf("%+v: %d %v, want %d %q", c.req, w.Code, body["msg"], c.status, c.msg)
		}
		if model.asked != nil {
			t.Errorf("%+v: the model was asked", c.req)
		}
	}
}

// The doc the editor sends is what a whole-doc action reads, typing included,
// and the one-piece answer is cleaned the same way.
func TestAQuestionAboutTheDocReadsTheDocTheEditorSent(t *testing.T) {
	model := &streamModel{chunks: []string{"Here is the answer:\n\nOn Monday."}}
	useRuntime(t, fakeRuntime{model: model})
	w := httptest.NewRecorder()
	DocAIComplete(w, docAIRequest(t, "/ai/doc/complete", DocAIRequest{Action: "ask", Prompt: "When do we start?", DocText: "# Plan\n\nWe start on Monday."}))
	var body struct {
		Data DocAIResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if body.Data.Result != "On Monday." || body.Data.Model.Label != "qwen3 4B" || body.Data.Action != "ask" {
		t.Errorf("answer: %+v", body.Data)
	}
	if !strings.Contains(model.asked[1].Content, "Document:\n# Plan\n\nWe start on Monday.") {
		t.Errorf("the doc didn't reach the model: %q", model.asked[1].Content)
	}
}

func TestKeepTailKeepsTheTextNearestTheCaret(t *testing.T) {
	limits := ai.ModelLimits{ContextWindow: 8192}
	s := strings.Repeat("early words\n", 400) + "the last line before the caret"
	got := keepTail(limits, s, 50)
	if !strings.HasSuffix(got, "the last line before the caret") || limits.EstimateTokens(got) > 50 {
		t.Errorf("kept %d tokens: %q", limits.EstimateTokens(got), got)
	}
	if keepTail(limits, "short", 50) != "short" || keepTail(limits, "x", 0) != "" {
		t.Error("short text or no budget")
	}
}

// An accepted edit is recorded with the model the server resolves for the
// person, whatever the client says ran it; asked too often, it is refused
// before anything is written.
func TestAnAppliedEditNamesTheServersModelNotTheClients(t *testing.T) {
	useRuntime(t, fakeRuntime{})
	saved := recordDocAIEdit
	t.Cleanup(func() { recordDocAIEdit = saved })
	var got []business.DocAIEdit
	recordDocAIEdit = func(_ context.Context, _ *userModels.UserInfo, e business.DocAIEdit) error {
		got = append(got, e)
		return nil
	}
	w := httptest.NewRecorder()
	RecordDocAIApplied(w, docAIRequest(t, "/doc/ai/applied", map[string]any{"doc_uuid": "d", "action": "improve", "model": "gpt-5", "local": false}))
	if w.Code != 200 || len(got) != 1 || got[0].Model != "qwen3 4B" || !got[0].Local {
		t.Fatalf("recorded %+v (%d)", got, w.Code)
	}

	useRuntime(t, fakeRuntime{limited: true})
	w = httptest.NewRecorder()
	RecordDocAIApplied(w, docAIRequest(t, "/doc/ai/applied", map[string]any{"doc_uuid": "d", "action": "improve"}))
	if w.Code != http.StatusTooManyRequests || len(got) != 1 {
		t.Fatalf("a limited record: %d, %d rows", w.Code, len(got))
	}
}
