package ai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenAIProviderRedactsOutboundBody proves the redactor is actually wired
// into the provider request path: with a redactor attached, the raw PII never
// appears in the bytes sent to the endpoint, and the placeholder does.
func TestOpenAIProviderRedactsOutboundBody(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProviderWithOpts("k", "gpt-4o-mini", "", srv.URL, 0, ProviderOpenAICompatible, ProviderOptions{})
	p.redactor = NewRedactor(nil)

	_, err := p.Chat(context.Background(), []ChatMessage{
		{Role: "user", Content: "reach me at jane@example.com or 415-555-0199"},
	}, ChatOptions{})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if strings.Contains(captured, "jane@example.com") {
		t.Fatalf("raw email leaked to provider body: %s", captured)
	}
	if !strings.Contains(captured, "[EMAIL_1]") {
		t.Fatalf("expected [EMAIL_1] placeholder in provider body: %s", captured)
	}
}

// TestOpenAIProviderNoRedactorSendsRaw confirms the local path is unchanged:
// with no redactor (local endpoint / redaction off), content is sent verbatim.
func TestOpenAIProviderNoRedactorSendsRaw(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProviderWithOpts("k", "gpt-4o-mini", "", srv.URL, 0, ProviderOpenAICompatible, ProviderOptions{})
	// no redactor attached

	_, err := p.Chat(context.Background(), []ChatMessage{
		{Role: "user", Content: "reach me at jane@example.com"},
	}, ChatOptions{})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !strings.Contains(captured, "jane@example.com") {
		t.Fatalf("local path must send content verbatim, got: %s", captured)
	}
}

// TestOpenAIEmbedRedactsOutboundBody proves embedding inputs are redacted too.
func TestOpenAIEmbedRedactsOutboundBody(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1,0.2]}]}`)
	}))
	defer srv.Close()

	p := NewOpenAICompatibleProviderWithOpts("k", "", "text-embedding-3-small", srv.URL, 0, ProviderOpenAI, ProviderOptions{})
	p.redactor = NewRedactor(nil)

	_, err := p.Embed(context.Background(), []string{"card 4242 4242 4242 4242"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if strings.Contains(captured, "4242 4242 4242 4242") {
		t.Fatalf("raw card leaked to embed body: %s", captured)
	}
	if !strings.Contains(captured, "[CARD_1]") {
		t.Fatalf("expected [CARD_1] placeholder in embed body: %s", captured)
	}
}
