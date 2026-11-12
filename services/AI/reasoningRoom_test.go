package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// modelHost stands in for a model host: it keeps the body of every chat request it was sent and answers
// it the way the real host does (an OpenAI-family answer or stream, an Ollama answer or stream, and Ollama's
// capabilities lookup from caps), or, when the test sets refusal, answers every chat request 400 refusal.
type modelHost struct {
	*httptest.Server
	mu      sync.Mutex
	last    []byte
	chats   [][]byte
	refusal string
}

func newModelHost(t *testing.T, caps map[string][]string) *modelHost {
	t.Helper()
	h := &modelHost{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var in struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(b, &in)
		if r.URL.Path == "/api/show" {
			c, ok := caps[in.Model]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string][]string{"capabilities": c})
			return
		}
		h.mu.Lock()
		h.last, h.chats = b, append(h.chats, b)
		refusal := h.refusal
		h.mu.Unlock()
		switch {
		case refusal != "":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, refusal)
		case r.URL.Path == "/api/chat" && in.Stream:
			_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"ok"},"done":true}`+"\n")
		case r.URL.Path == "/api/chat":
			_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"ok"},"done":true}`)
		case in.Stream:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"plan\":[]}"}}]}`)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

// sent decodes the last chat request the host was sent into v.
func (h *modelHost) sent(t *testing.T, v any) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last == nil {
		t.Fatal("the host was sent no chat request")
	}
	if err := json.Unmarshal(h.last, v); err != nil {
		t.Fatalf("decode the request: %v", err)
	}
}

var roomAsk = []ChatMessage{{Role: "user", Content: "plan my week"}}

// openAIEntryPoints are the four ways OneCamp calls an OpenAI-family model.
var openAIEntryPoints = []struct {
	name string
	call func(p *OpenAIProvider, ask int) error
}{
	{"Chat", func(p *OpenAIProvider, ask int) error {
		_, err := p.Chat(context.Background(), roomAsk, ChatOptions{MaxTokens: ask, JSONMode: true})
		return err
	}},
	{"ChatWithTools", func(p *OpenAIProvider, ask int) error {
		tools := []ToolSpec{{Name: "list_tasks", Description: "List my open tasks", Parameters: map[string]interface{}{"type": "object"}}}
		_, _, err := p.ChatWithTools(context.Background(), roomAsk, tools, ChatOptions{MaxTokens: ask})
		return err
	}},
	{"ChatStream", func(p *OpenAIProvider, ask int) error {
		ch, err := p.ChatStream(context.Background(), roomAsk, ChatOptions{MaxTokens: ask})
		for c := range ch {
			if c.Error != nil {
				return c.Error
			}
		}
		return err
	}},
	{"DescribeImages", func(p *OpenAIProvider, ask int) error {
		_, err := p.DescribeImages(context.Background(), []ImageInput{{Data: []byte("png"), MIME: "image/png"}}, "describe it", ChatOptions{MaxTokens: ask})
		return err
	}},
}

// A reasoning model thinks out of the same max_completion_tokens it answers from, so a caller that sizes its
// ask for the answer alone gets an empty answer, and a host in JSON mode refuses it with 400
// json_validate_failed (V-811: every "Plan my week" on the demo's gpt-oss-20b). Every call to a model that
// thinks, through each of the four ways OneCamp calls one, gets room to think on top of what its caller asked
// for; a model that doesn't think is sent exactly the caller's ask, with the admin's thinking switch on or off;
// the ceiling an admin recorded still caps what is sent; and asking for nothing still sends nothing.
func TestAReasoningModelGetsRoomToThinkOnEveryCall(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		kind      ProviderType
		reasoning bool
		ceiling   int
		ask       int
		want      int // 0 = the field is omitted
	}{
		{"gpt-oss on a compatible host", "openai/gpt-oss-20b", ProviderOpenAICompatible, false, 0, 400, 1424},
		{"deepseek-r1 on a compatible host", "deepseek-r1-distill-llama-70b", ProviderOpenAICompatible, false, 0, 400, 1424},
		{"OpenAI's own reasoning family", "o3-mini", ProviderOpenAI, false, 0, 400, 1424},
		{"thinking turned on by the admin", "qwen/qwen3-32b", ProviderOpenAICompatible, true, 0, 400, 1424},
		{"a model that doesn't think", "llama-3.3-70b-versatile", ProviderOpenAICompatible, false, 0, 400, 400},
		{"a model that doesn't think, with thinking on", "llama-3.3-70b-versatile", ProviderOpenAICompatible, true, 0, 400, 400},
		{"qwen3 with thinking off", "qwen/qwen3-32b", ProviderOpenAICompatible, false, 0, 400, 400},
		{"the recorded ceiling still caps it", "openai/gpt-oss-20b", ProviderOpenAICompatible, false, 1000, 400, 1000},
		{"asking for nothing sends nothing", "openai/gpt-oss-20b", ProviderOpenAICompatible, false, 0, 0, 0},
	}
	for _, ep := range openAIEntryPoints {
		for _, c := range cases {
			if c.ask == 0 && ep.name == "DescribeImages" {
				continue // a picture always asks for something: its own default when the caller names none
			}
			t.Run(ep.name+"/"+c.name, func(t *testing.T) {
				host := newModelHost(t, nil)
				p := NewOpenAICompatibleProviderWithOpts("k", c.model, "", host.URL, 0, c.kind, ProviderOptions{})
				p.reasoning, p.maxOutput = c.reasoning, c.ceiling
				if err := ep.call(p, c.ask); err != nil {
					t.Fatalf("%s: %v", ep.name, err)
				}
				var sent struct {
					MaxTokens *int `json:"max_completion_tokens"`
				}
				host.sent(t, &sent)
				got := 0
				if sent.MaxTokens != nil {
					got = *sent.MaxTokens
				}
				if got != c.want {
					t.Errorf("asked for %d, sent max_completion_tokens %d, want %d", c.ask, got, c.want)
				}
			})
		}
	}
}

// ollamaEntryPoints are the three ways OneCamp calls an Ollama model.
var ollamaEntryPoints = []struct {
	name string
	call func(p *OllamaProvider, ask int) error
}{
	{"Chat", func(p *OllamaProvider, ask int) error {
		_, err := p.Chat(context.Background(), roomAsk, ChatOptions{MaxTokens: ask})
		return err
	}},
	{"ChatStream", func(p *OllamaProvider, ask int) error {
		ch, err := p.ChatStream(context.Background(), roomAsk, ChatOptions{MaxTokens: ask})
		for c := range ch {
			if c.Error != nil {
				return c.Error
			}
		}
		return err
	}},
	{"DescribeImages", func(p *OllamaProvider, ask int) error {
		_, err := p.DescribeImages(context.Background(), []ImageInput{{Data: []byte("png"), MIME: "image/png"}}, "describe it", ChatOptions{MaxTokens: ask})
		return err
	}},
}

// Ollama's thinking is generated tokens too, spent from the same num_predict as the answer, so a call that
// thinks on Ollama gets the same room by the same rule: a family that always thinks (gpt-oss, whatever the
// think flag says), or thinking switched on for a model the engine says can think. The think flag and the
// room travel together on every call, the picture's too, so what the model is told and what it is given agree.
// Thinking off follows the family table as on an OpenAI-family host: gpt-oss ignores think false and thinks at
// its default, so it is sent its level "low" (V-811b); every other family is sent false.
func TestAnOllamaModelThatThinksGetsTheSameRoomOnEveryCall(t *testing.T) {
	caps := map[string][]string{
		"gpt-oss:20b":       {"completion", "tools", "thinking"},
		"qwen3:8b":          {"completion", "tools", "thinking"},
		"qwen3:4b-instruct": {"completion", "tools"},
		"llama3.3":          {"completion", "tools"},
	}
	cases := []struct {
		name      string
		model     string
		reasoning bool
		want      int
		think     string // the think field sent, as JSON (true, false, "low"), or "omitted"
	}{
		{"gpt-oss thinks whatever it is told, least with thinking off", "gpt-oss:20b", false, 1424, `"low"`},
		{"gpt-oss with thinking on", "gpt-oss:20b", true, 1424, "true"},
		{"qwen3 with thinking on", "qwen3:8b", true, 1424, "true"},
		{"qwen3 with thinking off", "qwen3:8b", false, 400, "false"},
		{"a qwen3 the engine says can't think", "qwen3:4b-instruct", true, 400, "omitted"},
		{"a model that doesn't think, with thinking on", "llama3.3", true, 400, "omitted"},
	}
	for _, ep := range ollamaEntryPoints {
		for _, c := range cases {
			t.Run(ep.name+"/"+c.name, func(t *testing.T) {
				host := newModelHost(t, caps)
				p := &OllamaProvider{host: host.URL, model: c.model, client: host.Client(), reasoning: c.reasoning}
				if err := ep.call(p, 400); err != nil {
					t.Fatalf("%s: %v", ep.name, err)
				}
				var sent struct {
					Think   json.RawMessage `json:"think"`
					Options struct {
						NumPredict int `json:"num_predict"`
					} `json:"options"`
				}
				host.sent(t, &sent)
				if sent.Options.NumPredict != c.want {
					t.Errorf("asked for 400, sent num_predict %d, want %d", sent.Options.NumPredict, c.want)
				}
				think := "omitted"
				if sent.Think != nil {
					think = string(sent.Think)
				}
				if think != c.think {
					t.Errorf("sent think %s, want %s", think, c.think)
				}
			})
		}
	}
}

// The prompt fitter and the wire read one number. A host that checks prompt + max_completion_tokens against
// the window (OpenAI, vLLM, Groq) refuses a request whose prompt was fitted to leave room for the ask alone
// when the call sends the ask plus room to think, and the refit after that refusal reserves the same too
// little, so a long chat or a large document on a thinking model fails where it used to be sent (CR-B53's
// probe: 777 over a 16,384 window). Built the way the resolver builds a member's model, the window holds
// back exactly what the call sends, and a fitted prompt plus that fits, on a model that thinks as on one
// that doesn't.
func TestAFittedPromptAndWhatItsCallSendsFitTheWindow(t *testing.T) {
	const window = 16384
	models := []struct {
		name      string
		model     string
		reasoning bool
		ceiling   int
		room      int // what the call sends on top of the ask, before the ceiling
	}{
		{"gpt-oss", "openai/gpt-oss-20b", false, 0, 1024},
		{"qwen3 with thinking on", "qwen/qwen3-32b", true, 0, 1024},
		{"a model that doesn't think", "llama-3.3-70b-versatile", false, 0, 0},
		{"a model that doesn't think, with thinking on", "llama-3.3-70b-versatile", true, 0, 0},
		{"gpt-oss under a recorded ceiling", "openai/gpt-oss-20b", false, 1500, 1024},
	}
	asks := []struct {
		name      string
		ask       int
		sysRepeat int
	}{{"ask 1024", 1024, 1}, {"ask 1024, a long system prompt", 1024, 90}, {"ask 2048", 2048, 1}}
	for _, m := range models {
		for _, a := range asks {
			t.Run(m.name+"/"+a.name, func(t *testing.T) {
				host := newModelHost(t, nil)
				ep := Endpoint{Kind: ProviderOpenAICompatible, BaseURL: host.URL, APIKey: "k", Model: m.model}
				rm, err := newModelResolver().get("fit|"+m.model, ep, &AIConfig{ReasoningEnabled: m.reasoning}, func() ModelLimits {
					return LimitsForModel(string(ProviderOpenAICompatible), m.model, window, m.ceiling)
				})
				if err != nil {
					t.Fatalf("build the model: %v", err)
				}
				ctx := WithModelLimits(context.Background(), rm.limits)
				_, msgs, err := ChatFitted(ctx, rm.llm, ChatOptions{MaxTokens: a.ask}, longChat(strings.Repeat("Answer from the workspace context and cite it. ", a.sysRepeat)))
				if err != nil {
					t.Fatalf("send: %v", err)
				}
				var sent struct {
					MaxTokens int `json:"max_completion_tokens"`
				}
				host.sent(t, &sent)
				prompt := rm.limits.EstimateMessagesTokens(msgs)
				if prompt+sent.MaxTokens > window {
					t.Errorf("fitted prompt ~%d + sent %d = %d > window %d", prompt, sent.MaxTokens, prompt+sent.MaxTokens, window)
				}
				// The switch the resolver was handed reaches the client, so a model that thinks is sent its room.
				want := a.ask + m.room
				if m.ceiling > 0 {
					want = min(want, m.ceiling)
				}
				if sent.MaxTokens != want {
					t.Errorf("asked for %d, sent %d, want %d", a.ask, sent.MaxTokens, want)
				}
				if held := rm.limits.ResponseReserve(a.ask); held != sent.MaxTokens {
					t.Errorf("the window holds back %d for the answer, the call sends %d", held, sent.MaxTokens)
				}
			})
		}
	}
}

// longChat is a conversation that fills any window: a long history and a large document.
func longChat(system string) PromptParts {
	var hist []ChatMessage
	for i := 0; i < 400; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		hist = append(hist, ChatMessage{Role: role, Content: strings.Repeat("the launch review moved to thursday afternoon ", 20)})
	}
	return PromptParts{
		System:   func(int) string { return system },
		History:  hist,
		Context:  strings.Repeat("A long document section about the launch plan and its owners. ", 4000),
		Question: "What changed in the launch plan?",
	}
}

// An ask is what a caller asks for; what it holds back from the window is the ask plus the room its call sends.
// No ask holds back the default share and sends no cap, so it gets no room. Anthropic's client sends the bare
// ask (OneCamp never sends that API's thinking parameter), so its window holds back the bare ask too.
func TestAnAskHoldsBackWhatItsCallSends(t *testing.T) {
	cases := []struct {
		name      string
		lim       ModelLimits
		ask       int
		reserve   int
		askTokens int
	}{
		{"gpt-oss", ModelLimits{Provider: "openai_compatible", Model: "openai/gpt-oss-20b"}, 1024, 2048, 1024},
		{"gpt-oss, no ask", ModelLimits{Provider: "openai_compatible", Model: "openai/gpt-oss-20b"}, 0, 1024, 1024},
		{"gpt-oss under a recorded ceiling", ModelLimits{Provider: "openai_compatible", Model: "openai/gpt-oss-20b", MaxOutput: 1500}, 1024, 1500, 1024},
		{"qwen3 with thinking on", ModelLimits{Provider: "openai_compatible", Model: "qwen/qwen3-32b", Reasoning: true}, 400, 1424, 400},
		{"qwen3 with thinking off", ModelLimits{Provider: "openai_compatible", Model: "qwen/qwen3-32b"}, 400, 400, 400},
		{"a model that doesn't think, with thinking on", ModelLimits{Provider: "openai_compatible", Model: "llama-3.3-70b-versatile", Reasoning: true}, 400, 400, 400},
		{"OpenAI's own reasoning family", ModelLimits{Provider: "openai", Model: "o3-mini"}, 400, 1424, 400},
		{"gpt-oss on Ollama", ModelLimits{Provider: "ollama", Model: "gpt-oss:20b"}, 400, 1424, 400},
		{"a family that always thinks, behind Anthropic's API", ModelLimits{Provider: "anthropic", Model: "deepseek-r1"}, 400, 400, 400},
	}
	for _, c := range cases {
		if got := c.lim.ResponseReserve(c.ask); got != c.reserve {
			t.Errorf("%s: ResponseReserve(%d) = %d, want %d", c.name, c.ask, got, c.reserve)
		}
		if got := c.lim.AnswerTokens(c.ask); got != c.askTokens {
			t.Errorf("%s: AnswerTokens(%d) = %d, want %d", c.name, c.ask, got, c.askTokens)
		}
	}
}

// A family an operator adds through OLLAMA_THINKING_MODELS can think: it gets the room while thinking is on,
// on either provider, and its bare ask while thinking is off.
func TestAFamilyAnOperatorAddsThinksOnlyWhileSwitchedOn(t *testing.T) {
	t.Setenv("OLLAMA_THINKING_MODELS", " exaone-deep ,")
	if !modelSupportsThinking("EXAONE-Deep:7.8b") || modelAlwaysThinks("exaone-deep:7.8b") {
		t.Fatal("an added family can think and doesn't always think")
	}
	for _, reasoning := range []bool{true, false} {
		want := 400
		if reasoning {
			want = 1424
		}
		p := NewOpenAICompatibleProviderWithOpts("k", "exaone-deep:7.8b", "", "http://x", 0, ProviderOpenAICompatible, ProviderOptions{})
		p.reasoning = reasoning
		if got := p.outputBudget("exaone-deep:7.8b", 400); got != want {
			t.Errorf("thinking %v: sent %d, want %d", reasoning, got, want)
		}
		lim := ModelLimits{Provider: "ollama", Model: "exaone-deep:7.8b", Reasoning: reasoning}
		if got := lim.ResponseReserve(400); got != want {
			t.Errorf("thinking %v: held back %d, want %d", reasoning, got, want)
		}
	}
}

// groqRefusal is Groq's answer when JSON mode is on and the model wrote nothing: gpt-oss spent the whole
// output budget thinking, so the host's JSON check had an empty generation to read (V-811b: 3 of 3 on the demo).
const groqRefusal = `{"error":{"message":"Failed to generate JSON. Please adjust your prompt. See 'failed_generation' for more details.","type":"invalid_request_error","code":"json_validate_failed","failed_generation":""}}`

// groqHost stands in for Groq's OpenAI-compatible endpoint serving a model that always thinks. At the endpoint's
// default effort (none sent, or "medium") the model spends the whole budget thinking and the host answers
// refusal (groqRefusal unless the test sets another body) with status (400 unless the test sets another); at
// "low" or "none" it answers a plan, unless the test sets thinksOut: the model writes nothing at every effort,
// its least too. It keeps the reasoning_effort ("" for none) and max_completion_tokens of every request it was sent.
type groqHost struct {
	*httptest.Server
	mu        sync.Mutex
	refusal   string
	status    int
	thinksOut bool
	efforts   []string
	budgets   []int
}

func newGroqHost(t *testing.T) *groqHost {
	t.Helper()
	h := &groqHost{refusal: groqRefusal, status: http.StatusBadRequest}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Effort *string `json:"reasoning_effort"`
			Budget int     `json:"max_completion_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		effort := ""
		if in.Effort != nil {
			effort = *in.Effort
		}
		h.mu.Lock()
		h.efforts, h.budgets = append(h.efforts, effort), append(h.budgets, in.Budget)
		refusal, status, thinksOut := h.refusal, h.status, h.thinksOut
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if thinksOut || effort == "" || effort == "medium" {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, refusal)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"plan\":[]}"}}],"usage":{"prompt_tokens":40,"completion_tokens":300}}`)
	}))
	t.Cleanup(h.Close)
	return h
}

// sentEfforts is the reasoning_effort of every request the host was sent, "" for none.
func (h *groqHost) sentEfforts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.efforts...)
}

// A model that always thinks spends its thinking from the same budget it answers from, and at its host's
// default effort gpt-oss spends all of it (V-811b: "Plan my week" on the demo's gpt-oss-20b never answered). With
// the admin's thinking switch off, every call to a family that takes an effort is sent the one that makes it
// think least, from its row of the family table: gpt-oss "low" (it takes low|medium|high and can't stop
// thinking), qwen3 on Groq "none" (only Groq's host takes it). With the switch on, nothing is sent and the host's
// default applies. A model whose family takes no effort is sent none, and OpenAI's own reasoning family is
// unchanged (it isn't on the table).
func TestAModelThatTakesAnEffortIsToldToThinkLeastWithThinkingOff(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		kind      ProviderType
		groq      bool
		reasoning bool
		want      string // the reasoning_effort sent, "" = the field is omitted
	}{
		{"gpt-oss-20b", "gpt-oss-20b", ProviderOpenAICompatible, true, false, "low"},
		{"openai/gpt-oss-20b", "openai/gpt-oss-20b", ProviderOpenAICompatible, true, false, "low"},
		{"gpt-oss off Groq", "openai/gpt-oss-120b", ProviderOpenAICompatible, false, false, "low"},
		{"gpt-oss with thinking on", "openai/gpt-oss-20b", ProviderOpenAICompatible, true, true, ""},
		{"qwen3 on Groq", "qwen/qwen3-32b", ProviderOpenAICompatible, true, false, "none"},
		{"qwen3 on Groq with thinking on", "qwen/qwen3-32b", ProviderOpenAICompatible, true, true, ""},
		{"qwen3 off Groq", "qwen/qwen3-32b", ProviderOpenAICompatible, false, false, ""},
		{"a qwen that doesn't think, on Groq", "qwen-2.5-32b", ProviderOpenAICompatible, true, false, ""},
		{"a family that takes no effort", "deepseek-r1-distill-llama-70b", ProviderOpenAICompatible, true, false, ""},
		{"a model that doesn't think", "llama-3.3-70b-versatile", ProviderOpenAICompatible, true, false, ""},
		{"OpenAI's o-series", "o3-mini", ProviderOpenAI, false, false, ""},
		{"OpenAI's gpt-5", "gpt-5", ProviderOpenAI, false, false, ""},
	}
	for _, ep := range openAIEntryPoints {
		for _, c := range cases {
			t.Run(ep.name+"/"+c.name, func(t *testing.T) {
				host := newModelHost(t, nil)
				base := host.URL + "/v1"
				if c.groq {
					base = host.URL + "/GROQ/openai/v1" // a Groq host's base URL names it, as api.groq.com/openai/v1 does, in any case
				}
				p := NewOpenAICompatibleProviderWithOpts("k", c.model, "", base, 0, c.kind, ProviderOptions{})
				p.reasoning = c.reasoning
				if err := ep.call(p, 400); err != nil {
					t.Fatalf("%s: %v", ep.name, err)
				}
				var sent struct {
					Effort *string `json:"reasoning_effort"`
				}
				host.sent(t, &sent)
				got := ""
				if sent.Effort != nil {
					got = *sent.Effort
				}
				if got != c.want {
					t.Errorf("sent reasoning_effort %q, want %q", got, c.want)
				}
			})
		}
	}
}

// The effort lives on the family's row and nowhere else, so the next family that thinks out of its budget is
// covered by adding its row: with thinking off it is sent its effort on an OpenAI-family host and on Ollama,
// and ChatJSONWithRetry's one retry after an empty JSON refusal asks for it, with thinking on, on both.
func TestTheNextFamilyThatTakesAnEffortNeedsOnlyItsRow(t *testing.T) {
	saved := thinkingFamilies
	t.Cleanup(func() { thinkingFamilies = saved })
	thinkingFamilies = append(append([]thinkingFamily(nil), saved...), thinkingFamily{hint: "next-thinker", always: true, effort: "minimal"})

	// Thinking off, through every OpenAI-family entry point.
	for _, ep := range openAIEntryPoints {
		host := newModelHost(t, nil)
		p := NewOpenAICompatibleProviderWithOpts("k", "acme/next-thinker-30b", "", host.URL+"/v1", 0, ProviderOpenAICompatible, ProviderOptions{})
		if err := ep.call(p, 400); err != nil {
			t.Fatalf("%s: %v", ep.name, err)
		}
		var sent struct {
			Effort *string `json:"reasoning_effort"`
		}
		host.sent(t, &sent)
		if sent.Effort == nil || *sent.Effort != "minimal" {
			t.Errorf("%s: sent reasoning_effort %v, want minimal", ep.name, sent.Effort)
		}
	}

	// Thinking on: the first JSON call is sent no effort, is refused, and the one retry asks for the family's.
	host := newGroqHost(t)
	p := NewOpenAICompatibleProviderWithOpts("k", "acme/next-thinker-30b", "", host.URL+"/v1", 0, ProviderOpenAICompatible, ProviderOptions{})
	p.reasoning = true
	if _, err := ChatJSONWithRetry(context.Background(), p, nil, "sys", "plan my week", ChatOptions{MaxTokens: 460}, nil); err != nil {
		t.Fatalf("the retry at the family's effort answers: %v", err)
	}
	if got := host.sentEfforts(); strings.Join(got, ",") != ",minimal" {
		t.Errorf("efforts sent %q, want the endpoint's default then minimal", got)
	}

	// Ollama takes the level in its think field, with thinking off, and with it on when the call asks to think least.
	for _, c := range []struct {
		name      string
		reasoning bool
		opts      ChatOptions
		think     string
	}{
		{"thinking off", false, ChatOptions{MaxTokens: 400}, `"minimal"`},
		{"thinking on", true, ChatOptions{MaxTokens: 400}, "true"},
		{"thinking on, asked to think least", true, ChatOptions{MaxTokens: 400, LeastThinking: true}, `"minimal"`},
	} {
		host := newModelHost(t, map[string][]string{"next-thinker:30b": {"completion", "thinking"}})
		o := &OllamaProvider{host: host.URL, model: "next-thinker:30b", client: host.Client(), reasoning: c.reasoning}
		if _, err := o.Chat(context.Background(), roomAsk, c.opts); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var sent struct {
			Think json.RawMessage `json:"think"`
		}
		host.sent(t, &sent)
		if string(sent.Think) != c.think {
			t.Errorf("Ollama, %s: sent think %s, want %s", c.name, sent.Think, c.think)
		}
	}
}

// Ollama's own API follows the same rule as an OpenAI-family host: an empty JSON refusal is asked once more
// only when the call at the least thinking is another request, its think field or its num_predict differing
// from the call that was sent. Otherwise the retry sends the same bytes for the same refusal, a second wait and
// a second breaker failure (V-811c). The host here refuses every call, so the last one's refusal is returned,
// with every call in the breaker.
func TestAnOllamaEmptyJSONRefusalIsAskedOnceMoreOnlyWhenThatIsAnotherRequest(t *testing.T) {
	caps := map[string][]string{
		"gpt-oss:20b": {"completion", "tools", "thinking"},
		"qwen3:8b":    {"completion", "tools", "thinking"},
		"llama3.3":    {"completion", "tools"},
	}
	cases := []struct {
		name      string
		model     string
		reasoning bool
		sent      string // each call's think field and num_predict, as JSON ("omitted" for no field)
	}{
		{"gpt-oss with thinking on, then at low", "gpt-oss:20b", true, `true/1484 "low"/1484`},
		{"gpt-oss with thinking off already went at low", "gpt-oss:20b", false, `"low"/1484`},
		{"qwen3 with thinking on, then off", "qwen3:8b", true, "true/1484 false/460"},
		{"qwen3 with thinking off", "qwen3:8b", false, "false/460"},
		{"a model that doesn't think", "llama3.3", true, "omitted/460"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host := newModelHost(t, caps)
			host.mu.Lock()
			host.refusal = groqRefusal
			host.mu.Unlock()
			p := &OllamaProvider{host: host.URL, model: c.model, client: host.Client(), reasoning: c.reasoning}
			cb := NewCircuitBreaker(5, time.Minute)
			_, err := ChatJSONWithRetry(context.Background(), p, cb, "sys", "plan my week", ChatOptions{MaxTokens: 460}, validJSON)
			if err == nil || !strings.Contains(err.Error(), "json_validate_failed") {
				t.Fatalf("the host's refusal is returned: %v", err)
			}
			host.mu.Lock()
			chats := append([][]byte(nil), host.chats...)
			host.mu.Unlock()
			var sent []string
			for _, b := range chats {
				var in struct {
					Think   json.RawMessage `json:"think"`
					Options struct {
						NumPredict int `json:"num_predict"`
					} `json:"options"`
				}
				_ = json.Unmarshal(b, &in)
				think := "omitted"
				if in.Think != nil {
					think = string(in.Think)
				}
				sent = append(sent, fmt.Sprintf("%s/%d", think, in.Options.NumPredict))
			}
			if got := strings.Join(sent, " "); got != c.sent {
				t.Errorf("sent %q, want %q", got, c.sent)
			}
			if cb.failures != len(chats) {
				t.Errorf("the breaker holds %d failures, want one per call (%d)", cb.failures, len(chats))
			}
		})
	}
}
