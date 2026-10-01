package ai

// Asking a provider what a model's limits actually are.
//
// Per-model windows are admin-set (migration 140), which leaves the operator holding a
// question they should not have to answer from memory: how big is this model's context
// window? Most sources will tell us, and the ones that will not should say so plainly
// rather than leave an empty field looking like an oversight.
//
// WHAT EACH SOURCE ACTUALLY EXPOSES. This differs per provider, and the difference is
// almost entirely in the FIELD NAME rather than the shape:
//
//   - Ollama: POST /api/show returns model_info with "<arch>.context_length"
//     (llama.context_length, qwen3.context_length, ...). The arch is not known ahead of
//     time, so the key is matched by suffix. A modelfile may also pin num_ctx in
//     parameters, and that is the RUN size rather than the capability.
//   - Anthropic: GET /v1/models/{id} carries max_input_tokens and max_tokens as typed
//     fields — the window and the output ceiling, exactly what is needed.
//   - OpenAI: nothing. The Models endpoint returns id/object/created/owned_by and no
//     limits, and their own docs page is the stated source of truth. An admin has to type
//     it, and the honest thing is to say so instead of returning a guess.
//   - openai_compatible: depends entirely on what is behind the URL, and this is where the
//     field names diverge — context_length (OpenRouter), context_window (Groq),
//     max_model_len (vLLM), max_context_length (LM Studio), meta.n_ctx_train
//     (llama.cpp), and OpenRouter additionally nests them under top_provider.
//
// So this is ONE probe that reads whichever known field is present, not a client per
// vendor. A vendor-specific client per source would need writing again for the next
// gateway an admin points at; a field scan already covers it if the name is one of these,
// and degrades to "not published" if not.
//
// SUGGESTS, NEVER APPLIES. Discovery fills the admin's form and they save it. Writing a
// probed value straight to the row would make an upgrade silently re-size prompts — the
// thing migration 140 was careful not to do — and would trust a number from a gateway
// that may be describing a different model than the one it routes to.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DiscoveredLimits is what a provider says about one model.
type DiscoveredLimits struct {
	// ContextWindow / MaxOutput are 0 when the provider did not report them.
	ContextWindow int `json:"context_window_tokens"`
	MaxOutput     int `json:"max_output_tokens"`
	// Field names the value came from, so an admin can see WHY a number is being
	// suggested rather than being asked to trust it.
	Source string `json:"source,omitempty"`
	// Note explains a zero result: not published by this provider, unreachable, or a
	// model the provider does not know.
	Note string `json:"note,omitempty"`
}

// Found reports whether anything usable came back.
func (d DiscoveredLimits) Found() bool { return d.ContextWindow > 0 || d.MaxOutput > 0 }

// discoveryTimeout bounds a probe. This runs while an admin waits on a form, so a slow or
// unreachable endpoint must fail fast and say so rather than hang the dialog.
const discoveryTimeout = 12 * time.Second

// contextWindowFields are the field names sources use for a model's context window, in
// no particular order — the first one present wins, and a payload carrying two of them
// carries the same number twice in practice.
var contextWindowFields = []string{
	"context_length",     // OpenRouter, Together
	"context_window",     // Groq
	"max_model_len",      // vLLM
	"max_context_length", // LM Studio
	"max_input_tokens",   // Anthropic
	"n_ctx_train",        // llama.cpp (also nested under meta)
	"n_ctx",              // llama.cpp /props
}

// maxOutputFields are the equivalents for the output ceiling.
var maxOutputFields = []string{
	"max_completion_tokens", // OpenRouter top_provider
	"max_output_tokens",
	"max_tokens", // Anthropic
}

// EndpointForAuthorizedModel resolves the endpoint (base URL, credential, kind) for an
// allowlisted (provider, model) pair, plus the live config discovery needs for provider
// defaults. Exported so the business layer can probe a model without duplicating the
// provider-row lookup that endpointFromProvider already does.
func EndpointForAuthorizedModel(ctx context.Context, providerID uuid.UUID, model string) (Endpoint, *AIConfig, error) {
	cfg := GetConfig()
	ep, err := endpointFromProvider(ctx, &providerID, model, 0, cfg)
	if err != nil {
		return Endpoint{}, cfg, err
	}
	return ep, cfg, nil
}

// DiscoverModelLimits asks ep's provider what model's limits are.
//
// Never returns an error the caller has to handle as a failure: an unreachable endpoint,
// an unknown model or a provider that publishes nothing all come back as a zero result
// with a Note explaining which. Discovery is a convenience, and an admin who cannot get a
// suggestion must still be able to type the number.
func DiscoverModelLimits(ctx context.Context, ep Endpoint, cfg *AIConfig) DiscoveredLimits {
	// Nil-safe like the rest of this package's entry points: a probe is a convenience, and
	// panicking on a missing context would turn "we could not suggest a number" into a
	// crashed request handler.
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	model := strings.TrimSpace(ep.Model)
	if model == "" {
		return DiscoveredLimits{Note: "No model name to look up."}
	}

	switch ep.Kind {
	case ProviderOllama:
		return discoverOllama(ctx, ep, cfg, model)
	case ProviderAnthropic:
		return discoverAnthropic(ctx, ep, model)
	case ProviderOpenAI:
		// Deliberately not probed. The Models endpoint carries no limits, so a request
		// here would spend a round trip to learn nothing and return the same zero.
		return DiscoveredLimits{
			Note: "OpenAI's API does not publish context windows. Check the model's page in OpenAI's docs and enter it here.",
		}
	case ProviderOpenAICompatible:
		return discoverOpenAICompatible(ctx, ep, model)
	}
	return DiscoveredLimits{Note: "This provider kind does not support limit discovery."}
}

// discoverOllama reads model_info from /api/show. The architecture prefix is unknown
// ahead of time (llama, qwen3, gemma, ...), so the context-length key is matched by
// suffix rather than constructed.
func discoverOllama(ctx context.Context, ep Endpoint, cfg *AIConfig, model string) DiscoveredLimits {
	host := strings.TrimRight(ep.BaseURL, "/")
	if host == "" && cfg != nil {
		host = strings.TrimRight(cfg.OllamaHost, "/")
	}
	if host == "" {
		return DiscoveredLimits{Note: "This Ollama provider has no host configured."}
	}

	body, err := probeJSON(ctx, http.MethodPost, host+"/api/show",
		map[string]string{"model": model}, nil)
	if err != nil {
		return DiscoveredLimits{Note: "Could not reach Ollama: " + trimForLog(err.Error())}
	}

	var payload struct {
		ModelInfo  map[string]any `json:"model_info"`
		Parameters string         `json:"parameters"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return DiscoveredLimits{Note: "Ollama returned an unexpected response."}
	}

	out := DiscoveredLimits{}
	for k, v := range payload.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			if n := asPositiveInt(v); n > 0 {
				out.ContextWindow, out.Source = n, k
				break
			}
		}
	}
	// A modelfile may pin num_ctx, which is what the model is RUN at rather than what it
	// can do. When it is smaller it is the operative number, so prefer it and say why.
	if n := ollamaParamNumCtx(payload.Parameters); n > 0 && (out.ContextWindow == 0 || n < out.ContextWindow) {
		out.ContextWindow, out.Source = n, "parameters.num_ctx"
	}
	if !out.Found() {
		out.Note = "Ollama did not report a context length for this model."
	}
	return out
}

// ollamaParamNumCtx pulls num_ctx out of Ollama's newline-delimited "parameters" text.
func ollamaParamNumCtx(params string) int {
	for _, line := range strings.Split(params, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "num_ctx" {
			return asPositiveInt(fields[1])
		}
	}
	return 0
}

// discoverAnthropic reads the typed max_input_tokens / max_tokens fields off the model.
func discoverAnthropic(ctx context.Context, ep Endpoint, model string) DiscoveredLimits {
	base := strings.TrimRight(ep.BaseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com/v1"
	}
	body, err := probeJSON(ctx, http.MethodGet, base+"/models/"+model, nil, map[string]string{
		"x-api-key":         ep.APIKey,
		"anthropic-version": "2023-06-01",
	})
	if err != nil {
		return DiscoveredLimits{Note: "Could not reach Anthropic: " + trimForLog(err.Error())}
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return DiscoveredLimits{Note: "Anthropic returned an unexpected response."}
	}
	out := limitsFromObject(obj)
	if !out.Found() {
		out.Note = "Anthropic did not report limits for this model."
	}
	return out
}

// discoverOpenAICompatible lists models and scans the matching entry for any of the known
// field names. Tries the single-model route first because some gateways serve thousands.
func discoverOpenAICompatible(ctx context.Context, ep Endpoint, model string) DiscoveredLimits {
	base := strings.TrimRight(ep.BaseURL, "/")
	if base == "" {
		return DiscoveredLimits{Note: "This provider has no base URL configured."}
	}
	headers := map[string]string{}
	if ep.APIKey != "" {
		headers["Authorization"] = "Bearer " + ep.APIKey
	}

	if body, err := probeJSON(ctx, http.MethodGet, base+"/models/"+model, nil, headers); err == nil {
		var obj map[string]any
		if json.Unmarshal(body, &obj) == nil {
			if out := limitsFromObject(obj); out.Found() {
				return out
			}
		}
	}

	body, err := probeJSON(ctx, http.MethodGet, base+"/models", nil, headers)
	if err != nil {
		return DiscoveredLimits{Note: "Could not reach the provider: " + trimForLog(err.Error())}
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if json.Unmarshal(body, &list) != nil {
		return DiscoveredLimits{Note: "The provider returned an unexpected response."}
	}
	for _, entry := range list.Data {
		if id, _ := entry["id"].(string); id != model {
			continue
		}
		if out := limitsFromObject(entry); out.Found() {
			return out
		}
		return DiscoveredLimits{Note: "This provider lists the model but does not publish its limits. Enter them from the model's documentation."}
	}
	return DiscoveredLimits{Note: "The provider did not list this model."}
}

// limitsFromObject scans one model object for any known limit field, including the two
// places gateways nest them: "meta" (llama.cpp) and "top_provider" (OpenRouter).
func limitsFromObject(obj map[string]any) DiscoveredLimits {
	out := DiscoveredLimits{}
	scan := func(m map[string]any, prefix string) {
		if m == nil {
			return
		}
		if out.ContextWindow == 0 {
			for _, f := range contextWindowFields {
				if n := asPositiveInt(m[f]); n > 0 {
					out.ContextWindow, out.Source = n, prefix+f
					break
				}
			}
		}
		if out.MaxOutput == 0 {
			for _, f := range maxOutputFields {
				if n := asPositiveInt(m[f]); n > 0 {
					out.MaxOutput = n
					break
				}
			}
		}
	}
	scan(obj, "")
	for _, nested := range []string{"meta", "top_provider"} {
		if sub, ok := obj[nested].(map[string]any); ok {
			scan(sub, nested+".")
		}
	}
	return out
}

// asPositiveInt coerces the several shapes a JSON number arrives in — float64 from
// encoding/json, a quoted string from providers that stringify numbers, or json.Number.
// Anything else, or anything non-positive, is 0 (meaning "not reported").
func asPositiveInt(v any) int {
	switch t := v.(type) {
	case float64:
		if t > 0 {
			return int(t)
		}
	case int:
		if t > 0 {
			return t
		}
	case json.Number:
		if n, err := t.Int64(); err == nil && n > 0 {
			return int(n)
		}
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// probeJSON performs one short request through the guarded HTTP client, so discovery is
// subject to the same egress rules as every other provider call — an admin cannot use it
// to reach somewhere a completion could not.
func probeJSON(ctx context.Context, method, url string, jsonBody any, headers map[string]string) ([]byte, error) {
	var reader io.Reader
	if jsonBody != nil {
		encoded, err := json.Marshal(jsonBody)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if jsonBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	client := newProviderHTTPClient(httpClientConfig{timeout: discoveryTimeout, guardSSRF: true})
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Bounded read: a model list from a large gateway is big, and an unbounded read on an
	// admin-supplied URL is a memory hazard.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return body, nil
}
