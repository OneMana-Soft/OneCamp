package ai

import "testing"

func TestWebSearchConfigValid(t *testing.T) {
	cases := []struct {
		name string
		c    webSearchConfig
		want bool
	}{
		{"disabled", webSearchConfig{provider: "searxng", baseURL: "http://x", enabled: false}, false},
		{"searxng ok", webSearchConfig{provider: "searxng", baseURL: "http://x", enabled: true}, true},
		{"searxng no base", webSearchConfig{provider: "searxng", enabled: true}, false},
		{"tavily ok", webSearchConfig{provider: "tavily", apiKey: "k", enabled: true}, true},
		{"tavily no key", webSearchConfig{provider: "tavily", enabled: true}, false},
		{"brave ok", webSearchConfig{provider: "brave", apiKey: "k", enabled: true}, true},
		{"unknown", webSearchConfig{provider: "duck", apiKey: "k", enabled: true}, false},
		{"empty", webSearchConfig{enabled: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := webSearchConfigValid(c.c); got != c.want {
				t.Fatalf("webSearchConfigValid(%+v) = %v, want %v", c.c, got, c.want)
			}
		})
	}
}

func TestParseSearxng(t *testing.T) {
	body := []byte(`{"results":[
		{"title":"A","url":"https://a.com","content":"alpha   snippet"},
		{"title":"B","url":"","content":"no url, dropped"},
		{"title":"C","url":"https://c.com","content":"gamma"}
	]}`)
	out, err := parseSearxng(body, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 results (url-less dropped), got %d", len(out))
	}
	if out[0].Title != "A" || out[0].URL != "https://a.com" || out[0].Snippet != "alpha snippet" {
		t.Fatalf("unexpected first result: %+v", out[0])
	}
}

func TestParseSearxngRespectsMax(t *testing.T) {
	body := []byte(`{"results":[
		{"title":"1","url":"https://1.com"},
		{"title":"2","url":"https://2.com"},
		{"title":"3","url":"https://3.com"}
	]}`)
	out, _ := parseSearxng(body, 2)
	if len(out) != 2 {
		t.Fatalf("expected cap of 2, got %d", len(out))
	}
}

func TestParseTavily(t *testing.T) {
	body := []byte(`{"results":[{"title":"T","url":"https://t.com","content":"tav"}]}`)
	out, err := parseTavily(body, 5)
	if err != nil || len(out) != 1 || out[0].URL != "https://t.com" {
		t.Fatalf("unexpected tavily parse: %+v err=%v", out, err)
	}
}

func TestParseBrave(t *testing.T) {
	body := []byte(`{"web":{"results":[{"title":"Br","url":"https://b.com","description":"brave desc"}]}}`)
	out, err := parseBrave(body, 5)
	if err != nil || len(out) != 1 || out[0].Snippet != "brave desc" {
		t.Fatalf("unexpected brave parse: %+v err=%v", out, err)
	}
}

func TestParseInvalidJSON(t *testing.T) {
	if _, err := parseSearxng([]byte("not json"), 5); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestClampSnippet(t *testing.T) {
	long := make([]byte, webSearchMaxSnippetLen+50)
	for i := range long {
		long[i] = 'x'
	}
	if got := clampSnippet(string(long)); len(got) != webSearchMaxSnippetLen {
		t.Fatalf("expected clamp to %d, got %d", webSearchMaxSnippetLen, len(got))
	}
	if got := clampSnippet("  a\n\tb   c  "); got != "a b c" {
		t.Fatalf("whitespace collapse failed: %q", got)
	}
}

func TestMentionsWebSearchIntent(t *testing.T) {
	yes := []string{
		"what's the latest version of node?",
		"search the web for go releases",
		"can you google the current exchange rate",
		"any recent news on the launch",
		"what's the weather today",
		"look this up online",
	}
	for _, q := range yes {
		if !mentionsWebSearchIntent(q) {
			t.Errorf("expected web-search intent for %q", q)
		}
	}
	no := []string{
		"summarize my open tasks",
		"what did we decide in the design channel",
		"hello there",
		"create a task for the login bug",
	}
	for _, q := range no {
		if mentionsWebSearchIntent(q) {
			t.Errorf("did not expect web-search intent for %q", q)
		}
	}
}

// With web search disabled, a summary-intent question that also implies
// current/external info must NOT pull in the tool catalog on the web-search
// path (it would otherwise offer a tool that can't run).
func TestShouldIncludeToolsWebSearchGatedByEnablement(t *testing.T) {
	if WebSearchEnabled() {
		t.Skip("web search unexpectedly enabled in test env")
	}
	// "catch me up" is a summary intent; "latest news" is web intent. With web
	// search off and no workspace entity named, tools stay excluded.
	if ShouldIncludeTools("catch me up on the latest news") {
		t.Fatal("web-search intent should not include tools when web search is disabled")
	}
}

func TestWebSearchCacheArgs(t *testing.T) {
	// Deterministic + normalized (case/whitespace-insensitive query).
	a := webSearchCacheArgs("tavily", "  Latest Go Release  ", 5)
	b := webSearchCacheArgs("tavily", "latest go release", 5)
	if len(a) != 2 || a[0] != "tavily" {
		t.Fatalf("unexpected key shape: %+v", a)
	}
	if a[1] != b[1] {
		t.Fatalf("normalization mismatch: %q vs %q", a[1], b[1])
	}
	// Different provider, query, or max must produce different hashes/keys.
	if a[1] == webSearchCacheArgs("tavily", "latest go release", 8)[1] {
		t.Fatal("different max should change the key")
	}
	if a[0] == webSearchCacheArgs("brave", "latest go release", 5)[0] {
		t.Fatal("different provider should change the provider segment")
	}
	if a[1] == webSearchCacheArgs("tavily", "different query", 5)[1] {
		t.Fatal("different query should change the key")
	}
}
