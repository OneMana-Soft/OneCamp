package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// V-157 part 3: every AI controller answers a failure with ClientError's words. Those keep the words the workspace
// wrote for people, and replace any text a provider, a remote agent or a connection wrote: on 31 Oct the demo's ask
// echoed Groq's 413, which named the account's organization id.

// secretOrg stands for what a provider's own text can hold: its account, its hosts.
const secretOrg = "org_01secretorg"

func TestClientErrorKeepsTheWordsWrittenForPeople(t *testing.T) {
	//lint:ignore SA1007 the test needs url.Parse's own error for a URL that can't be parsed
	_, parseErr := url.Parse("http://exa mple.com")
	for _, err := range []error{
		errors.New("that image is too large to analyze (limit 8 MB)"),
		fmt.Errorf("there's no text to translate"),
		// A setting's own check: url.Parse fails with a *url.Error too, but no connection was made.
		fmt.Errorf("base URL is not a valid URL: %w", parseErr),
		fmt.Errorf("couldn't check what you can do in project 'Launch': %w", errors.New("you're not a member of this project")),
	} {
		status, words := ClientError(context.Background(), err)
		if status != http.StatusInternalServerError || words != err.Error() {
			t.Errorf("ClientError(%q) = %d %q: the words written for people were replaced", err, status, words)
		}
		if got := ClientWords(context.Background(), err); got != err.Error() {
			t.Errorf("ClientWords(%q) = %q: the words written for people were replaced", err, got)
		}
	}
}

func TestNoProviderOrRemoteTextReachesAPerson(t *testing.T) {
	secretBody := `{"error":{"message":"model gpt-x is not available to ` + secretOrg + ` at https://api.example-provider.com/v1"}}`
	reply := func(status int, body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	chat := func(srv *httptest.Server) error {
		p := NewOpenAICompatibleProviderWithOpts("k", "gpt-x", "", srv.URL, 0, ProviderOpenAICompatible, ProviderOptions{})
		_, err := p.Chat(context.Background(), []ChatMessage{{Role: roleUser, Content: "hi"}}, ChatOptions{MaxTokens: 16})
		return err
	}
	// An Anthropic stream that reports an error event part-way.
	streamErr := func() error {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded for "+secretOrg+"\"}}\n\n")
		}))
		defer srv.Close()
		ch, err := NewAnthropicProviderWithOpts("k", "claude-x", srv.URL, ProviderOptions{}).ChatStream(context.Background(), []ChatMessage{{Role: roleUser, Content: "hi"}}, ChatOptions{MaxTokens: 16})
		if err != nil {
			return err
		}
		for c := range ch {
			if c.Error != nil {
				return c.Error
			}
		}
		return errors.New("the stream reported no error")
	}
	a2a := func(srv *httptest.Server) error {
		_, err := NewA2AProvider(srv.URL, "", "", "c", "0.3", adminAdded).Chat(context.Background(), []ChatMessage{{Role: roleUser, Content: "x"}}, ChatOptions{})
		return err
	}
	// cut answers 200 and stops part-way through its reply, as a dropped connection or a proxy's timeout does: the
	// reply can't be read, which is no more the workspace's words than a refusal's body is.
	cut := func(partial string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/show" {
				_, _ = io.WriteString(w, `{"capabilities":["completion"]}`)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(partial)+4096))
			_, _ = io.WriteString(w, partial)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	// streamEnd is the error a stream ends with.
	streamEnd := func(ch <-chan StreamChunk, err error) error {
		if err != nil {
			return err
		}
		for c := range ch {
			if c.Error != nil {
				return c.Error
			}
		}
		return errors.New("the stream reported no error")
	}
	ctx, msgs, opts := context.Background(), []ChatMessage{{Role: roleUser, Content: "hi"}}, ChatOptions{MaxTokens: 16}
	openai := func(srv *httptest.Server) *OpenAIProvider {
		return NewOpenAICompatibleProviderWithOpts("k", "gpt-x", "", srv.URL, 0, ProviderOpenAICompatible, ProviderOptions{})
	}
	ollama := func(srv *httptest.Server) *OllamaProvider { return NewOllamaProviderWithDims(srv.URL, "qwen-x", "", 0) }
	anthropic := func(srv *httptest.Server) *AnthropicProvider {
		return NewAnthropicProviderWithOpts("k", "claude-x", srv.URL, ProviderOptions{})
	}
	errOf := func(_ string, err error) error { return err }
	withTools := func(_ string, _ []ToolCall, err error) error { return err }
	search := []ToolSpec{{Name: "search_workspace", Description: "Search the workspace", Parameters: map[string]interface{}{"type": "object"}}}
	dial := &url.Error{Op: "Post", URL: "http://10.0.0.7:11434/api/chat", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}}

	cases := []struct {
		name string
		err  error
	}{
		{"a refusal's body, passed up as it is", chat(reply(http.StatusInternalServerError, secretBody))},
		{"a refusal's body, wrapped by a business function", fmt.Errorf("failed to summarize the channel: %w", chat(reply(http.StatusBadRequest, secretBody)))},
		{"an error inside a 200 reply", chat(reply(http.StatusOK, secretBody))},
		{"an error event in a stream", streamErr()},
		{"a remote agent's JSON-RPC error", a2a(reply(http.StatusOK, `{"jsonrpc":"2.0","id":"1","error":{"code":-32000,"message":"quota for `+secretOrg+` used up"}}`))},
		{"a remote agent's refusal", a2a(reply(http.StatusForbidden, secretBody))},
		{"a failed connection", fmt.Errorf("AI Q&A failed: %w", dial)},
		{"a failed connection, flattened on the way", fmt.Errorf("summarize: %v", dial)},
		{"a 200 reply that isn't JSON (a proxy's page)", chat(reply(http.StatusOK, "<html><body>502 Bad Gateway</body></html>"))},
		{"an OpenAI reply cut short", errOf(openai(cut(`{"choices":[{"message":{"role":"assistant","content":"The bo`)).Chat(ctx, msgs, opts))},
		{"an OpenAI tools reply cut short", withTools(openai(cut(`{"choices":[{"message":{"role":"assistant","content":"The bo`)).ChatWithTools(ctx, msgs, search, opts))},
		{"an OpenAI stream cut short", streamEnd(openai(cut("data: {\"choices\":[{\"delta\":{\"content\":\"The board\"}}]}\n\n")).ChatStream(ctx, msgs, opts))},
		{"an Ollama reply cut short", errOf(ollama(cut(`{"message":{"role":"assistant","content":"The bo`)).Chat(ctx, msgs, opts))},
		{"an Ollama stream cut short", streamEnd(ollama(cut(`{"message":{"role":"assistant","content":"The board"},"done":false}`+"\n")).ChatStream(ctx, msgs, opts))},
		{"an Anthropic reply cut short", errOf(anthropic(cut(`{"content":[{"type":"text","text":"The bo`)).Chat(ctx, msgs, opts))},
		{"an Anthropic stream cut short", streamEnd(anthropic(cut("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"The board\"}}\n\n")).ChatStream(ctx, msgs, opts))},
	}
	for _, c := range cases {
		if c.err == nil {
			t.Errorf("%s: the call did not fail", c.name)
			continue
		}
		status, words := ClientError(context.Background(), c.err)
		if status < 400 || words == "" || words == c.err.Error() || strings.Contains(words, secretOrg) ||
			strings.Contains(words, "example-provider") || strings.Contains(words, "10.0.0.7") {
			t.Errorf("%s: ClientError = %d %q: the provider's own text reached the person (%v)", c.name, status, words, c.err)
		}
		if got := ClientWords(context.Background(), c.err); got != words {
			t.Errorf("%s: ClientWords = %q, ClientError's words %q", c.name, got, words)
		}
	}
}
