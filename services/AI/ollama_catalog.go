package ai

// Curated Ollama model catalog.
//
// Ollama exposes NO stable public catalog API — ollama.com/library is a
// website, not a queryable endpoint, and scraping it would be brittle. So the
// "browse & install" experience is powered by a curated list that is:
//
//  1. EMBEDDED in the binary as an always-works baseline (critical for
//     air-gapped / offline self-hosted installs), and
//  2. OPTIONALLY refreshed from a remote JSON manifest at a configurable
//     HTTPS URL (AI_OLLAMA_CATALOG_URL), cached in Redis on a TTL. This lets
//     the catalog stay current with newly-published models WITHOUT shipping a
//     new OneCamp build — you edit one hosted JSON file and every instance
//     picks it up within the cache TTL. Any fetch/parse failure transparently
//     falls back to the embedded list.
//
// Either way the list is only for DISCOVERY: the admin can always type ANY
// tag manually and pull it live, so a stale list never blocks installing a
// brand-new model. The list is then annotated at request time with live
// "installed?" state and server-resource feasibility (RAM/disk).
//
// Maintenance: keep the embedded set a small, widely-used baseline. Put the
// fast-moving long tail in the remote manifest. Sizes are approximate download
// sizes for the default quantization and only drive UX hints.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// CatalogCapability is a coarse capability tag used to filter/group the
// catalog in the UI.
type CatalogCapability string

const (
	CapChat      CatalogCapability = "chat"      // general chat / completion
	CapEmbedding CatalogCapability = "embedding" // produces embeddings (RAG)
	CapVision    CatalogCapability = "vision"    // accepts images
	CapCode      CatalogCapability = "code"      // tuned for code
	CapTools     CatalogCapability = "tools"     // supports tool/function calling
	CapReasoning CatalogCapability = "reasoning" // explicit chain-of-thought models
)

// CatalogModel is one curated, installable model.
type CatalogModel struct {
	Tag          string              `json:"tag"`           // exact pull tag, e.g. "llama3.2:3b"
	Family       string              `json:"family"`        // grouping key, e.g. "llama3.2"
	DisplayName  string              `json:"display_name"`  // human label
	Description  string              `json:"description"`   // one-line summary
	Parameters   string              `json:"parameters"`    // e.g. "3B", "8x7B"
	SizeBytes    int64               `json:"size_bytes"`    // approx download size
	MinRAMBytes  int64               `json:"min_ram_bytes"` // recommended RAM to run comfortably
	Capabilities []CatalogCapability `json:"capabilities"`
	Recommended  bool                `json:"recommended"` // featured / good default pick
}

const (
	mb = 1024 * 1024
	gb = 1024 * mb
)

// embeddedOllamaCatalog is the always-available baseline shipped in the binary.
// Ordered roughly by how generally useful / popular each family is; the UI can
// re-sort. The remote manifest (when configured) is layered on top of this.
//
// Verified against ollama.com/library. This is a curated baseline of widely-
// used, LOCALLY-RUNNABLE models (cloud-only tags are intentionally excluded —
// they can't be pulled to local disk). Keep it current-ish, but the long tail
// and brand-new releases are meant to come from the remote manifest
// (AI_OLLAMA_CATALOG_URL) without a rebuild. Sizes are approximate default-
// quantization download sizes and only drive UX hints.
var embeddedOllamaCatalog = []CatalogModel{
	// ── Default ───────────────────────────────────────────────────────
	// The model a new local install pulls (OLLAMA_MODEL's default). Chosen by
	// running agent jobs on a 4-core CPU, 3 Oct 2026: 12 of 15 passed at 5.5 s
	// a reply, against 6 of 15 for llama3.2:3b.
	{
		Tag: "qwen3:4b-instruct", Family: "qwen3", DisplayName: "Qwen 3 4B Instruct",
		Description: "The default: reliable at agent work and tool use on an ordinary CPU, and answers directly without a reasoning pause.",
		Parameters:  "4B", SizeBytes: 2500 * mb, MinRAMBytes: 8 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools}, Recommended: true,
	},

	// ── Llama (Meta) ──────────────────────────────────────────────────
	{
		Tag: "llama3.2:3b", Family: "llama3.2", DisplayName: "Llama 3.2 3B",
		Description: "Small and fast. Weaker than Qwen 3 4B Instruct at agent work; fine for plain chat on modest hardware.",
		Parameters:  "3B", SizeBytes: 2 * gb, MinRAMBytes: 8 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools},
	},
	{
		Tag: "llama3.2:1b", Family: "llama3.2", DisplayName: "Llama 3.2 1B",
		Description: "Tiny model for very low-resource servers and quick tasks.",
		Parameters:  "1B", SizeBytes: 1300 * mb, MinRAMBytes: 4 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools},
	},
	{
		Tag: "llama3.1:8b", Family: "llama3.1", DisplayName: "Llama 3.1 8B",
		Description: "Balanced quality and speed. A reliable all-rounder for most workspaces.",
		Parameters:  "8B", SizeBytes: 4900 * mb, MinRAMBytes: 16 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools},
	},
	{
		Tag: "llama3.3:70b", Family: "llama3.3", DisplayName: "Llama 3.3 70B",
		Description: "Frontier-class quality rivaling much larger models. Needs a powerful server (lots of RAM or a GPU).",
		Parameters:  "70B", SizeBytes: 43 * gb, MinRAMBytes: 64 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools},
	},

	// ── Gemma (Google) ────────────────────────────────────────────────
	{
		Tag: "gemma4:e4b", Family: "gemma4", DisplayName: "Gemma 4 E4B",
		Description: "Google's latest. Multimodal edge model (~4B effective) with reasoning and a 128K context. Strong default.",
		Parameters:  "4B-eff", SizeBytes: 9600 * mb, MinRAMBytes: 16 * gb,
		Capabilities: []CatalogCapability{CapChat, CapVision, CapTools, CapReasoning}, Recommended: true,
	},
	{
		Tag: "gemma4:e2b", Family: "gemma4", DisplayName: "Gemma 4 E2B",
		Description: "Smaller Gemma 4 edge model (~2B effective) for laptops and low-resource servers.",
		Parameters:  "2B-eff", SizeBytes: 7200 * mb, MinRAMBytes: 12 * gb,
		Capabilities: []CatalogCapability{CapChat, CapVision, CapTools, CapReasoning},
	},
	{
		Tag: "gemma4:26b", Family: "gemma4", DisplayName: "Gemma 4 26B (MoE)",
		Description: "Mixture-of-experts Gemma 4 (≈4B active) with a 256K context for higher-quality local answers.",
		Parameters:  "26B", SizeBytes: 18 * gb, MinRAMBytes: 32 * gb,
		Capabilities: []CatalogCapability{CapChat, CapVision, CapTools, CapReasoning},
	},
	{
		Tag: "gemma4:31b", Family: "gemma4", DisplayName: "Gemma 4 31B",
		Description: "Dense, frontier-quality Gemma 4 with a 256K context for powerful servers.",
		Parameters:  "31B", SizeBytes: 20 * gb, MinRAMBytes: 48 * gb,
		Capabilities: []CatalogCapability{CapChat, CapVision, CapTools, CapReasoning},
	},
	{
		Tag: "gemma3:4b", Family: "gemma3", DisplayName: "Gemma 3 4B",
		Description: "Efficient multimodal Google model that runs comfortably on a single GPU.",
		Parameters:  "4B", SizeBytes: 3300 * mb, MinRAMBytes: 8 * gb,
		Capabilities: []CatalogCapability{CapChat, CapVision}, Recommended: true,
	},
	{
		Tag: "gemma3:12b", Family: "gemma3", DisplayName: "Gemma 3 12B",
		Description: "Larger Gemma 3 with stronger reasoning and vision on a single GPU.",
		Parameters:  "12B", SizeBytes: 8100 * mb, MinRAMBytes: 24 * gb,
		Capabilities: []CatalogCapability{CapChat, CapVision},
	},
	{
		Tag: "gemma3:27b", Family: "gemma3", DisplayName: "Gemma 3 27B",
		Description: "High-quality Gemma 3 for bigger servers; multimodal with a long context.",
		Parameters:  "27B", SizeBytes: 17 * gb, MinRAMBytes: 32 * gb,
		Capabilities: []CatalogCapability{CapChat, CapVision},
	},
	{
		Tag: "gemma3:1b", Family: "gemma3", DisplayName: "Gemma 3 1B",
		Description: "Compact text-only Gemma 3 for lightweight local chat.",
		Parameters:  "1B", SizeBytes: 815 * mb, MinRAMBytes: 4 * gb,
		Capabilities: []CatalogCapability{CapChat},
	},

	// ── Qwen (Alibaba) ────────────────────────────────────────────────
	{
		Tag: "qwen3:8b", Family: "qwen3", DisplayName: "Qwen 3 8B",
		Description: "Latest-generation Qwen with hybrid thinking, tool use, and strong multilingual quality.",
		Parameters:  "8B", SizeBytes: 5200 * mb, MinRAMBytes: 16 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools, CapReasoning}, Recommended: true,
	},
	{
		Tag: "qwen3:4b", Family: "qwen3", DisplayName: "Qwen 3 4B",
		Description: "Small Qwen 3 with thinking and tool use for modest hardware.",
		Parameters:  "4B", SizeBytes: 2600 * mb, MinRAMBytes: 8 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools, CapReasoning},
	},
	{
		Tag: "qwen3:14b", Family: "qwen3", DisplayName: "Qwen 3 14B",
		Description: "Higher-quality Qwen 3 for servers with more headroom.",
		Parameters:  "14B", SizeBytes: 9300 * mb, MinRAMBytes: 32 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools, CapReasoning},
	},
	{
		Tag: "qwen3:30b", Family: "qwen3", DisplayName: "Qwen 3 30B (MoE)",
		Description: "Mixture-of-experts Qwen 3 (≈3B active) — high quality with efficient inference.",
		Parameters:  "30B", SizeBytes: 19 * gb, MinRAMBytes: 32 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools, CapReasoning},
	},
	{
		Tag: "qwen2.5:7b", Family: "qwen2.5", DisplayName: "Qwen 2.5 7B",
		Description: "Proven multilingual general model with a 128K context. A solid, lighter alternative to Qwen 3.",
		Parameters:  "7B", SizeBytes: 4700 * mb, MinRAMBytes: 16 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools},
	},

	// ── gpt-oss (OpenAI open weights) ─────────────────────────────────
	{
		Tag: "gpt-oss:20b", Family: "gpt-oss", DisplayName: "gpt-oss 20B",
		Description: "OpenAI's open-weight model for strong reasoning and agentic/tool use. Great quality-to-size.",
		Parameters:  "20B", SizeBytes: 14 * gb, MinRAMBytes: 24 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools, CapReasoning}, Recommended: true,
	},
	{
		Tag: "gpt-oss:120b", Family: "gpt-oss", DisplayName: "gpt-oss 120B",
		Description: "Large OpenAI open-weight model for frontier reasoning on powerful servers.",
		Parameters:  "120B", SizeBytes: 65 * gb, MinRAMBytes: 80 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools, CapReasoning},
	},

	// ── DeepSeek-R1 (reasoning) ───────────────────────────────────────
	{
		Tag: "deepseek-r1:8b", Family: "deepseek-r1", DisplayName: "DeepSeek-R1 8B",
		Description: "Open reasoning model that thinks step-by-step; performance approaching leading closed models.",
		Parameters:  "8B", SizeBytes: 5200 * mb, MinRAMBytes: 16 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools, CapReasoning},
	},
	{
		Tag: "deepseek-r1:14b", Family: "deepseek-r1", DisplayName: "DeepSeek-R1 14B",
		Description: "Stronger DeepSeek reasoning for harder analytical and math tasks.",
		Parameters:  "14B", SizeBytes: 9 * gb, MinRAMBytes: 32 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools, CapReasoning},
	},
	{
		Tag: "deepseek-r1:32b", Family: "deepseek-r1", DisplayName: "DeepSeek-R1 32B",
		Description: "High-end DeepSeek reasoning for demanding workloads on bigger servers.",
		Parameters:  "32B", SizeBytes: 20 * gb, MinRAMBytes: 32 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools, CapReasoning},
	},

	// ── Phi (Microsoft) ───────────────────────────────────────────────
	{
		Tag: "phi4:14b", Family: "phi4", DisplayName: "Phi-4 14B",
		Description: "State-of-the-art small Microsoft model with strong math and reasoning.",
		Parameters:  "14B", SizeBytes: 9100 * mb, MinRAMBytes: 24 * gb,
		Capabilities: []CatalogCapability{CapChat},
	},
	{
		Tag: "phi4-mini:3.8b", Family: "phi4-mini", DisplayName: "Phi-4 Mini",
		Description: "Compact Phi-4 with multilingual support, reasoning, and function calling.",
		Parameters:  "3.8B", SizeBytes: 2500 * mb, MinRAMBytes: 8 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools},
	},

	// ── Mistral ───────────────────────────────────────────────────────
	{
		Tag: "mistral:7b", Family: "mistral", DisplayName: "Mistral 7B",
		Description: "Fast, popular open model. A solid lightweight default with tool support.",
		Parameters:  "7B", SizeBytes: 4100 * mb, MinRAMBytes: 16 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools},
	},
	{
		Tag: "mistral-small3.2:24b", Family: "mistral-small3.2", DisplayName: "Mistral Small 3.2 24B",
		Description: "Capable mid-size Mistral with vision and improved function calling.",
		Parameters:  "24B", SizeBytes: 15 * gb, MinRAMBytes: 32 * gb,
		Capabilities: []CatalogCapability{CapChat, CapVision, CapTools},
	},
	{
		Tag: "mistral-nemo:12b", Family: "mistral-nemo", DisplayName: "Mistral Nemo 12B",
		Description: "12B Mistral/NVIDIA model with a 128K context and tool support.",
		Parameters:  "12B", SizeBytes: 7100 * mb, MinRAMBytes: 24 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools},
	},

	// ── Code-specialized ──────────────────────────────────────────────
	{
		Tag: "qwen2.5-coder:7b", Family: "qwen2.5-coder", DisplayName: "Qwen 2.5 Coder 7B",
		Description: "Code-specialized model for code Q&A, generation, and review.",
		Parameters:  "7B", SizeBytes: 4700 * mb, MinRAMBytes: 16 * gb,
		Capabilities: []CatalogCapability{CapChat, CapCode, CapTools}, Recommended: true,
	},
	{
		Tag: "qwen3-coder:30b", Family: "qwen3-coder", DisplayName: "Qwen 3 Coder 30B",
		Description: "Long-context Qwen 3 model tuned for agentic and coding tasks.",
		Parameters:  "30B", SizeBytes: 19 * gb, MinRAMBytes: 32 * gb,
		Capabilities: []CatalogCapability{CapChat, CapCode, CapTools},
	},

	// ── Reasoning (dedicated) ─────────────────────────────────────────
	{
		Tag: "qwq:32b", Family: "qwq", DisplayName: "QwQ 32B",
		Description: "Qwen's dedicated reasoning model for complex problem solving.",
		Parameters:  "32B", SizeBytes: 20 * gb, MinRAMBytes: 32 * gb,
		Capabilities: []CatalogCapability{CapChat, CapTools, CapReasoning},
	},

	// ── Vision ────────────────────────────────────────────────────────
	{
		Tag: "qwen2.5vl:7b", Family: "qwen2.5vl", DisplayName: "Qwen 2.5 VL 7B",
		Description: "Flagship Qwen vision-language model for image understanding and OCR.",
		Parameters:  "7B", SizeBytes: 6 * gb, MinRAMBytes: 16 * gb,
		Capabilities: []CatalogCapability{CapChat, CapVision},
	},
	{
		Tag: "llama3.2-vision:11b", Family: "llama3.2-vision", DisplayName: "Llama 3.2 Vision 11B",
		Description: "Multimodal Llama that reads images alongside text.",
		Parameters:  "11B", SizeBytes: 7900 * mb, MinRAMBytes: 16 * gb,
		Capabilities: []CatalogCapability{CapChat, CapVision},
	},
	{
		Tag: "llava:7b", Family: "llava", DisplayName: "LLaVA 7B",
		Description: "Classic vision-language model for describing and reasoning over images.",
		Parameters:  "7B", SizeBytes: 4700 * mb, MinRAMBytes: 16 * gb,
		Capabilities: []CatalogCapability{CapChat, CapVision},
	},

	// ── Embeddings (RAG / semantic search) ────────────────────────────
	{
		Tag: "nomic-embed-text", Family: "nomic-embed-text", DisplayName: "Nomic Embed Text",
		Description: "Default embedding model for semantic search. 768-dim, fast, tiny, large context.",
		Parameters:  "137M", SizeBytes: 274 * mb, MinRAMBytes: 2 * gb,
		Capabilities: []CatalogCapability{CapEmbedding}, Recommended: true,
	},
	{
		Tag: "embeddinggemma:300m", Family: "embeddinggemma", DisplayName: "EmbeddingGemma 300M",
		Description: "Google's compact 300M embedding model (768-dim) for efficient retrieval.",
		Parameters:  "300M", SizeBytes: 622 * mb, MinRAMBytes: 2 * gb,
		Capabilities: []CatalogCapability{CapEmbedding},
	},
	{
		Tag: "mxbai-embed-large", Family: "mxbai-embed-large", DisplayName: "mxbai Embed Large",
		Description: "Higher-quality embeddings (1024-dim) from mixedbread.ai for better retrieval.",
		Parameters:  "335M", SizeBytes: 670 * mb, MinRAMBytes: 2 * gb,
		Capabilities: []CatalogCapability{CapEmbedding},
	},
	{
		Tag: "bge-m3", Family: "bge-m3", DisplayName: "BGE-M3",
		Description: "Multilingual embedding model (1024-dim) with long-document, multi-granularity support.",
		Parameters:  "567M", SizeBytes: 1200 * mb, MinRAMBytes: 4 * gb,
		Capabilities: []CatalogCapability{CapEmbedding},
	},
	{
		Tag: "qwen3-embedding:0.6b", Family: "qwen3-embedding", DisplayName: "Qwen 3 Embedding 0.6B",
		Description: "Qwen 3-based embedding model with strong multilingual retrieval quality.",
		Parameters:  "0.6B", SizeBytes: 639 * mb, MinRAMBytes: 2 * gb,
		Capabilities: []CatalogCapability{CapEmbedding},
	},
	{
		Tag: "all-minilm", Family: "all-minilm", DisplayName: "all-MiniLM",
		Description: "Very small embedding model (384-dim) for low-resource servers.",
		Parameters:  "23M", SizeBytes: 46 * mb, MinRAMBytes: 1 * gb,
		Capabilities: []CatalogCapability{CapEmbedding},
	},
}

// OllamaCatalog returns the curated catalog: the remote manifest merged over
// the embedded baseline when a manifest URL is configured and reachable, else
// the embedded baseline alone. The returned slice is a fresh copy safe for the
// caller to annotate.
//
// "Merge" semantics: remote entries are layered on top of embedded ones keyed
// by Tag — a remote entry with the same Tag overrides the embedded metadata
// (so we can correct sizes/descriptions remotely), and brand-new remote tags
// are appended. Embedded-only tags always remain, so a broken/empty manifest
// can never shrink the baseline.
func OllamaCatalog() []CatalogModel {
	embedded := embeddedOllamaCatalog
	remote := cachedRemoteCatalog(context.Background())
	if len(remote) == 0 {
		out := make([]CatalogModel, len(embedded))
		copy(out, embedded)
		return out
	}
	return mergeCatalogs(embedded, remote)
}

// mergeCatalogs layers remote over embedded by Tag (remote wins), preserving
// embedded order first, then appending net-new remote tags in their order.
func mergeCatalogs(embedded, remote []CatalogModel) []CatalogModel {
	remoteByTag := make(map[string]CatalogModel, len(remote))
	for _, m := range remote {
		if strings.TrimSpace(m.Tag) != "" {
			remoteByTag[m.Tag] = m
		}
	}

	out := make([]CatalogModel, 0, len(embedded)+len(remote))
	seen := make(map[string]bool, len(embedded)+len(remote))
	for _, e := range embedded {
		if r, ok := remoteByTag[e.Tag]; ok {
			out = append(out, normalizeCatalogModel(r))
		} else {
			out = append(out, e)
		}
		seen[e.Tag] = true
	}
	for _, r := range remote {
		if strings.TrimSpace(r.Tag) == "" || seen[r.Tag] {
			continue
		}
		out = append(out, normalizeCatalogModel(r))
		seen[r.Tag] = true
	}
	return out
}

// normalizeCatalogModel defensively fills/repairs a remote entry so a sloppy
// manifest can't produce a broken UI row.
func normalizeCatalogModel(m CatalogModel) CatalogModel {
	if m.Family == "" {
		// Derive a family from the tag prefix before ':' as a fallback.
		if i := strings.IndexByte(m.Tag, ':'); i > 0 {
			m.Family = m.Tag[:i]
		} else {
			m.Family = m.Tag
		}
	}
	if m.DisplayName == "" {
		m.DisplayName = m.Tag
	}
	return m
}

// --- Remote manifest fetching -------------------------------------------

// remoteCatalogCache holds the last successfully-fetched remote manifest in
// process, as a fast path in front of Redis and as a final fallback if Redis
// is unavailable. Guarded by remoteCatalogMu.
var (
	remoteCatalogMu      sync.RWMutex
	remoteCatalogProcess []CatalogModel
)

// ollamaCatalogURL returns the configured remote manifest URL (or "").
func ollamaCatalogURL() string {
	return strings.TrimSpace(os.Getenv("AI_OLLAMA_CATALOG_URL"))
}

// cachedRemoteCatalog returns the remote manifest, preferring the Redis cache,
// then a fresh fetch, then the in-process copy. Returns nil when no manifest
// URL is configured or every path fails (caller falls back to embedded).
//
// All failures are silent-by-design: the catalog is a convenience, and an
// offline/air-gapped install MUST keep working on the embedded baseline.
func cachedRemoteCatalog(ctx context.Context) []CatalogModel {
	url := ollamaCatalogURL()
	if url == "" {
		return nil
	}

	// 1. Redis cache (shared across instances, TTL-bounded).
	if models, ok := loadRemoteCatalogFromCache(ctx); ok {
		setProcessRemoteCatalog(models)
		return models
	}

	// 2. Fresh fetch, then populate the cache.
	models, err := fetchRemoteCatalog(ctx, url)
	if err == nil && len(models) > 0 {
		storeRemoteCatalogInCache(ctx, models)
		setProcessRemoteCatalog(models)
		return models
	}

	// 3. Last-known in-process copy (survives a transient outage).
	remoteCatalogMu.RLock()
	defer remoteCatalogMu.RUnlock()
	return remoteCatalogProcess
}

func setProcessRemoteCatalog(models []CatalogModel) {
	remoteCatalogMu.Lock()
	remoteCatalogProcess = models
	remoteCatalogMu.Unlock()
}

// remoteCatalogManifest is the wire shape of the hosted JSON. A top-level
// "models" array keeps room for future metadata (e.g. a schema version).
type remoteCatalogManifest struct {
	Version int            `json:"version,omitempty"`
	Models  []CatalogModel `json:"models"`
}

// fetchRemoteCatalog GETs and parses the manifest. Accepts either the wrapped
// {"models":[...]} shape or a bare [...] array for authoring convenience.
func fetchRemoteCatalog(ctx context.Context, url string) ([]CatalogModel, error) {
	if !strings.HasPrefix(strings.ToLower(url), "https://") {
		return nil, fmt.Errorf("AI_OLLAMA_CATALOG_URL must be an https URL")
	}

	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog manifest returned %d", resp.StatusCode)
	}

	// Bound the body so a hostile/misconfigured URL can't exhaust memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}

	var manifest remoteCatalogManifest
	if err := json.Unmarshal(body, &manifest); err == nil && len(manifest.Models) > 0 {
		return sanitizeRemoteCatalog(manifest.Models), nil
	}
	// Fallback: a bare array.
	var bare []CatalogModel
	if err := json.Unmarshal(body, &bare); err == nil && len(bare) > 0 {
		return sanitizeRemoteCatalog(bare), nil
	}
	return nil, fmt.Errorf("catalog manifest is empty or malformed")
}

// sanitizeRemoteCatalog drops entries without a tag and caps the list size so
// a runaway manifest can't bloat the admin payload.
func sanitizeRemoteCatalog(in []CatalogModel) []CatalogModel {
	const maxEntries = 500
	out := make([]CatalogModel, 0, len(in))
	for _, m := range in {
		if strings.TrimSpace(m.Tag) == "" {
			continue
		}
		out = append(out, m)
		if len(out) >= maxEntries {
			break
		}
	}
	return out
}

// RefreshOllamaCatalog forces a remote-manifest re-fetch, bypassing the Redis
// cache, and returns the merged catalog. Used by the admin "refresh catalog"
// action so a freshly-published manifest can be pulled on demand. Returns the
// embedded baseline when no manifest URL is configured.
func RefreshOllamaCatalog(ctx context.Context) ([]CatalogModel, error) {
	url := ollamaCatalogURL()
	if url == "" {
		return OllamaCatalog(), nil
	}
	models, err := fetchRemoteCatalog(ctx, url)
	if err != nil {
		// Keep serving whatever we have; report the error for admin feedback.
		return OllamaCatalog(), err
	}
	storeRemoteCatalogInCache(ctx, models)
	setProcessRemoteCatalog(models)
	return mergeCatalogs(embeddedOllamaCatalog, models), nil
}

// loadRemoteCatalogFromCache reads the cached manifest from Redis. Returns
// (nil, false) on a miss or any error so the caller proceeds to fetch.
func loadRemoteCatalogFromCache(ctx context.Context) ([]CatalogModel, bool) {
	var models []CatalogModel
	found, err := redisStore.GetJSON(ctx, registry.AIOllamaCatalog, nil, &models)
	if err != nil || !found || len(models) == 0 {
		return nil, false
	}
	return models, true
}

// storeRemoteCatalogInCache writes the manifest to Redis (best-effort).
func storeRemoteCatalogInCache(ctx context.Context, models []CatalogModel) {
	_ = redisStore.SetJSON(ctx, registry.AIOllamaCatalog, nil, models)
}
