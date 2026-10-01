package business

// Connectivity / credential test for the configured STT provider.
//
// The actual transcription runs in the Python agent, but the most common
// misconfigurations are credential/endpoint problems that the BE can verify
// directly with a cheap, bounded probe:
//   - deepgram → GET https://api.deepgram.com/v1/projects  (validates the key)
//   - openai   → GET {base_url|api.openai.com/v1}/models   (validates key + endpoint)
//   - google   → structural validation of the service-account JSON
//
// The probe tests the CURRENTLY SAVED config (decrypted server-side); the admin
// saves, then clicks Test — mirroring how the app/AI-provider tests work. No
// secret is ever returned to the FE; only a pass/fail + a safe message.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// TestResult is the admin-facing outcome of a probe. OK=false is a normal
// "your config is wrong" result (HTTP 200 with OK:false), not a server error.
type TestResult struct {
	OK       bool   `json:"ok"`
	Provider string `json:"provider"`
	Message  string `json:"message"`
}

// testHTTPClient is a bounded client shared by the probes.
var testHTTPClient = &http.Client{Timeout: 10 * time.Second}

// TestConfig probes the saved STT configuration. Never returns an error for a
// bad credential/endpoint — those surface as OK:false so the FE shows a precise
// message. Returns an error only for an internal failure (e.g. building the
// request), which the controller maps to 500.
func TestConfig(ctx context.Context) (*TestResult, error) {
	mode := Mode()
	if mode != ModeBackend {
		// Frontend / off mode has no server-side STT to test.
		return &TestResult{
			OK:       true,
			Provider: STTProvider(),
			Message: fmt.Sprintf(
				"Transcription mode is %q — there's no server-side STT to test. Switch to backend mode to test a provider.",
				mode),
		}, nil
	}

	provider := STTProvider()
	switch provider {
	case STTDeepgram:
		return testDeepgram(ctx, provider), nil
	case STTOpenAI:
		return testOpenAICompatible(ctx, provider), nil
	case STTLocal:
		return testLocalWhisper(ctx, provider), nil
	case STTGoogle:
		return testGoogle(provider), nil
	default:
		return &TestResult{OK: false, Provider: provider, Message: "Unknown STT provider."}, nil
	}
}

// testLocalWhisper probes the bundled Whisper server.
//
// Deliberately does NOT run the SSRF guard, and that is safe here for a reason
// worth stating: the OpenAI-compatible probe validates a URL an ADMIN typed,
// so it must assume the admin could be tricked into pointing it at something
// internal. This URL is a server-side constant. There is no user input to
// abuse, and running the guard would only reject the one endpoint this
// provider is allowed to reach, since the guard requires HTTPS and a public
// host and the bundled server is plain HTTP on the private stack network.
//
// The failure worth reporting precisely is "the container is not running",
// because that is what an admin will actually hit: they pick the bundled
// server before starting it.
func testLocalWhisper(ctx context.Context, provider string) *TestResult {
	base := LocalSTTBaseURL()
	url := strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1") + "/health"

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return &TestResult{OK: false, Provider: provider, Message: probeError(err)}
	}
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		return &TestResult{
			OK:       false,
			Provider: provider,
			Message:  "Could not reach the bundled transcription server at " + base + ". Start it with `make stt_up`, then test again.",
		}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		msg := "The bundled transcription server is running. Meeting audio stays on this machine."
		if LocalSTTAPIKey() == "" {
			// It answers /health without a token and refuses everything else
			// without one, so a running server with no secret transcribes
			// nothing. Worth saying here rather than at the first silent call.
			msg = "The bundled transcription server is running, but INTERNAL_SECRET is not set, so it will refuse every transcription request. Set it and restart."
			return &TestResult{OK: false, Provider: provider, Message: msg}
		}
		return &TestResult{OK: true, Provider: provider, Message: msg}
	}
	return &TestResult{
		OK:       false,
		Provider: provider,
		Message:  fmt.Sprintf("The bundled transcription server answered %d. Check `make stt_logs`.", resp.StatusCode),
	}
}

// testDeepgram validates the Deepgram key by listing projects (cheap, auth-gated).
func testDeepgram(ctx context.Context, provider string) *TestResult {
	key := STTAPIKey()
	if key == "" {
		return &TestResult{OK: false, Provider: provider, Message: "No Deepgram API key configured."}
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, "https://api.deepgram.com/v1/projects", nil)
	if err != nil {
		return &TestResult{OK: false, Provider: provider, Message: probeError(err)}
	}
	req.Header.Set("Authorization", "Token "+key)

	resp, err := testHTTPClient.Do(req)
	if err != nil {
		return &TestResult{OK: false, Provider: provider, Message: probeError(err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusOK:
		return &TestResult{OK: true, Provider: provider, Message: "Deepgram key is valid."}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &TestResult{OK: false, Provider: provider, Message: "Deepgram rejected the API key (unauthorized)."}
	default:
		return &TestResult{OK: false, Provider: provider, Message: fmt.Sprintf("Deepgram returned HTTP %d.", resp.StatusCode)}
	}
}

// testOpenAICompatible validates the key + endpoint by listing models. Works
// for OpenAI, Groq, and most self-hosted Whisper servers that expose /models.
func testOpenAICompatible(ctx context.Context, provider string) *TestResult {
	key := STTAPIKey()
	base := strings.TrimSpace(STTBaseURL())
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	// SSRF guard for custom endpoints (self-hosted). Public hosts pass through.
	if _, err := helpers.ValidateOutboundURL(base, false); err != nil {
		return &TestResult{OK: false, Provider: provider, Message: "Endpoint blocked: " + err.Error()}
	}
	if key == "" {
		// Some self-hosted endpoints are keyless; still probe reachability.
		key = ""
	}

	url := strings.TrimRight(base, "/") + "/models"
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return &TestResult{OK: false, Provider: provider, Message: probeError(err)}
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := testHTTPClient.Do(req)
	if err != nil {
		return &TestResult{OK: false, Provider: provider, Message: probeError(err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusOK:
		return &TestResult{OK: true, Provider: provider, Message: fmt.Sprintf("Endpoint reachable and key accepted (%s).", base)}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &TestResult{OK: false, Provider: provider, Message: "Endpoint rejected the API key (unauthorized)."}
	case resp.StatusCode == http.StatusNotFound:
		// /models not implemented by this server — reachable but can't fully verify.
		return &TestResult{OK: true, Provider: provider, Message: fmt.Sprintf("Endpoint reachable (%s); it doesn't expose /models, so the key couldn't be verified.", base)}
	default:
		return &TestResult{OK: false, Provider: provider, Message: fmt.Sprintf("Endpoint returned HTTP %d.", resp.StatusCode)}
	}
}

// testGoogle structurally validates the service-account JSON. We can't mint a
// token without the Google client lib, but a malformed/empty credential is the
// overwhelmingly common failure, and that we can catch precisely.
func testGoogle(provider string) *TestResult {
	creds := GoogleCredentials()
	if creds == "" {
		// Fall back to the env file path; if that's set, we trust the agent.
		return &TestResult{
			OK:       true,
			Provider: provider,
			Message:  "No inline credentials saved; the agent will use GOOGLE_APPLICATION_CREDENTIALS from the environment.",
		}
	}
	var sa struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		ProjectID   string `json:"project_id"`
	}
	if err := json.Unmarshal([]byte(creds), &sa); err != nil {
		return &TestResult{OK: false, Provider: provider, Message: "Credentials are not valid JSON."}
	}
	if sa.Type != "service_account" {
		return &TestResult{OK: false, Provider: provider, Message: `Credentials JSON is missing "type": "service_account".`}
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return &TestResult{OK: false, Provider: provider, Message: "Credentials JSON is missing client_email or private_key."}
	}
	return &TestResult{OK: true, Provider: provider, Message: fmt.Sprintf("Service-account JSON looks valid (%s).", sa.ClientEmail)}
}

// probeError maps low-level transport errors to safe, admin-friendly messages.
func probeError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "blocked for security"):
		return "Connection blocked: this address is not allowed."
	case strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "Timeout"):
		return "Connection timed out. Check the endpoint is reachable from the server."
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "x509"):
		return "TLS certificate verification failed."
	case strings.Contains(msg, "connection refused"):
		return "Connection refused. Is the endpoint running and reachable?"
	case strings.Contains(msg, "no such host"):
		return "Host not found. Check the endpoint hostname."
	default:
		return msg
	}
}
