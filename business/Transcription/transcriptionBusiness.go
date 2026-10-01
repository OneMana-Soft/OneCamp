// Package business (Transcription) is the admin-managed configuration layer
// for call transcription. It follows the exact DB-first / ENV-fallback pattern
// used by business/Settings (system_configs + helpers.EncryptSecret), so an
// admin can change the transcription mode and the STT model at runtime from the
// admin panel WITHOUT a redeploy or a frontend rebuild.
//
// STT is model-agnostic by design — mirroring how migration 64 made the LLM
// layer provider-agnostic. The config carries a provider KIND plus a free-text
// model, an optional base URL, an optional language, and a single generic
// (encrypted) API key. The Python agent builds the matching LiveKit STT plugin
// from these at room-join, so plugging in a new STT (Deepgram, Google, OpenAI
// Whisper, Groq, a self-hosted Whisper, or any OpenAI-compatible endpoint) is a
// data change, not a code change.
//
// Three consumers read this config:
//   - the FE (FrontendTranscriber) reads `mode` via GET /config/client so the
//     browser Web-Speech path turns on/off at runtime instead of being baked
//     into the bundle via NEXT_PUBLIC_TRANSCRIPTION_MODE.
//   - the Python transcription agent reads the full resolved config (including
//     the decrypted STT key) via the internal-secret endpoint
//     GET /livekit/transcription-config at room-join.
//   - the admin panel reads a redacted view (GET /admin/transcription/config)
//     and writes it (POST /admin/transcription/config).
//
// Secrets (the STT API key, Google credentials JSON) are encrypted at rest with
// helpers.EncryptSecret and are NEVER returned to the FE — only has_* flags.
// The agent endpoint is the only path that returns decrypted secret material,
// and it is gated by the shared INTERNAL_SECRET (server-to-server only).
package business

import (
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// Config keys in system_configs.
const (
	keyMode              = "transcription_mode"               // frontend | backend | off
	keySTTProvider       = "transcription_stt_provider"       // deepgram | google | openai
	keySTTModel          = "transcription_stt_model"          // free-text model (nova-2, whisper-1, …)
	keySTTBaseURL        = "transcription_stt_base_url"       // openai-compatible endpoint (optional)
	keySTTLanguage       = "transcription_stt_language"       // BCP-47 hint (optional, e.g. "en")
	keySTTAPIKey         = "transcription_stt_api_key"        // encrypted generic key (deepgram/openai)
	keyGoogleCredentials = "transcription_google_credentials" // encrypted service-account JSON (google)
	// Legacy key, still read as a fallback so existing installs keep working
	// after the generic key was introduced.
	keyLegacyDeepgramAPIKey = "transcription_deepgram_api_key"
)

// Modes.
const (
	ModeFrontend = "frontend" // browser Web Speech API; zero cost, no key
	ModeBackend  = "backend"  // server-side STT via the Python agent
	ModeOff      = "off"      // transcription disabled entirely
)

// STT provider kinds (backend mode only). The kind selects which LiveKit
// plugin the agent instantiates:
//   - deepgram → livekit-plugins-deepgram
//   - google   → livekit-plugins-google
//   - openai   → livekit-plugins-openai, which also covers ANY OpenAI-
//     compatible STT endpoint (OpenAI Whisper, Groq, a self-hosted
//     faster-whisper / vLLM server, …) via STTBaseURL.
const (
	STTDeepgram = "deepgram"
	STTGoogle   = "google"
	STTOpenAI   = "openai"
	// STTLocal is the Whisper server bundled with OneCamp, running on the
	// customer's own machine.
	//
	// Its own provider rather than "pick OpenAI-compatible and type a URL"
	// because it is a different decision, not a different endpoint: it is the
	// only option where the audio of a meeting never leaves the server, and
	// where transcription costs nothing per minute. An admin choosing where
	// their meetings are sent should not have to infer that from a text field.
	STTLocal = "local"
)

// localSTTBaseURL is where the bundled Whisper server answers on the stack
// network. NOT admin-editable, which is what makes it safe to probe: the
// OpenAI-compatible provider runs its URL through the SSRF guard because an
// admin types that one, and this URL is a constant nobody outside the server
// chooses. Overridable by env only, for an operator running the container
// somewhere else on their own network.
const localSTTBaseURL = "http://whisper:9000/v1"

// LocalSTTAPIKey is the credential for the bundled server.
//
// The stack's existing internal secret, shared with the agent-to-backend hop,
// rather than a new variable every operator would have to generate and keep in
// step. Same trust boundary: both ends are our own containers on a network with
// no published ports. Empty on a misconfigured install, which surfaces as the
// Test button failing rather than as silence.
func LocalSTTAPIKey() string {
	return strings.TrimSpace(os.Getenv("INTERNAL_SECRET"))
}

// LocalSTTBaseURL is the effective endpoint for the bundled server.
func LocalSTTBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("STT_LOCAL_BASE_URL")); v != "" {
		return v
	}
	return localSTTBaseURL
}

// EffectiveSTTBaseURL is the endpoint the probe and the agent should both use.
//
// One function so those two can never disagree about where the audio goes,
// which is the kind of split that makes a green "Test" button sit above a
// transcriber talking to something else.
func EffectiveSTTBaseURL() string {
	if STTProvider() == STTLocal {
		return LocalSTTBaseURL()
	}
	return STTBaseURL()
}

// UsesAPIKey reports whether a provider needs a credential FROM THE ADMIN.
//
// The bundled server is not an exception because it is unauthenticated. It
// requires a bearer token on every request, including /v1/models, and refuses
// everything without one. The token is the stack's own internal secret, which
// the server already has and the admin should never be asked to type.
func UsesAPIKey(provider string) bool {
	switch provider {
	case STTDeepgram, STTOpenAI, STTGoogle:
		return true
	default:
		return false
	}
}

// validSTTProviders is the allow-list the controller validates against and the
// FE renders as a dropdown.
var validSTTProviders = map[string]bool{
	STTDeepgram: true,
	STTGoogle:   true,
	STTOpenAI:   true,
	STTLocal:    true,
}

// ValidSTTProviders lists the accepted providers, sorted, for error messages.
//
// Derived from the allow-list rather than written out at the call site: the
// controller's "must be one of" message already had a hardcoded list, and it
// was wrong the moment a provider was added, telling an admin that the option
// the UI had just offered them was not accepted.
func ValidSTTProviders() []string {
	out := make([]string, 0, len(validSTTProviders))
	for p := range validSTTProviders {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// defaultModelForProvider gives each provider a sane out-of-the-box model so an
// admin can switch provider without also having to know a model name. Empty
// means "let the plugin use its own default".
func defaultModelForProvider(provider string) string {
	switch provider {
	case STTDeepgram:
		return "nova-2"
	case STTOpenAI, STTLocal:
		// The OpenAI audio API requires a model field; every Whisper server
		// accepts this name and serves whichever weights it was started with.
		return "whisper-1"
	default: // google and anything else: plugin default
		return ""
	}
}

const (
	defaultMode        = ModeFrontend
	defaultSTTProvider = STTDeepgram
)

var (
	mu      sync.RWMutex
	cache   map[string]string
	expires time.Time
)

const cacheTTL = 30 * time.Second

// allConfigKeys is the set loaded in one batched read.
var allConfigKeys = []string{
	keyMode, keySTTProvider, keySTTModel, keySTTBaseURL, keySTTLanguage,
	keySTTAPIKey, keyGoogleCredentials, keyLegacyDeepgramAPIKey,
}

// loadAll pulls the transcription keys once (cached). Missing keys simply
// aren't in the map, so callers apply their own env/default fallback.
func loadAll() map[string]string {
	mu.RLock()
	if cache != nil && time.Now().Before(expires) {
		c := cache
		mu.RUnlock()
		return c
	}
	mu.RUnlock()

	out := map[string]string{}
	if postgresInit.DBConn != nil && postgresInit.DBConn.SqlDB != nil {
		rows, err := configModel.GetMultipleConfigsByKeys(allConfigKeys)
		if err == nil {
			for _, row := range rows {
				out[row.Key] = row.Value
			}
		}
	}

	mu.Lock()
	cache = out
	expires = time.Now().Add(cacheTTL)
	mu.Unlock()
	return out
}

func invalidate() {
	mu.Lock()
	cache = nil
	expires = time.Time{}
	mu.Unlock()
}

// normalizeMode coerces any value to a valid mode, defaulting to frontend.
func normalizeMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case ModeBackend:
		return ModeBackend
	case ModeOff:
		return ModeOff
	case ModeFrontend:
		return ModeFrontend
	default:
		return defaultMode
	}
}

// normalizeSTT coerces any value to a valid STT provider, defaulting to deepgram.
func normalizeSTT(v string) string {
	p := strings.ToLower(strings.TrimSpace(v))
	if validSTTProviders[p] {
		return p
	}
	return defaultSTTProvider
}

// IsValidSTTProvider reports whether the provider is in the allow-list. Used by
// the controller for precise 400s.
func IsValidSTTProvider(v string) bool {
	return validSTTProviders[strings.ToLower(strings.TrimSpace(v))]
}

// Mode returns the effective transcription mode. DB-first, then the
// TRANSCRIPTION_MODE env var, then "frontend". Always a valid value.
func Mode() string {
	all := loadAll()
	if v, ok := all[keyMode]; ok && v != "" {
		return normalizeMode(v)
	}
	if env := os.Getenv("TRANSCRIPTION_MODE"); env != "" {
		return normalizeMode(env)
	}
	return defaultMode
}

// STTProvider returns the effective backend STT provider. DB-first, then the
// STT_PROVIDER env var, then "deepgram". Always a valid value.
func STTProvider() string {
	all := loadAll()
	if v, ok := all[keySTTProvider]; ok && v != "" {
		return normalizeSTT(v)
	}
	if env := os.Getenv("STT_PROVIDER"); env != "" {
		return normalizeSTT(env)
	}
	return defaultSTTProvider
}

// STTModel returns the effective model name for the active provider. DB-first,
// then the STT_MODEL env var, then the provider's built-in default.
func STTModel() string {
	all := loadAll()
	if v, ok := all[keySTTModel]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if env := strings.TrimSpace(os.Getenv("STT_MODEL")); env != "" {
		return env
	}
	return defaultModelForProvider(STTProvider())
}

// STTBaseURL returns the OpenAI-compatible base URL for the openai provider
// kind (e.g. a self-hosted Whisper or Groq endpoint). DB-first, then
// STT_BASE_URL env. Empty = use the plugin's default (api.openai.com).
func STTBaseURL() string {
	all := loadAll()
	if v, ok := all[keySTTBaseURL]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(os.Getenv("STT_BASE_URL"))
}

// STTLanguage returns the optional BCP-47 language hint. DB-first, then
// STT_LANGUAGE env. Empty = provider auto-detect.
func STTLanguage() string {
	all := loadAll()
	if v, ok := all[keySTTLanguage]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(os.Getenv("STT_LANGUAGE"))
}

// STTAPIKey returns the decrypted generic STT API key (used by deepgram and
// openai-compatible providers). DB-first (generic key, then the legacy
// deepgram-specific key for back-compat), then the DEEPGRAM_API_KEY /
// OPENAI_API_KEY env vars. Empty = not configured.
func STTAPIKey() string {
	all := loadAll()
	if enc, ok := all[keySTTAPIKey]; ok && enc != "" {
		if dec, err := helpers.DecryptSecret(enc); err == nil && dec != "" {
			return dec
		}
	}
	// Legacy generic-less installs stored the key under the deepgram-specific
	// name; honor it so an upgrade doesn't silently break transcription.
	if enc, ok := all[keyLegacyDeepgramAPIKey]; ok && enc != "" {
		if dec, err := helpers.DecryptSecret(enc); err == nil && dec != "" {
			return dec
		}
	}
	switch STTProvider() {
	case STTOpenAI:
		if v := os.Getenv("OPENAI_API_KEY"); v != "" {
			return v
		}
		return os.Getenv("DEEPGRAM_API_KEY")
	default:
		return os.Getenv("DEEPGRAM_API_KEY")
	}
}

// hasSTTAPIKey reports whether an STT key exists in DB or env, without
// decrypting/returning it.
func hasSTTAPIKey(all map[string]string) (bool, string) {
	if enc, ok := all[keySTTAPIKey]; ok && enc != "" {
		return true, "db"
	}
	if enc, ok := all[keyLegacyDeepgramAPIKey]; ok && enc != "" {
		return true, "db"
	}
	if os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("DEEPGRAM_API_KEY") != "" {
		return true, "env"
	}
	return false, "none"
}

// GoogleCredentials returns the decrypted Google service-account JSON. DB-first;
// the env path historically points GOOGLE_APPLICATION_CREDENTIALS at a FILE,
// so there is no plaintext env fallback here — the agent keeps using the file
// path when the DB value is empty. Empty = use the env/file path.
func GoogleCredentials() string {
	all := loadAll()
	if enc, ok := all[keyGoogleCredentials]; ok && enc != "" {
		if dec, err := helpers.DecryptSecret(enc); err == nil && dec != "" {
			return dec
		}
	}
	return ""
}

// sourceOf reports where an effective value comes from (for the admin UI).
func sourceOf(dbKey, envKey string) string {
	all := loadAll()
	if v, ok := all[dbKey]; ok && v != "" {
		return "db"
	}
	if envKey != "" && os.Getenv(envKey) != "" {
		return "env"
	}
	return "default"
}

// --- Admin view + save ---

// Status is the redacted admin-facing view. Secrets surface only as has_*
// booleans + source, never their value.
type Status struct {
	Mode                 string `json:"mode"`
	ModeSource           string `json:"mode_source"` // db | env | default
	STTProvider          string `json:"stt_provider"`
	STTProviderSource    string `json:"stt_provider_source"`
	STTModel             string `json:"stt_model"`
	STTBaseURL           string `json:"stt_base_url"`
	STTLanguage          string `json:"stt_language"`
	HasSTTAPIKey         bool   `json:"has_stt_api_key"`
	STTAPIKeySource      string `json:"stt_api_key_source"` // db | env | none
	HasGoogleCredentials bool   `json:"has_google_credentials"`
	GoogleSource         string `json:"google_source"` // db | env | none
}

// GetStatus returns the redacted settings view for the admin UI.
func GetStatus() Status {
	all := loadAll()

	hasKey, keySrc := hasSTTAPIKey(all)

	hasGoogle := false
	googleSrc := "none"
	if enc, ok := all[keyGoogleCredentials]; ok && enc != "" {
		hasGoogle, googleSrc = true, "db"
	} else if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
		hasGoogle, googleSrc = true, "env"
	}

	return Status{
		Mode:                 Mode(),
		ModeSource:           sourceOf(keyMode, "TRANSCRIPTION_MODE"),
		STTProvider:          STTProvider(),
		STTProviderSource:    sourceOf(keySTTProvider, "STT_PROVIDER"),
		STTModel:             STTModel(),
		STTBaseURL:           STTBaseURL(),
		STTLanguage:          STTLanguage(),
		HasSTTAPIKey:         hasKey,
		STTAPIKeySource:      keySrc,
		HasGoogleCredentials: hasGoogle,
		GoogleSource:         googleSrc,
	}
}

// AgentConfig is the resolved, decrypted config the Python agent fetches at
// room-join. This is the ONLY surface that returns secret material, and it is
// gated by the internal-service secret (server-to-server only).
type AgentConfig struct {
	Mode              string `json:"mode"`
	STTProvider       string `json:"stt_provider"`
	STTModel          string `json:"stt_model,omitempty"`
	STTBaseURL        string `json:"stt_base_url,omitempty"`
	STTLanguage       string `json:"stt_language,omitempty"`
	STTAPIKey         string `json:"stt_api_key,omitempty"`
	GoogleCredentials string `json:"google_credentials,omitempty"`
}

// GetAgentConfig returns the fully-resolved config (with decrypted secrets)
// for the transcription agent. Server-to-server only.
func GetAgentConfig() AgentConfig {
	provider := STTProvider()
	cfg := AgentConfig{
		Mode:              Mode(),
		STTProvider:       provider,
		STTModel:          STTModel(),
		STTBaseURL:        EffectiveSTTBaseURL(),
		STTLanguage:       STTLanguage(),
		STTAPIKey:         STTAPIKey(),
		GoogleCredentials: GoogleCredentials(),
	}
	// The bundled server speaks the OpenAI audio API, so the agent drives it
	// with the same plugin. Translated HERE rather than taught to the agent:
	// "local" is a deployment choice, the agent only needs a plugin kind and an
	// endpoint, and keeping that knowledge on one side means adding a bundled
	// provider never requires shipping a new agent image.
	if provider == STTLocal {
		cfg.STTProvider = STTOpenAI
		cfg.STTAPIKey = LocalSTTAPIKey()
	}
	return cfg
}

// SaveInput carries partial admin updates. Nil fields are left unchanged;
// secret fields follow omit=keep / ""=clear / value=set semantics.
type SaveInput struct {
	Mode              *string
	STTProvider       *string
	STTModel          *string
	STTBaseURL        *string
	STTLanguage       *string
	STTAPIKey         *string
	GoogleCredentials *string
}

// Save persists admin-entered transcription settings (encrypting secrets) and
// busts the cache so the next read (FE client-config, agent, admin view) sees
// the new values.
func Save(in SaveInput) error {
	if in.Mode != nil {
		if err := configModel.UpsertConfig(keyMode, normalizeMode(*in.Mode)); err != nil {
			return err
		}
	}
	if in.STTProvider != nil {
		if err := configModel.UpsertConfig(keySTTProvider, normalizeSTT(*in.STTProvider)); err != nil {
			return err
		}
	}
	if in.STTModel != nil {
		if err := configModel.UpsertConfig(keySTTModel, strings.TrimSpace(*in.STTModel)); err != nil {
			return err
		}
	}
	if in.STTBaseURL != nil {
		if err := configModel.UpsertConfig(keySTTBaseURL, strings.TrimSpace(*in.STTBaseURL)); err != nil {
			return err
		}
	}
	if in.STTLanguage != nil {
		if err := configModel.UpsertConfig(keySTTLanguage, strings.TrimSpace(*in.STTLanguage)); err != nil {
			return err
		}
	}
	if in.STTAPIKey != nil {
		enc, err := helpers.EncryptSecret(strings.TrimSpace(*in.STTAPIKey))
		if err != nil {
			return err
		}
		if err := configModel.UpsertConfig(keySTTAPIKey, enc); err != nil {
			return err
		}
	}
	if in.GoogleCredentials != nil {
		enc, err := helpers.EncryptSecret(strings.TrimSpace(*in.GoogleCredentials))
		if err != nil {
			return err
		}
		if err := configModel.UpsertConfig(keyGoogleCredentials, enc); err != nil {
			return err
		}
	}
	invalidate()
	return nil
}
