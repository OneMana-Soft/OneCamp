package ai

// Which model a member's doc AI runs on, said in words a person reads.
//
// The doc's AI bar names the model before the first word arrives: "Runs on your
// server · qwen3 4B" when the model is on the workspace's own network, or the
// provider and model when the text leaves it ("Sent to OpenAI · gpt-4.1-mini").
// A remote model is never left unnamed: the person deciding whether to send a
// paragraph of a private doc is told where it goes before it goes.
//
// The answer describes the client ResolveUserModelWithLimits handed out, never a
// second guess at the same choice: the workspace default's client is the
// default's endpoint, and any other client is the member's own pick (local-only
// mode, a pick that could not be built and a revoked pick all hand out the
// default), so the label and the model that writes cannot disagree.

import (
	"context"
	"regexp"
	"strings"
	"sync"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
)

// DocModel is the model a doc AI request runs on, as the bar names it.
type DocModel struct {
	// Name is the model's id as configured ("qwen3:4b-instruct").
	Name string `json:"name"`
	// Label is the name as a person reads it ("qwen3 4B").
	Label string `json:"label"`
	// Provider is the provider kind ("ollama", "openai", …).
	Provider string `json:"provider"`
	// ProviderLabel names where a remote model is ("OpenAI"); empty for a
	// local one.
	ProviderLabel string `json:"provider_label,omitempty"`
	// Local is whether the model is on the workspace's own network.
	Local bool `json:"local"`
}

// cloudByHost caches the local or cloud answer per provider kind and host:
// telling them apart can resolve a host name, which a label must not pay per
// call.
var cloudByHost sync.Map

// isCloudEndpoint is whether ep sends text outside the workspace's network.
// A seam, so a test decides without DNS.
var isCloudEndpoint = func(cfg *AIConfig, ep Endpoint) bool {
	key := string(ep.Kind) + "|" + cfg.endpointHost(ep)
	if v, ok := cloudByHost.Load(key); ok {
		return v.(bool)
	}
	cloud := cfg.endpointIsCloud(ep)
	cloudByHost.Store(key, cloud)
	return cloud
}

// userPick is a member's own model pick: a seam over the store.
var userPick = aiModels.GetUserModelPreference

// DescribeModel is the model llm is, where llm is the client
// ResolveUserModelWithLimits gave userUUID.
func (s *AIService) DescribeModel(ctx context.Context, userUUID string, llm LLMProvider) DocModel {
	if s == nil || s.Config == nil {
		return DocModel{}
	}
	def := describeEndpoint(s.Config, s.Config.Chat)
	if llm == nil || sameClient(llm, s.LLM) || strings.TrimSpace(userUUID) == "" {
		return def
	}
	pref, err := userPick(ctx, userUUID)
	if err != nil || pref == nil {
		return def
	}
	providerID := pref.ProviderID
	ep, err := endpointFromProvider(ctx, &providerID, pref.Model, 0, s.Config)
	if err != nil {
		return def
	}
	info := describeEndpoint(s.Config, ep)
	if l := strings.TrimSpace(pref.Label); l != "" && l != pref.Model {
		info.Label = l
	}
	return info
}

// sameClient is whether two clients are one; false for a client whose type
// can't be compared, which is never the workspace default.
func sameClient(a, b LLMProvider) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

func describeEndpoint(cfg *AIConfig, ep Endpoint) DocModel {
	local := !isCloudEndpoint(cfg, ep)
	info := DocModel{Name: ep.Model, Label: ModelDisplayName(ep.Model), Provider: string(ep.Kind), Local: local}
	if !local {
		info.ProviderLabel = providerLabel(ep.Kind)
	}
	return info
}

var modelSize = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)([bm])\b`)

// ModelDisplayName is a model id as a person reads it: "qwen3:4b-instruct" is
// "qwen3 4B" and "llama3.2:3b" is "llama3.2 3B"; a name with no size tag is kept
// as it is. Pure.
func ModelDisplayName(model string) string {
	model = strings.TrimSpace(model)
	if i := strings.LastIndex(model, "/"); i >= 0 && i < len(model)-1 && !strings.Contains(model[i:], ":") {
		model = model[i+1:]
	}
	base, tag, ok := strings.Cut(model, ":")
	if !ok {
		return model
	}
	if m := modelSize.FindStringSubmatch(tag); m != nil {
		return base + " " + m[1] + strings.ToUpper(m[2])
	}
	if tag == "latest" {
		return base
	}
	return base + " " + tag
}

func providerLabel(kind ProviderType) string {
	switch kind {
	case ProviderOpenAI:
		return "OpenAI"
	case ProviderAnthropic:
		return "Anthropic"
	case ProviderOllama:
		return "an Ollama server"
	default:
		return "a hosted model"
	}
}
