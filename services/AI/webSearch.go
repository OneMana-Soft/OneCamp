package ai

// Provider-agnostic web search for the AI assistant and agents (Wave 2,
// Requirement 3). OneCamp never hard-depends on one search vendor: an admin
// points it at whichever provider they run/buy via env, and it stays off until
// they do.
//
//	AI_WEB_SEARCH_PROVIDER  searxng | tavily | brave   (empty => disabled)
//	AI_WEB_SEARCH_BASE_URL  endpoint base (required for searxng; optional
//	                        override for tavily/brave)
//	AI_WEB_SEARCH_API_KEY   provider API key (required for tavily/brave)
//
// Residency: the HTTP call goes through the SAME guarded client as every AI
// provider, so under local-only mode any NON-local search endpoint is refused
// at dial. A self-hosted SearXNG on the customer's own network keeps web search
// available even in local-only mode; cloud providers (Tavily/Brave) are blocked
// then, by design. The tool is read-only, rate-limited (by the caller), and
// each call is audited content-free.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	registry "github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

const (
	webSearchTimeout        = 8 * time.Second
	webSearchDefaultResults = 5
	webSearchMaxResults     = 10
	webSearchMaxSnippetLen  = 400
	webSearchMaxBodyBytes   = 1 << 20 // 1MB cap on a provider response
)

// ErrWebSearchDisabled is returned when no web-search provider is configured.
var ErrWebSearchDisabled = errors.New("ai: web search is not configured")

// WebSearchResult is one normalized hit (provider-independent shape).
type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

type webSearchConfig struct {
	provider string
	baseURL  string
	apiKey   string
	enabled  bool
}

// loadWebSearchConfig resolves the active web-search config from the live AI
// config (admin settings, with env as the boot fallback baked in by
// LoadAIConfig/BuildConfigFromDB). Before the service is initialized it reads
// env directly so the capability still resolves.
func loadWebSearchConfig() webSearchConfig {
	if cfg := GetConfig(); cfg != nil {
		return webSearchConfig{
			provider: strings.ToLower(strings.TrimSpace(cfg.WebSearchProvider)),
			baseURL:  strings.TrimRight(strings.TrimSpace(cfg.WebSearchBaseURL), "/"),
			apiKey:   strings.TrimSpace(cfg.WebSearchAPIKey),
			enabled:  cfg.WebSearchEnabled,
		}
	}
	prov := strings.ToLower(strings.TrimSpace(os.Getenv("AI_WEB_SEARCH_PROVIDER")))
	return webSearchConfig{
		provider: prov,
		baseURL:  strings.TrimRight(strings.TrimSpace(os.Getenv("AI_WEB_SEARCH_BASE_URL")), "/"),
		apiKey:   strings.TrimSpace(os.Getenv("AI_WEB_SEARCH_API_KEY")),
		enabled:  prov != "",
	}
}

// webSearchConfigValid reports whether the configured provider is enabled and
// has what it needs to run (so a half-configured/disabled env reads as
// "disabled", not "broken").
func webSearchConfigValid(c webSearchConfig) bool {
	if !c.enabled {
		return false
	}
	switch c.provider {
	case "searxng":
		return c.baseURL != ""
	case "tavily", "brave":
		return c.apiKey != ""
	default:
		return false
	}
}

// WebSearchEnabled reports whether a usable web-search provider is configured.
func WebSearchEnabled() bool { return webSearchConfigValid(loadWebSearchConfig()) }

// WebSearchProviderName returns the configured provider id ("" when disabled),
// for status/diagnostics. Never returns the API key.
func WebSearchProviderName() string {
	c := loadWebSearchConfig()
	if !webSearchConfigValid(c) {
		return ""
	}
	return c.provider
}

// WebSearch runs a query against the configured provider and returns normalized
// results. Read-only. The guarded client enforces residency (local-only blocks
// non-local endpoints) and SSRF protection at dial time.
func WebSearch(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	c := loadWebSearchConfig()
	if !webSearchConfigValid(c) {
		return nil, ErrWebSearchDisabled
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("a search query is required")
	}
	if maxResults <= 0 || maxResults > webSearchMaxResults {
		maxResults = webSearchDefaultResults
	}

	// Serve from the short-lived cache when warm: cuts cost/latency for metered
	// providers and shields them from duplicate bursts (several agents asking
	// the same thing). The 10-minute TTL keeps "current" answers fresh enough.
	cacheArgs := webSearchCacheArgs(c.provider, query, maxResults)
	var cached []WebSearchResult
	if found, _ := redisStore.GetJSON(ctx, registry.AIWebSearch, cacheArgs, &cached); found {
		return cached, nil
	}

	client := newProviderHTTPClient(httpClientConfig{timeout: webSearchTimeout, guardSSRF: true})

	var (
		results []WebSearchResult
		err     error
	)
	switch c.provider {
	case "searxng":
		results, err = searxngSearch(ctx, client, c, query, maxResults)
	case "tavily":
		results, err = tavilySearch(ctx, client, c, query, maxResults)
	case "brave":
		results, err = braveSearch(ctx, client, c, query, maxResults)
	default:
		return nil, ErrWebSearchDisabled
	}
	if err != nil {
		return nil, err
	}
	// Cache the result (including an empty set, so a no-hit query isn't
	// re-fetched every time within the window). Best-effort.
	_ = redisStore.SetJSON(ctx, registry.AIWebSearch, cacheArgs, results)
	return results, nil
}

// webSearchCacheArgs builds a deterministic cache key for a (provider, query,
// max) triple: the provider plus a sha256 of the normalized query + result
// count (so distinct queries/sizes never collide and the key stays bounded).
// Pure.
func webSearchCacheArgs(provider, query string, maxResults int) []string {
	norm := strings.ToLower(strings.TrimSpace(query))
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", maxResults, norm)))
	return []string{provider, hex.EncodeToString(sum[:])}
}

// readCappedBody reads at most webSearchMaxBodyBytes so a hostile/huge response
// can't exhaust memory.
func readCappedBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, webSearchMaxBodyBytes))
}

func clampSnippet(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if len(s) > webSearchMaxSnippetLen {
		s = s[:webSearchMaxSnippetLen]
	}
	return s
}

// ── SearXNG (self-hostable; residency-friendly) ───────────────────────────

func searxngSearch(ctx context.Context, client *http.Client, c webSearchConfig, query string, max int) ([]WebSearchResult, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	endpoint := c.baseURL + "/search?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	body, status, err := doWebSearchRequest(client, req)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("search provider returned status %d", status)
	}
	return parseSearxng(body, max)
}

func parseSearxng(body []byte, max int) ([]WebSearchResult, error) {
	var resp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("could not parse search results")
	}
	out := make([]WebSearchResult, 0, max)
	for _, r := range resp.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		out = append(out, WebSearchResult{Title: strings.TrimSpace(r.Title), URL: strings.TrimSpace(r.URL), Snippet: clampSnippet(r.Content)})
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// ── Tavily (cloud API) ─────────────────────────────────────────────────────

func tavilySearch(ctx context.Context, client *http.Client, c webSearchConfig, query string, max int) ([]WebSearchResult, error) {
	base := c.baseURL
	if base == "" {
		base = "https://api.tavily.com"
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"api_key":     c.apiKey,
		"query":       query,
		"max_results": max,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/search", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	body, status, err := doWebSearchRequest(client, req)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("search provider returned status %d", status)
	}
	return parseTavily(body, max)
}

func parseTavily(body []byte, max int) ([]WebSearchResult, error) {
	var resp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("could not parse search results")
	}
	out := make([]WebSearchResult, 0, max)
	for _, r := range resp.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		out = append(out, WebSearchResult{Title: strings.TrimSpace(r.Title), URL: strings.TrimSpace(r.URL), Snippet: clampSnippet(r.Content)})
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// ── Brave Search (cloud API) ───────────────────────────────────────────────

func braveSearch(ctx context.Context, client *http.Client, c webSearchConfig, query string, max int) ([]WebSearchResult, error) {
	base := c.baseURL
	if base == "" {
		base = "https://api.search.brave.com"
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", fmt.Sprintf("%d", max))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/res/v1/web/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", c.apiKey)
	body, status, err := doWebSearchRequest(client, req)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("search provider returned status %d", status)
	}
	return parseBrave(body, max)
}

func parseBrave(body []byte, max int) ([]WebSearchResult, error) {
	var resp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("could not parse search results")
	}
	out := make([]WebSearchResult, 0, max)
	for _, r := range resp.Web.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		out = append(out, WebSearchResult{Title: strings.TrimSpace(r.Title), URL: strings.TrimSpace(r.URL), Snippet: clampSnippet(r.Description)})
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// doWebSearchRequest performs the request and returns the capped body + status.
// A residency block surfaces here as a dial error from the guarded transport.
func doWebSearchRequest(client *http.Client, req *http.Request) ([]byte, int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("web search request failed")
	}
	defer resp.Body.Close()
	body, rerr := readCappedBody(resp.Body)
	if rerr != nil {
		return nil, resp.StatusCode, fmt.Errorf("could not read search response")
	}
	return body, resp.StatusCode, nil
}

// AuditWebSearch records a content-free audit row for a web search (who + the
// provider + result count; never the query text — the same residency posture
// as auditEgressBlocked). Best-effort.
func AuditWebSearch(ctx context.Context, provider string, resultCount int) {
	e := &auditModel.AuditEntry{
		Action:   "ai.web_search",
		Category: auditModel.CategorySecurity,
		Summary:  fmt.Sprintf("AI web search via %s returned %d results", provider, resultCount),
	}
	if ui, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo); ok {
		id := ui.UserPostgresInfo.Id
		e.ActorID = &id
		e.ActorEmail = ui.UserPostgresInfo.EmailID
	}
	_ = auditModel.Insert(ctx, e)
}
