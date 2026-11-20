package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
)

// On the public demo a visitor's typed or spoken text never leaves the demo
// box (zoom-05 C1, MAIN's condition 7): the translator, like every member's
// run, resolves through ResolveUserModel (business/AI translateModel), and on
// DEMO_MODE that is the workspace's own default model. An outside provider a
// member picked is never read, never built and never called: its stub fails
// the test if anything reaches it. Off the demo the same pick resolves to the
// outside provider, so the stub is live and the demo's answer isn't vacuous.
func TestOnTheDemoTheTranslatorUsesTheLocalDefaultNeverAnOutsideProvider(t *testing.T) {
	var reached atomic.Int32
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		http.Error(w, "an outside provider", http.StatusTeapot)
	}))
	defer outside.Close()

	savedPick, savedEP := userPick, pickEndpoint
	t.Cleanup(func() { userPick, pickEndpoint = savedPick, savedEP })
	var picks atomic.Int32
	userPick = func(context.Context, string) (*aiModels.AuthorizedModel, error) {
		picks.Add(1)
		return &aiModels.AuthorizedModel{ProviderID: uuid.New(), ProviderKind: string(ProviderOpenAICompatible),
			ProviderName: "outside", Model: "outside-model", Label: "Outside"}, nil
	}
	pickEndpoint = func(_ context.Context, _ *uuid.UUID, model string, _ int, _ *AIConfig) (Endpoint, error) {
		return Endpoint{Kind: ProviderOpenAICompatible, BaseURL: outside.URL + "/v1", APIKey: "k", Model: model}, nil
	}
	service := func() (*AIService, *fakeLLM) {
		local := &fakeLLM{outs: []string{`{"lines": ["bonjour"]}`}}
		return &AIService{
			LLM:        local,
			Resiliency: NewResiliencyManager(60),
			Config: &AIConfig{Enabled: true, Chat: Endpoint{Kind: ProviderOllama,
				BaseURL: "http://127.0.0.1:11434", Model: "qwen3:4b-instruct"}},
			models: newModelResolver(),
		}, local
	}
	ctx := context.Background()

	t.Run("off the demo the pick is the outside provider", func(t *testing.T) {
		t.Setenv("DEMO_MODE", "")
		svc, local := service()
		if llm, _ := svc.ResolveUserModel(ctx, "visitor-uuid"); llm == nil || sameClient(llm, local) {
			t.Fatalf("off the demo the member's outside pick should resolve; got the default (%v)", llm)
		}
	})

	t.Run("on the demo it is the local default", func(t *testing.T) {
		t.Setenv("DEMO_MODE", "true")
		svc, local := service()
		picks.Store(0)
		llm, cb := svc.ResolveUserModel(ctx, "visitor-uuid")
		// The translator's one model call goes through what was resolved.
		if _, err := ChatJSONWithRetry(ctx, llm, cb, "translate", `{"lines": ["hello"]}`, ChatOptions{},
			func(string) bool { return true }); err != nil || local.calls != 1 {
			t.Errorf("the local default answered %d times (%v), want once", local.calls, err)
		}
		if !sameClient(llm, local) || cb != svc.Resiliency.CB {
			t.Errorf("on the demo the translator resolved %T, not the local default", llm)
		}
		if got := svc.DescribeModel(ctx, "visitor-uuid", llm).Label; got != "qwen3 4B" {
			t.Errorf("on the demo the translation is labelled %q, want the local default qwen3 4B", got)
		}
		if n := picks.Load(); n != 0 {
			t.Errorf("on the demo the member's pick was read %d times, want never", n)
		}
	})
	if n := reached.Load(); n != 0 {
		t.Errorf("the outside provider was called %d times; a demo visitor's text must never leave the box", n)
	}
}

// CR-425 F1: a caller that names the model it runs on (the translator) reads
// the member's pick and its provider once, and is named as DescribeModel
// names it; DescribeModel alone reads them again (its callers hold only the
// client).
func TestResolveUserModelDescribedReadsThePickOnceAndNamesItLikeDescribeModel(t *testing.T) {
	t.Setenv("DEMO_MODE", "")
	savedPick, savedEP := userPick, pickEndpoint
	t.Cleanup(func() { userPick, pickEndpoint = savedPick, savedEP })
	var picks, providers atomic.Int32
	userPick = func(context.Context, string) (*aiModels.AuthorizedModel, error) {
		picks.Add(1)
		return &aiModels.AuthorizedModel{ProviderID: uuid.New(), ProviderKind: string(ProviderOllama),
			ProviderName: "lab", Model: "llama3.1:8b", Label: "Lab llama"}, nil
	}
	pickEndpoint = func(_ context.Context, _ *uuid.UUID, model string, _ int, _ *AIConfig) (Endpoint, error) {
		providers.Add(1)
		return Endpoint{Kind: ProviderOllama, BaseURL: "http://127.0.0.1:11435", Model: model}, nil
	}
	svc := &AIService{LLM: &fakeLLM{}, Resiliency: NewResiliencyManager(60), models: newModelResolver(),
		Config: &AIConfig{Enabled: true, Chat: Endpoint{Kind: ProviderOllama, BaseURL: "http://127.0.0.1:11434", Model: "qwen3:4b-instruct"}}}
	ctx := context.Background()
	llm, _, desc := svc.ResolveUserModelDescribed(ctx, "member-uuid")
	if picks.Load() != 1 || providers.Load() != 1 {
		t.Fatalf("one resolution read the pick %d and its provider %d times; want 1 and 1", picks.Load(), providers.Load())
	}
	if want := svc.DescribeModel(ctx, "member-uuid", llm); desc != want || desc.Label != "Lab llama" || desc.Name != "llama3.1:8b" {
		t.Fatalf("described %+v; DescribeModel says %+v (want Lab llama, llama3.1:8b)", desc, want)
	}
	if _, _, def := (&AIService{LLM: svc.LLM, Resiliency: svc.Resiliency, Config: svc.Config}).ResolveUserModelDescribed(ctx, "member-uuid"); def.Name != "qwen3:4b-instruct" {
		t.Fatalf("with no picks to resolve the description is the default's: %+v", def)
	}
}
