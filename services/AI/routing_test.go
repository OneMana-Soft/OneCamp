package ai

import (
	"context"
	"testing"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	"github.com/google/uuid"
)

func TestEveryPurposeIsKnownAndDescribed(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Purposes {
		if !IsPurpose(p.Key) || p.Label == "" || p.Description == "" || seen[p.Key] {
			t.Errorf("purpose %+v is unnamed, undescribed or listed twice", p)
		}
		seen[p.Key] = true
	}
	for _, k := range []string{PurposeSummaries, PurposeMeetings, PurposeMemory} {
		if !seen[k] {
			t.Errorf("%s is routable in code but missing from the admin list", k)
		}
	}
	if IsPurpose("chat") || IsPurpose("") {
		t.Error("a person's chat is never routed: it uses the model they picked")
	}
}

func TestRouteForIgnoresEmptyAndMissing(t *testing.T) {
	var nilCfg *AIConfig
	if _, ok := nilCfg.RouteFor(PurposeSummaries); ok {
		t.Error("a nil config routed something")
	}
	cfg := &AIConfig{Routing: map[string]aiModels.RouteTarget{
		PurposeMeetings: {ProviderID: uuid.New(), Model: "big-context"},
		PurposeMemory:   {ProviderID: uuid.New(), Model: ""},
	}}
	if tgt, ok := cfg.RouteFor(PurposeMeetings); !ok || tgt.Model != "big-context" {
		t.Errorf("meetings route lost: %+v %v", tgt, ok)
	}
	for _, p := range []string{PurposeMemory, PurposeSummaries} {
		if _, ok := cfg.RouteFor(p); ok {
			t.Errorf("%s has no model and must use the default", p)
		}
	}
}

// With nothing routed, summaries run on the workspace default, exactly as
// before routing existed.
func TestSummariesUseTheDefaultWhenNothingIsRouted(t *testing.T) {
	def := &fakeLLM{outs: []string{"summary"}}
	svc := &AIService{LLM: def, Config: &AIConfig{Enabled: true}}
	out, err := svc.Summarize(context.Background(), "content", "prompt")
	if err != nil || out != "summary" || def.calls != 1 {
		t.Fatalf("out=%q err=%v calls=%d", out, err, def.calls)
	}
}

// A route the service cannot honour (here: no resolver to build it) degrades
// to the default instead of failing the summary.
func TestAnUnusableRouteFallsBackToTheDefault(t *testing.T) {
	def := &fakeLLM{outs: []string{"from default"}}
	svc := &AIService{LLM: def, Config: &AIConfig{Enabled: true, Routing: map[string]aiModels.RouteTarget{
		PurposeMeetings: {ProviderID: uuid.New(), Model: "missing"},
	}}}
	out, err := svc.SummarizeFor(context.Background(), PurposeMeetings, "transcript", "recap it")
	if err != nil || out != "from default" || def.calls != 1 {
		t.Fatalf("out=%q err=%v calls=%d", out, err, def.calls)
	}
}
