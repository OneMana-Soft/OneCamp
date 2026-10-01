package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// One probe has to read every source's payload, because the field NAME is the only thing
// that differs between them. These are the real shapes, so adding a gateway means adding a
// case here and a name to the list rather than writing another client.
func TestLimitsFromObjectReadsEverySourcesFieldName(t *testing.T) {
	cases := []struct {
		source     string
		payload    string
		wantWindow int
		wantOutput int
	}{
		{
			source:     "OpenRouter (top-level context_length)",
			payload:    `{"id":"m","context_length":1000000,"max_completion_tokens":32768}`,
			wantWindow: 1000000, wantOutput: 32768,
		},
		{
			source:     "OpenRouter (nested under top_provider)",
			payload:    `{"id":"m","top_provider":{"context_length":128000,"max_completion_tokens":16384}}`,
			wantWindow: 128000, wantOutput: 16384,
		},
		{
			source:     "Groq (context_window)",
			payload:    `{"id":"m","context_window":131072}`,
			wantWindow: 131072,
		},
		{
			source:     "vLLM (max_model_len)",
			payload:    `{"id":"m","max_model_len":32768}`,
			wantWindow: 32768,
		},
		{
			source:     "LM Studio (max_context_length)",
			payload:    `{"id":"m","max_context_length":8192}`,
			wantWindow: 8192,
		},
		{
			source:     "llama.cpp (nested under meta)",
			payload:    `{"id":"m","meta":{"n_ctx_train":4096}}`,
			wantWindow: 4096,
		},
		{
			source:     "Anthropic (max_input_tokens / max_tokens)",
			payload:    `{"id":"m","display_name":"Model","max_input_tokens":200000,"max_tokens":64000}`,
			wantWindow: 200000, wantOutput: 64000,
		},
		{
			source:     "a provider that stringifies its numbers",
			payload:    `{"id":"m","context_length":"65536"}`,
			wantWindow: 65536,
		},
		{
			source:  "OpenAI's actual shape — no limits at all",
			payload: `{"id":"gpt-4o","object":"model","created":1715367049,"owned_by":"system"}`,
		},
		{
			source:  "zero and negative values are 'not reported', not a window",
			payload: `{"id":"m","context_length":0,"max_model_len":-1}`,
		},
	}

	for _, c := range cases {
		var obj map[string]any
		if err := json.Unmarshal([]byte(c.payload), &obj); err != nil {
			t.Fatalf("%s: bad fixture: %v", c.source, err)
		}
		got := limitsFromObject(obj)
		if got.ContextWindow != c.wantWindow {
			t.Errorf("%s: window = %d, want %d", c.source, got.ContextWindow, c.wantWindow)
		}
		if got.MaxOutput != c.wantOutput {
			t.Errorf("%s: output = %d, want %d", c.source, got.MaxOutput, c.wantOutput)
		}
		// When something was found, the field it came from must be reported — an admin
		// being asked to accept a number is entitled to know where it came from.
		if got.ContextWindow > 0 && got.Source == "" {
			t.Errorf("%s: found a window but reported no source field", c.source)
		}
	}
}

// A found value must never be below the floor the schema and budgeter agree on, or the
// suggestion would be one an admin cannot actually save.
func TestDiscoveredWindowsAreSavableValues(t *testing.T) {
	var obj map[string]any
	_ = json.Unmarshal([]byte(`{"id":"m","context_length":8192}`), &obj)
	if got := limitsFromObject(obj); got.ContextWindow < minContextWindow {
		t.Errorf("a discovered window of %d is below the %d floor and could not be saved",
			got.ContextWindow, minContextWindow)
	}
}

// Ollama names the key after the model architecture, which is not known ahead of time, so
// it is matched by suffix rather than constructed.
func TestOllamaContextKeyIsMatchedBySuffix(t *testing.T) {
	for _, arch := range []string{"llama", "qwen3", "gemma3", "deepseek2", "some.future.arch"} {
		payload := `{"model_info":{"general.architecture":"` + arch + `","` + arch + `.context_length":131072}}`
		var p struct {
			ModelInfo map[string]any `json:"model_info"`
		}
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			t.Fatalf("%s: bad fixture: %v", arch, err)
		}
		found := 0
		for k, v := range p.ModelInfo {
			if strings.HasSuffix(k, ".context_length") {
				found = asPositiveInt(v)
			}
		}
		if found != 131072 {
			t.Errorf("architecture %q: context length not found by suffix, got %d", arch, found)
		}
	}
}

// A modelfile's num_ctx is what the model is RUN at, so when it is smaller than the
// architecture's capability it is the operative number and must win.
func TestOllamaNumCtxOverrideWinsWhenSmaller(t *testing.T) {
	if got := ollamaParamNumCtx("num_ctx                        8192\ntemperature 0.7\n"); got != 8192 {
		t.Errorf("num_ctx = %d, want 8192", got)
	}
	// Absent, malformed and unrelated parameters all mean "no override".
	for _, params := range []string{"", "temperature 0.7", "num_ctx", "num_ctx notanumber", "num_ctx 0"} {
		if got := ollamaParamNumCtx(params); got != 0 {
			t.Errorf("params %q should yield no override, got %d", params, got)
		}
	}
}

// OpenAI is not probed at all: its Models endpoint publishes no limits, so a request would
// spend a round trip to learn nothing. The note has to send the admin somewhere useful
// instead of looking like a failure.
func TestOpenAIReportsThatItPublishesNothing(t *testing.T) {
	got := DiscoverModelLimits(nil, Endpoint{Kind: ProviderOpenAI, Model: "gpt-4o"}, nil)
	if got.Found() {
		t.Error("OpenAI must not return a fabricated window")
	}
	if got.Note == "" {
		t.Fatal("a zero result must explain itself")
	}
	for _, want := range []string{"does not publish", "docs"} {
		if !strings.Contains(strings.ToLower(got.Note), want) {
			t.Errorf("note should mention %q so the admin knows where to look, got %q", want, got.Note)
		}
	}
}

// A missing model name is answered without a network call, and every provider kind returns
// a note rather than an empty struct — an unexplained zero is indistinguishable from a bug.
func TestDiscoveryAlwaysExplainsAZeroResult(t *testing.T) {
	if got := DiscoverModelLimits(nil, Endpoint{Kind: ProviderOllama, Model: "  "}, nil); got.Note == "" {
		t.Error("a blank model name must produce a note")
	}
	// An Ollama provider with no host cannot be probed, and must say that rather than
	// attempting a request against an empty URL.
	if got := discoverOllama(nil, Endpoint{Kind: ProviderOllama}, &AIConfig{}, "m"); got.Note == "" {
		t.Error("a missing Ollama host must produce a note")
	}
	// An openai_compatible provider with no base URL is the same case.
	if got := discoverOpenAICompatible(nil, Endpoint{Kind: ProviderOpenAICompatible}, "m"); got.Note == "" {
		t.Error("a missing base URL must produce a note")
	}
	if got := DiscoverModelLimits(nil, Endpoint{Kind: ProviderType("nonsense"), Model: "m"}, nil); got.Note == "" {
		t.Error("an unknown provider kind must produce a note")
	}
}

// The field lists are the extension point, so a name appearing in both would make which
// one won depend on list order.
func TestLimitFieldNamesDoNotOverlap(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range contextWindowFields {
		if seen[f] {
			t.Errorf("%q is listed twice among the context-window fields", f)
		}
		seen[f] = true
	}
	for _, f := range maxOutputFields {
		if seen[f] {
			t.Errorf("%q is both a context-window and a max-output field name", f)
		}
	}
}
