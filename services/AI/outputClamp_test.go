package ai

import "testing"

// A stated output ceiling must cap what we SEND, not only what we budget.
//
// WHY THIS EXISTS. Migration 140 lets an admin record a model's max output, and the first
// version used it in exactly one place: ResponseReserve, which sizes the INPUT budget.
// Nothing clamped the max_tokens on the wire. So an admin could record "this model caps at
// 4096", a caller could ask for 8192, and we sent 8192 — which both Anthropic and OpenAI
// answer with a 400 rather than by generating less. The column was named for a limit it did
// not enforce.
//
// Real callers ask for 4096 and 8192 (the board tools, the code-PR proxy), so this is not
// hypothetical for any model capped below that.
func TestStatedCeilingCapsWhatIsSent(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		capped := &AnthropicProvider{maxOutput: 4096}
		if got := capped.clampOutput(8192); got != 4096 {
			t.Errorf("a request above the ceiling must be capped: got %d, want 4096", got)
		}
		if got := capped.clampOutput(1024); got != 1024 {
			t.Errorf("a request below the ceiling must pass through: got %d, want 1024", got)
		}
		if got := capped.clampOutput(4096); got != 4096 {
			t.Errorf("a request exactly at the ceiling must pass: got %d", got)
		}
		// Anthropic's API REQUIRES max_tokens, so asking for nothing still has to produce
		// the documented default rather than 0.
		if got := capped.clampOutput(0); got != defaultAnthropicMaxTokens && got != 4096 {
			t.Errorf("no request must fall back to the default (or the lower ceiling): got %d", got)
		}
		// No stated ceiling leaves behaviour exactly as it was.
		unstated := &AnthropicProvider{}
		if got := unstated.clampOutput(8192); got != 8192 {
			t.Errorf("with no ceiling the caller decides: got %d, want 8192", got)
		}
		if got := unstated.clampOutput(0); got != defaultAnthropicMaxTokens {
			t.Errorf("with no ceiling and no request, the documented default applies: got %d", got)
		}
	})

	t.Run("openai family", func(t *testing.T) {
		capped := &OpenAIProvider{maxOutput: 4096}
		if got := capped.clampOutput(8192); got != 4096 {
			t.Errorf("a request above the ceiling must be capped: got %d, want 4096", got)
		}
		if got := capped.clampOutput(1024); got != 1024 {
			t.Errorf("a request below the ceiling must pass through: got %d", got)
		}
		// Unlike Anthropic, this API does not require the field. Asking for nothing must
		// stay nothing so the request omits it and the provider's own default applies —
		// inventing a ceiling here would cap answers that are currently unbounded.
		if got := capped.clampOutput(0); got != 0 {
			t.Errorf("no request must stay unset so the field is omitted: got %d", got)
		}
		unstated := &OpenAIProvider{}
		if got := unstated.clampOutput(8192); got != 8192 {
			t.Errorf("with no ceiling the caller decides: got %d, want 8192", got)
		}
	})
}

// The ceiling reaches the client through buildLLM, the same way the context window does.
// Without this the field would be recorded, budgeted with, and never applied to a request.
func TestBuildLLMCarriesTheCeilingToTheClient(t *testing.T) {
	cases := []struct {
		name string
		ep   Endpoint
		want int
	}{
		{"anthropic", Endpoint{Kind: ProviderAnthropic, Model: "m", APIKey: "k"}, 4096},
		{"openai", Endpoint{Kind: ProviderOpenAI, Model: "m", APIKey: "k"}, 4096},
		{"openai_compatible", Endpoint{Kind: ProviderOpenAICompatible, Model: "m", BaseURL: "http://localhost:1234/v1"}, 4096},
	}
	for _, c := range cases {
		llm, err := buildLLM(c.ep, 8192, c.want, false, nil)
		if err != nil {
			t.Fatalf("%s: buildLLM: %v", c.name, err)
		}
		switch p := llm.(type) {
		case *AnthropicProvider:
			if p.maxOutput != c.want {
				t.Errorf("%s: ceiling did not reach the client: %d", c.name, p.maxOutput)
			}
		case *OpenAIProvider:
			if p.maxOutput != c.want {
				t.Errorf("%s: ceiling did not reach the client: %d", c.name, p.maxOutput)
			}
		default:
			t.Errorf("%s: unexpected provider type %T", c.name, llm)
		}
	}

	// Ollama is deliberately excluded: it does not reject an over-large num_predict, it
	// just generates fewer tokens, so clamping would only hide the operator's value from
	// the request without preventing any failure. Its context window still applies.
	llm, err := buildLLM(Endpoint{Kind: ProviderOllama, BaseURL: "http://localhost:11434", Model: "m"}, 8192, 4096, false, nil)
	if err != nil {
		t.Fatalf("ollama: buildLLM: %v", err)
	}
	op, ok := llm.(*OllamaProvider)
	if !ok {
		t.Fatalf("expected an Ollama provider, got %T", llm)
	}
	if op.numCtx != 8192 {
		t.Errorf("the context window must still reach an Ollama client: num_ctx=%d", op.numCtx)
	}
}

// End to end through the resolver: an admin's stated ceiling on the allowlist row must
// arrive at the built client, not stop at the budget.
func TestResolvedClientCarriesTheStatedCeiling(t *testing.T) {
	rm, err := newModelResolver().get("k|m",
		Endpoint{Kind: ProviderAnthropic, Model: "m", APIKey: "k"},
		&AIConfig{ContextWindowTokens: 200000},
		func() ModelLimits { return LimitsForModel("anthropic", "m", 200000, 8192) })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	p, ok := rm.llm.(*AnthropicProvider)
	if !ok {
		t.Fatalf("expected an Anthropic provider, got %T", rm.llm)
	}
	if p.maxOutput != 8192 {
		t.Errorf("the row's ceiling must reach the client: got %d, want 8192", p.maxOutput)
	}
	if rm.limits.MaxOutput != 8192 {
		t.Errorf("cached limits must agree with the client: got %d", rm.limits.MaxOutput)
	}
	// And a caller asking for more than the row allows is capped rather than rejected.
	if got := p.clampOutput(64000); got != 8192 {
		t.Errorf("an over-large request must be capped to the row's ceiling: got %d", got)
	}
}
