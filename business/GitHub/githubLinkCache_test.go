package business

import (
	"sync/atomic"
	"testing"

	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
)

// linkCacheKey must lower-case both components so callers passing
// mixed-case repo names do not produce duplicate cache entries.
func TestLinkCacheKeyCaseInsensitive(t *testing.T) {
	a := linkCacheKey("Akashc777", "OneCamp")
	b := linkCacheKey("akashc777", "onecamp")
	if a != b {
		t.Fatalf("expected case-insensitive cache key, got %q vs %q", a, b)
	}
}

// invalidateGitHubLinkCache must bump the generation counter so any
// previously-cached entry is treated as stale on the next read.
func TestLinkCacheGenerationFencing(t *testing.T) {
	githubLinkCache.Purge()
	startGen := atomic.LoadUint64(&githubLinkGeneration)

	key := linkCacheKey("acme", "repo")
	githubLinkCache.Set(key, githubLinkCacheEntry{
		link:       &githubLinkModel.GitHubLink{RepoOwner: "acme", RepoName: "repo"},
		generation: startGen,
	})

	invalidateGitHubLinkCache()

	entry, ok := githubLinkCache.Get(key)
	if !ok {
		t.Fatalf("entry should still be present in raw cache before TTL expiry")
	}
	if entry.generation == atomic.LoadUint64(&githubLinkGeneration) {
		t.Fatalf("entry generation should differ from current after bump")
	}
}

// parseAutomationRules tolerates malformed JSON (returns nil) and
// empty input.
func TestParseAutomationRulesTolerant(t *testing.T) {
	if got := parseAutomationRules(nil); got != nil {
		t.Fatalf("nil link should yield nil rules, got %v", got)
	}
	empty := ""
	if got := parseAutomationRules(&githubLinkModel.GitHubLink{AutomationRules: &empty}); got != nil {
		t.Fatalf("empty rules should yield nil, got %v", got)
	}
	bad := "not json"
	if got := parseAutomationRules(&githubLinkModel.GitHubLink{AutomationRules: &bad}); got != nil {
		t.Fatalf("bad rules should yield nil, got %v", got)
	}
	good := `{"issue_opened":"todo"}`
	rules := parseAutomationRules(&githubLinkModel.GitHubLink{AutomationRules: &good})
	if rules["issue_opened"] != "todo" {
		t.Fatalf("expected todo status, got %v", rules)
	}
}
