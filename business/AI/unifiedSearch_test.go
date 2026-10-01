package business

import (
	"strings"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func TestClipUnified(t *testing.T) {
	if got := clipUnified("  hello   world \n"); got != "hello world" {
		t.Fatalf("expected normalized whitespace, got %q", got)
	}
	long := strings.Repeat("a", unifiedSnippetLen+50)
	got := clipUnified(long)
	if len([]rune(got)) != unifiedSnippetLen+1 { // +1 for the ellipsis rune
		t.Fatalf("expected clip to %d runes + ellipsis, got %d", unifiedSnippetLen, len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatal("expected ellipsis suffix")
	}
}

func TestGithubItemsToHits_MapDedupeCap(t *testing.T) {
	items := []connectorBusiness.GitHubItem{
		{Title: "Fix billing bug", Repo: "acme/api", Number: 12, State: "open", URL: "https://gh/1"},
		{Title: "Add billing report", Repo: "acme/web", Number: 7, State: "open", URL: "https://gh/2"},
		{Title: "dup", Repo: "acme/api", Number: 12, State: "open", URL: "https://gh/1"}, // dup URL
	}
	hits := githubItemsToHits(items, 10)
	if len(hits) != 2 {
		t.Fatalf("expected 2 after dedupe, got %d", len(hits))
	}
	if hits[0].Source != UnifiedSourceGitHub || hits[0].URL != "https://gh/1" {
		t.Fatalf("unexpected first hit: %+v", hits[0])
	}
	if !strings.Contains(hits[0].Meta, "acme/api #12") || !strings.Contains(hits[0].Meta, "open") {
		t.Fatalf("meta should carry repo/number/state, got %q", hits[0].Meta)
	}

	// Cap is enforced.
	capped := githubItemsToHits(items, 1)
	if len(capped) != 1 {
		t.Fatalf("expected cap of 1, got %d", len(capped))
	}
}

func TestGmailHits_TitleAndURL(t *testing.T) {
	emails := []connectorBusiness.EmailSummary{
		{ID: "abc", From: "alice@x.com", Subject: "Q3 plan", Snippet: "let's review"},
		{ID: "def", From: "bob@x.com", Subject: "", Snippet: "no subject here"},
	}
	hits := gmailHits(emails)
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].Title != "Q3 plan" || hits[0].URL != "https://mail.google.com/mail/u/0/#all/abc" {
		t.Fatalf("unexpected first hit: %+v", hits[0])
	}
	if hits[0].Meta != "from alice@x.com" {
		t.Fatalf("meta=%q", hits[0].Meta)
	}
	if hits[1].Title != "(no subject)" {
		t.Fatalf("empty subject should fall back, got %q", hits[1].Title)
	}
}

func TestWorkspaceHits_TitleMetaAndRouting(t *testing.T) {
	results := []ai.SimilarResult{
		{ContentType: "post", ContentText: "<p>ship the billing page</p>", AuthorName: "alice", ChannelName: "product", PostUUID: "p1", ChannelUUID: "c1"},
	}
	hits := workspaceHits(results)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	h := hits[0]
	if h.Title != "Post by alice" {
		t.Fatalf("title=%q", h.Title)
	}
	if h.Meta != "#product" {
		t.Fatalf("meta=%q", h.Meta)
	}
	if strings.Contains(h.Snippet, "<p>") {
		t.Fatalf("snippet HTML not stripped: %q", h.Snippet)
	}
	if h.PostUUID != "p1" || h.ChannelUUID != "c1" || h.ContentType != "post" {
		t.Fatalf("routing fields not preserved: %+v", h)
	}
}

func TestUnifiedSearchCacheArgs(t *testing.T) {
	a := unifiedSearchCacheArgs("user-1", "  Hello World ")
	b := unifiedSearchCacheArgs("user-1", "hello world")
	if len(a) != 2 || a[0] != "user-1" {
		t.Fatalf("unexpected args: %v", a)
	}
	// Normalization: case + surrounding whitespace must not change the key.
	if a[1] != b[1] {
		t.Fatalf("normalized queries should share a hash: %q vs %q", a[1], b[1])
	}
	// Different user → different namespace arg; different query → different hash.
	if c := unifiedSearchCacheArgs("user-2", "hello world"); c[0] == a[0] {
		t.Fatal("different users must not share the user arg")
	}
	if d := unifiedSearchCacheArgs("user-1", "different"); d[1] == a[1] {
		t.Fatal("different queries must not share the hash")
	}
}

func TestQueryTokensAndMatch(t *testing.T) {
	toks := queryTokens("  Launch DATE, q3!! ")
	// lowercased, punctuation-trimmed, deduped, drops <2-char tokens
	if strings.Join(toks, ",") != "launch,date,q3" {
		t.Fatalf("tokens=%v", toks)
	}
	if matchesAllTokens("we set the launch date for q3", nil) {
		t.Fatal("empty token set must not match")
	}
	if !matchesAllTokens("We set the LAUNCH date for Q3", toks) {
		t.Fatal("should match all tokens case-insensitively")
	}
	if matchesAllTokens("launch is on", toks) {
		t.Fatal("should require ALL tokens (missing date/q3)")
	}
}

func TestMemoryHits_FilterMapCap(t *testing.T) {
	items := []adapter.MemoryItemView{
		{Content: "Ship billing on Friday", Kind: "commitment", DueAt: "2026-07-03", ScopeType: "channel", ScopeLabel: "product", ChannelUUID: "ch-1"},
		{Content: "Use Postgres for the store", Kind: "decision", ScopeLabel: "Alice, Bob", ScopeType: "group"},
		{Content: "", Kind: "decision"}, // empty content skipped
	}
	hits := memoryHits(items, "billing", 10)
	if len(hits) != 1 {
		t.Fatalf("expected 1 billing match, got %d", len(hits))
	}
	h := hits[0]
	if h.Source != UnifiedSourceMemory || h.Kind != "commitment" {
		t.Fatalf("unexpected hit: %+v", h)
	}
	if !strings.Contains(h.Meta, "#product") || !strings.Contains(h.Meta, "due 2026-07-03") {
		t.Fatalf("meta should carry scope + due, got %q", h.Meta)
	}
	if h.ChannelUUID != "ch-1" {
		t.Fatalf("channel routing field should be carried, got %q", h.ChannelUUID)
	}

	// Cap is enforced (both items match "the").
	capped := memoryHits([]adapter.MemoryItemView{
		{Content: "the alpha", Kind: "decision"},
		{Content: "the beta", Kind: "decision"},
	}, "the", 1)
	if len(capped) != 1 {
		t.Fatalf("expected cap of 1, got %d", len(capped))
	}
}
