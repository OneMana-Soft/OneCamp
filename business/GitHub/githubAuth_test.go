package business

// Tests for the OAuth2 TokenSource cache. These don't need a database
// or network — they exercise the in-memory cache and source-rebuild
// logic only. Anything that hits Postgres or the GitHub API lives
// under tests/integration with the //go:build integration tag.

import (
	"sync"
	"testing"

	integrationModel "github.com/akashc777/OneCamp/models/postgres/Integration"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// resetSourceCache wipes the package-level cache between tests so they
// stay independent. Tests that don't reset would see leakage from
// earlier tests' calls to getOrBuildTokenSource.
func resetSourceCache(t *testing.T) {
	t.Helper()
	sourceCacheMu.Lock()
	defer sourceCacheMu.Unlock()
	sourceCache = nil
}

// TestTokenSourceCache_ReusedAcrossCalls verifies that calling
// getOrBuildTokenSource twice with the same access token returns the
// same TokenSource instance — important so concurrent callers share
// the oauth2.ReuseTokenSource's internal serialisation of refreshes.
func TestTokenSourceCache_ReusedAcrossCalls(t *testing.T) {
	resetSourceCache(t)

	access := "tok-1"
	refresh := "ref-1"
	integration := &integrationModel.Integration{
		AccessToken:  &access,
		RefreshToken: &refresh,
	}

	src1 := getOrBuildTokenSource(t.Context(), integration)
	src2 := getOrBuildTokenSource(t.Context(), integration)

	if src1 != src2 {
		t.Fatalf("expected reused TokenSource on second call; got distinct instances")
	}
}

// TestTokenSourceCache_RebuiltOnTokenChange verifies that after a
// re-connect (a brand new access token in the row) the cache returns
// a freshly-built source. This is what invalidateGitHubTokenSourceCache
// supports — but the cache also self-corrects on token-string change
// alone, which is the safety net.
func TestTokenSourceCache_RebuiltOnTokenChange(t *testing.T) {
	resetSourceCache(t)

	first := "tok-A"
	refresh := "ref-A"
	integration1 := &integrationModel.Integration{
		AccessToken:  &first,
		RefreshToken: &refresh,
	}
	src1 := getOrBuildTokenSource(t.Context(), integration1)

	second := "tok-B"
	integration2 := &integrationModel.Integration{
		AccessToken:  &second,
		RefreshToken: &refresh,
	}
	src2 := getOrBuildTokenSource(t.Context(), integration2)

	if src1 == src2 {
		t.Fatalf("expected a new TokenSource after the access token changed")
	}
}

// TestTokenSourceCache_StaticWhenNoRefreshToken covers legacy
// non-rotating GitHub OAuth Apps where there's no refresh_token. We
// still want a working source — just one that doesn't try to refresh.
// oauth2.StaticTokenSource is the right shape.
func TestTokenSourceCache_StaticWhenNoRefreshToken(t *testing.T) {
	resetSourceCache(t)

	access := "static-only"
	integration := &integrationModel.Integration{
		AccessToken: &access,
		// no RefreshToken
	}

	src := getOrBuildTokenSource(t.Context(), integration)

	tok, err := src.Token()
	if err != nil {
		t.Fatalf("Token() unexpectedly errored: %v", err)
	}
	if tok.AccessToken != access {
		t.Fatalf("static source returned wrong token: got %q want %q", tok.AccessToken, access)
	}
	// Calling Token() repeatedly must not error — that's the static
	// guarantee. ReuseTokenSource without a base would, by contrast,
	// try to refresh and fail on the empty refresh string.
	for i := 0; i < 3; i++ {
		if _, err := src.Token(); err != nil {
			t.Fatalf("static source errored on call %d: %v", i+1, err)
		}
	}
}

// TestTokenSourceCache_ConcurrentBuildIsSafe runs many goroutines
// against an empty cache to exercise the double-checked locking inside
// getOrBuildTokenSource. They should all see the same TokenSource at
// the end, with no race detector complaints.
func TestTokenSourceCache_ConcurrentBuildIsSafe(t *testing.T) {
	resetSourceCache(t)

	access := "concurrent-tok"
	refresh := "concurrent-ref"
	integration := &integrationModel.Integration{
		AccessToken:  &access,
		RefreshToken: &refresh,
	}

	const N = 32
	var wg sync.WaitGroup
	results := make([]oauth2.TokenSource, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			results[i] = getOrBuildTokenSource(t.Context(), integration)
		}()
	}
	wg.Wait()

	// All goroutines should converge on the same TokenSource instance.
	for i := 1; i < N; i++ {
		if results[i] != results[0] {
			t.Fatalf("concurrent calls produced distinct TokenSources at index %d", i)
		}
	}
}

// TestInvalidateClearsCache documents the contract that a re-connect
// or disconnect call clears the cached source so the next caller
// rebuilds against the freshly-saved row.
func TestInvalidateClearsCache(t *testing.T) {
	resetSourceCache(t)

	access := "tok-pre-invalidate"
	refresh := "ref"
	integration := &integrationModel.Integration{
		AccessToken:  &access,
		RefreshToken: &refresh,
	}

	src1 := getOrBuildTokenSource(t.Context(), integration)
	invalidateGitHubTokenSourceCache()
	src2 := getOrBuildTokenSource(t.Context(), integration)

	if src1 == src2 {
		t.Fatalf("expected new TokenSource after invalidate; got reused instance")
	}
}

// Ensure uuid is imported (kept at the bottom so the linter doesn't
// trip if all tests above happen not to use it directly).
var _ = uuid.Nil
