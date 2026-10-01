package business

// Prerecorded (clip) transcription for voice input — the REST counterpart to
// the realtime LiveKit call transcription. It reuses the SAME admin-managed,
// model-agnostic STT config (provider / model / base_url / language / key), so
// voice dictation runs on whatever speech engine the workspace already trusts —
// including a self-hosted Whisper / faster-whisper / Groq via the OpenAI-
// compatible base_url, which keeps it consistent with the residency posture.
//
// Generic dispatch by provider KIND (mirrors the Python agent's _build_stt):
//   - openai : POST {base_url|api.openai.com}/audio/transcriptions (multipart).
//              Also covers ANY OpenAI-compatible STT (self-hosted Whisper, Groq).
//   - deepgram: POST api.deepgram.com/v1/listen (prerecorded, raw body).
//   - google  : not wired for REST clip transcription yet (needs SA-JWT signing);
//               DictationAvailable() reports false so no dangling affordance.
// Adding a provider is one case here — no schema change.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// transcribeTimeout bounds the upstream STT call. Clips are short; a minute is
// ample and prevents a hung provider from holding the request.
const transcribeTimeout = 60 * time.Second

// DictationAvailable reports whether server-side clip transcription can run with
// the current admin config: an OpenAI-compatible endpoint (key OR a self-hosted
// base_url) or Deepgram (key). Lets the FE hide the mic when it would fail.
func DictationAvailable() bool {
	switch STTProvider() {
	case STTOpenAI:
		return strings.TrimSpace(STTAPIKey()) != "" || strings.TrimSpace(STTBaseURL()) != ""
	case STTDeepgram:
		return strings.TrimSpace(STTAPIKey()) != ""
	default:
		return false
	}
}

// TranscribeAudio transcribes a recorded audio clip via the configured STT
// provider and returns the plain transcript. filename carries the extension the
// provider uses to sniff the container (e.g. "clip.webm"); contentType is the
// blob's MIME. Generic + provider-dispatched.
func TranscribeAudio(ctx context.Context, audio []byte, filename, contentType string) (string, error) {
	if len(audio) == 0 {
		return "", fmt.Errorf("no audio to transcribe")
	}
	cctx, cancel := context.WithTimeout(ctx, transcribeTimeout)
	defer cancel()

	switch STTProvider() {
	case STTOpenAI:
		return transcribeOpenAICompatible(cctx, audio, filename)
	case STTDeepgram:
		return transcribeDeepgram(cctx, audio, contentType)
	default:
		return "", fmt.Errorf("voice input isn't available for the configured speech provider")
	}
}

// transcribeOpenAICompatible calls the OpenAI /audio/transcriptions endpoint (or
// any compatible server via the admin base_url). The base_url follows the same
// convention the LiveKit openai plugin uses — it already includes the API
// version segment (e.g. https://host/v1) — so we append only /audio/transcriptions.
func transcribeOpenAICompatible(ctx context.Context, audio []byte, filename string) (string, error) {
	endpoint := "https://api.openai.com/v1/audio/transcriptions"
	if base := strings.TrimSpace(STTBaseURL()); base != "" {
		endpoint = strings.TrimRight(base, "/") + "/audio/transcriptions"
	}

	if strings.TrimSpace(filename) == "" {
		filename = "audio.webm"
	}
	model := strings.TrimSpace(STTModel())
	if model == "" {
		model = "whisper-1"
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("build transcription request")
	}
	if _, err := fw.Write(audio); err != nil {
		return "", fmt.Errorf("build transcription request")
	}
	_ = mw.WriteField("model", model)
	_ = mw.WriteField("response_format", "json")
	if lang := strings.TrimSpace(STTLanguage()); lang != "" {
		_ = mw.WriteField("language", lang)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("build transcription request")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return "", fmt.Errorf("build transcription request")
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if key := strings.TrimSpace(STTAPIKey()); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", fmt.Errorf("the speech service couldn't be reached")
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("the speech service could not transcribe this audio")
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", fmt.Errorf("the speech service returned an unexpected response")
	}
	return strings.TrimSpace(out.Text), nil
}

// transcribeDeepgram calls Deepgram's prerecorded endpoint with the raw audio
// body (Deepgram sniffs the container from the Content-Type).
func transcribeDeepgram(ctx context.Context, audio []byte, contentType string) (string, error) {
	q := url.Values{}
	q.Set("smart_format", "true")
	q.Set("punctuate", "true")
	if model := strings.TrimSpace(STTModel()); model != "" {
		q.Set("model", model)
	}
	if lang := strings.TrimSpace(STTLanguage()); lang != "" {
		q.Set("language", lang)
	}
	endpoint := "https://api.deepgram.com/v1/listen?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(audio))
	if err != nil {
		return "", fmt.Errorf("build transcription request")
	}
	if ct := strings.TrimSpace(contentType); ct != "" {
		req.Header.Set("Content-Type", ct)
	} else {
		req.Header.Set("Content-Type", "audio/webm")
	}
	req.Header.Set("Authorization", "Token "+strings.TrimSpace(STTAPIKey()))

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", fmt.Errorf("the speech service couldn't be reached")
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("the speech service could not transcribe this audio")
	}
	var out struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", fmt.Errorf("the speech service returned an unexpected response")
	}
	if len(out.Results.Channels) == 0 || len(out.Results.Channels[0].Alternatives) == 0 {
		return "", nil
	}
	return strings.TrimSpace(out.Results.Channels[0].Alternatives[0].Transcript), nil
}
