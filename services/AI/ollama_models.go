package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

// Model management for the Ollama provider: list installed models, pull
// (download) a new model with streaming progress, delete a model, and
// report the server version.
//
// These back the admin "install / select / delete model" UX. Because
// new models are released constantly, nothing is hardcoded — the catalog
// of installed models is always read live from the Ollama server.

// --- API types ---

type ollamaTagsResponse struct {
	Models []struct {
		Name       string    `json:"name"`
		Model      string    `json:"model"`
		Size       int64     `json:"size"`
		ModifiedAt time.Time `json:"modified_at"`
		Details    struct {
			Family string `json:"family"`
		} `json:"details"`
	} `json:"models"`
}

type ollamaPullRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

type ollamaPullProgressLine struct {
	Status    string `json:"status"`
	Total     int64  `json:"total"`
	Completed int64  `json:"completed"`
	Error     string `json:"error"`
}

type ollamaDeleteRequest struct {
	Model string `json:"model"`
}

type ollamaVersionResponse struct {
	Version string `json:"version"`
}

// embeddingModelHint matches common embedding-model naming so the FE can
// separate chat models from embedding models in the picker. Best effort.
func embeddingModelHint(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "embed") ||
		strings.Contains(n, "bge") ||
		strings.Contains(n, "minilm") ||
		strings.Contains(n, "gte")
}

// ErrOllamaUpdateRequired is returned when a pull fails because the
// running Ollama server is too old for the requested model's
// architecture. It is actionable: the admin must update the Ollama
// container (the app deliberately does NOT auto-update infrastructure).
var ErrOllamaUpdateRequired = errors.New("ollama: server update required for this model")

// isUpdateRequiredError reports whether an Ollama error message indicates
// the server is too old for the model. Ollama phrases this a few ways
// across versions; match the stable substrings.
func isUpdateRequiredError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "requires newer version of ollama") ||
		strings.Contains(m, "newer version of ollama") ||
		strings.Contains(m, "update ollama") ||
		(strings.Contains(m, "unsupported") && strings.Contains(m, "architecture"))
}

// ListModels returns the models installed on the Ollama server.
func (o *OllamaProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.host+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("ollama: build tags request: %w", err)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: tags request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama: tags returned %d: %s", resp.StatusCode, string(body))
	}

	var tags ollamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, fmt.Errorf("ollama: decode tags: %w", err)
	}

	out := make([]ModelInfo, 0, len(tags.Models))
	for _, m := range tags.Models {
		name := m.Name
		if name == "" {
			name = m.Model
		}
		out = append(out, ModelInfo{
			ID:        name,
			Installed: true,
			SizeBytes: m.Size,
			Embedding: embeddingModelHint(name),
		})
	}
	return out, nil
}

// PullModel downloads a model, streaming progress. The Ollama /api/pull
// endpoint emits newline-delimited JSON progress objects.
func (o *OllamaProvider) PullModel(ctx context.Context, model string) (<-chan PullProgress, error) {
	body, err := json.Marshal(ollamaPullRequest{Model: model, Stream: true})
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal pull request: %w", err)
	}

	// No client timeout: a large pull can take many minutes. Cancellation
	// is driven by ctx (the caller sets a deadline appropriate to a
	// download, and the SSE handler cancels on client disconnect).
	streamClient := &http.Client{Timeout: 0}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.host+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: build pull request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: pull request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		msg := string(b)
		if isUpdateRequiredError(msg) {
			return nil, fmt.Errorf("%w: %s", ErrOllamaUpdateRequired, msg)
		}
		return nil, fmt.Errorf("ollama: pull returned %d: %s", resp.StatusCode, msg)
	}

	ch := make(chan PullProgress, 64)

	go func() {
		defer close(ch)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				ch <- PullProgress{Done: true, Error: ctx.Err().Error()}
				return
			default:
			}

			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			var p ollamaPullProgressLine
			if err := json.Unmarshal(line, &p); err != nil {
				helpers.LogErrorWithContext(ctx, "ollama: parse pull progress: %v", err)
				continue
			}

			if p.Error != "" {
				ch <- PullProgress{
					Done:           true,
					Error:          p.Error,
					UpdateRequired: isUpdateRequiredError(p.Error),
				}
				return
			}

			ch <- PullProgress{
				Status:    p.Status,
				Total:     p.Total,
				Completed: p.Completed,
			}
		}

		if err := scanner.Err(); err != nil {
			ch <- PullProgress{Done: true, Error: fmt.Sprintf("stream read error: %v", err)}
			return
		}

		// Ollama signals completion with a final {"status":"success"} line.
		ch <- PullProgress{Status: "success", Done: true}
	}()

	return ch, nil
}

// DeleteModel removes a locally-installed model from the Ollama server.
func (o *OllamaProvider) DeleteModel(ctx context.Context, model string) error {
	body, err := json.Marshal(ollamaDeleteRequest{Model: model})
	if err != nil {
		return fmt.Errorf("ollama: marshal delete request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, o.host+"/api/delete", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ollama: build delete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: delete request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama: delete returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// Version returns the Ollama server version string.
func (o *OllamaProvider) Version(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.host+"/api/version", nil)
	if err != nil {
		return "", fmt.Errorf("ollama: build version request: %w", err)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama: version request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama: version returned %d", resp.StatusCode)
	}

	var v ollamaVersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", fmt.Errorf("ollama: decode version: %w", err)
	}
	return v.Version, nil
}

// --- Ollama runtime version awareness ---
//
// OneCamp does NOT auto-update the Ollama container — that's an
// infrastructure operation the operator owns (image pull + container
// recreate, possible GPU/CUDA implications, a brief inference outage).
// Instead we surface enough information for the admin to make the call:
// the running version, the latest released version, and whether an
// update is available. The admin panel renders an "update available"
// badge plus the one-line command.

type ollamaLatestRelease struct {
	TagName string `json:"tag_name"`
}

// LatestOllamaVersion returns the latest published Ollama release tag
// (without the leading "v"), best-effort. Network failures return an
// empty string and a nil error so callers can treat "unknown" gracefully
// rather than failing the whole status response.
func LatestOllamaVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/ollama/ollama/releases/latest", nil)
	if err != nil {
		return "", nil
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Offline / air-gapped deploy — not an error worth surfacing.
		return "", nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil
	}

	var rel ollamaLatestRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", nil
	}
	return strings.TrimPrefix(rel.TagName, "v"), nil
}

// CompareVersions returns -1, 0, or +1 comparing dotted numeric version
// strings (e.g. "0.3.14" vs "0.4.0"). Non-numeric / missing segments are
// treated as 0. Used to decide whether an Ollama update is available.
func CompareVersions(a, b string) int {
	as := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bs := strings.Split(strings.TrimPrefix(b, "v"), ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var ai, bi int
		if i < len(as) {
			ai = atoiSafe(as[i])
		}
		if i < len(bs) {
			bi = atoiSafe(bs[i])
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}

func atoiSafe(s string) int {
	// Strip any pre-release suffix like "0.4.0-rc1" → "0".
	num := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		num = num*10 + int(r-'0')
	}
	return num
}
