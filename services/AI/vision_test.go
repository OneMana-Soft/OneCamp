package ai

import (
	"strings"
	"testing"
)

// All three providers must satisfy the VisionProvider capability so an admin
// can point the vision model at any of them.
func TestProvidersImplementVisionProvider(t *testing.T) {
	var _ VisionProvider = (*OpenAIProvider)(nil)
	var _ VisionProvider = (*AnthropicProvider)(nil)
	var _ VisionProvider = (*OllamaProvider)(nil)
}

func TestDataURL(t *testing.T) {
	got := dataURL("image/png", []byte("hi"))
	if !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("unexpected data url: %q", got)
	}
	// Empty MIME falls back to image/png.
	if !strings.HasPrefix(dataURL("", []byte("x")), "data:image/png;base64,") {
		t.Fatalf("empty mime should default to image/png")
	}
}

func TestHasVision(t *testing.T) {
	cfg := &AIConfig{}
	if cfg.HasVision() {
		t.Fatal("empty config should not report vision")
	}
	cfg.Vision = Endpoint{Kind: ProviderOpenAI, Model: "gpt-4o"}
	if !cfg.HasVision() {
		t.Fatal("configured vision endpoint should report vision")
	}
	// Model without kind, or kind without model, is not usable.
	if (&AIConfig{Vision: Endpoint{Model: "x"}}).HasVision() {
		t.Fatal("vision needs a provider kind")
	}
}
