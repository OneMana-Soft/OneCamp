package ai

import "testing"

func TestModelSupportsThinking(t *testing.T) {
	thinking := []string{
		"deepseek-r1", "deepseek-r1:7b", "qwen3", "qwen3:8b-instruct",
		"qwq", "qwq:32b", "magistral", "phi4-reasoning", "gpt-oss:20b",
		"  DeepSeek-R1  ",
	}
	for _, m := range thinking {
		if !modelSupportsThinking(m) {
			t.Errorf("expected %q to be detected as thinking-capable", m)
		}
	}
	notThinking := []string{
		"", "llama3.2:3b", "llama3.3", "llama-3.3-70b-versatile",
		"nomic-embed-text", "mistral", "gemma2", "phi3",
	}
	for _, m := range notThinking {
		if modelSupportsThinking(m) {
			t.Errorf("expected %q NOT to be thinking-capable", m)
		}
	}
}

func TestResolveThink_OmitsForNonThinkingModel(t *testing.T) {
	// A non-thinking model must never receive the flag, even with reasoning on
	// or an explicit request — that's what 400s on newer Ollama.
	o := &OllamaProvider{model: "llama3.2:3b", reasoning: true}
	if got := o.resolveThink(nil); got != nil {
		t.Fatalf("non-thinking model must omit think (nil), got %v", *got)
	}
	yes := true
	if got := o.resolveThink(&yes); got != nil {
		t.Fatalf("non-thinking model must omit think even when explicitly requested, got %v", *got)
	}
}

func TestResolveThink_HonorsForThinkingModel(t *testing.T) {
	o := &OllamaProvider{model: "qwen3:8b", reasoning: true}
	if got := o.resolveThink(nil); got == nil || *got != true {
		t.Fatalf("thinking model with reasoning on should send think=true, got %v", got)
	}
	no := false
	if got := o.resolveThink(&no); got == nil || *got != false {
		t.Fatalf("explicit want=false must win for a thinking model, got %v", got)
	}
}
