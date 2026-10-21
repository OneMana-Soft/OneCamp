package ai

import (
	"context"
	"testing"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
)

func TestModelDisplayNameIsWhatAPersonReads(t *testing.T) {
	for in, want := range map[string]string{
		"qwen3:4b-instruct":       "qwen3 4B",
		"llama3.2:3b":             "llama3.2 3B",
		"gemma3:270m":             "gemma3 270M",
		"mistral:latest":          "mistral",
		"gpt-4.1-mini":            "gpt-4.1-mini",
		"library/qwen3:8b":        "library/qwen3 8B",
		"meta-llama/Llama-3.1-8B": "Llama-3.1-8B",
		"phi4:mini":               "phi4 mini",
		"  qwen3:4b  ":            "qwen3 4B",
	} {
		if got := ModelDisplayName(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// The bar names the model that writes: the workspace default for the default's
// client, said as on the server or not, and a remote one with where it is.
func TestDescribeModelNamesTheClientThatWrites(t *testing.T) {
	saved, savedPick := isCloudEndpoint, userPick
	t.Cleanup(func() { isCloudEndpoint, userPick = saved, savedPick })
	userPick = func(context.Context, string) (*aiModels.AuthorizedModel, error) {
		t.Fatal("the default's client needs no lookup of the member's pick")
		return nil, nil
	}
	local := &OllamaProvider{}
	svc := &AIService{LLM: local, Config: &AIConfig{Chat: Endpoint{Kind: ProviderOllama, Model: "qwen3:4b-instruct"}}}

	isCloudEndpoint = func(*AIConfig, Endpoint) bool { return false }
	got := svc.DescribeModel(context.Background(), "u-1", local)
	if got != (DocModel{Name: "qwen3:4b-instruct", Label: "qwen3 4B", Provider: "ollama", Local: true}) {
		t.Errorf("local default: %+v", got)
	}

	svc.Config.Chat = Endpoint{Kind: ProviderOpenAI, Model: "gpt-4.1-mini"}
	isCloudEndpoint = func(*AIConfig, Endpoint) bool { return true }
	got = svc.DescribeModel(context.Background(), "u-1", local)
	if got.Local || got.ProviderLabel != "OpenAI" || got.Label != "gpt-4.1-mini" {
		t.Errorf("remote default: %+v", got)
	}
	if (*AIService)(nil).DescribeModel(context.Background(), "u", nil) != (DocModel{}) {
		t.Error("no service names no model")
	}
}

func TestSameClientNeverPanics(t *testing.T) {
	a := &OllamaProvider{}
	if !sameClient(a, a) || sameClient(a, &OllamaProvider{}) || sameClient(nil, a) {
		t.Error("pointer clients compare by identity")
	}
	type valueClient struct {
		LLMProvider
		//lint:ignore U1000 a slice field is what makes this client impossible to compare
		parts []string
	}
	if sameClient(valueClient{}, valueClient{}) {
		t.Error("a client that can't be compared is not the default")
	}
}
