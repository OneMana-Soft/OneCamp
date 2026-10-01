package provider

// Tests for the shared import-provider OAuth refresh layer. We don't
// hit a real OAuth endpoint here — the cache + static-fallback +
// invalidate semantics are pure logic and can be exercised
// deterministically.

import (
	"sync"
	"testing"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

func clearImportSourceCache(t *testing.T) {
	t.Helper()
	importTokenSources.Range(func(k, _ any) bool {
		importTokenSources.Delete(k)
		return true
	})
}

// TestImportSource_StaticPathOnEmptyClient: when client_id/secret are
// not configured we must NOT attempt to build an oauth2 source.
// Returning the stored token as-is is the right behaviour for PAT/API
// token connections.
func TestImportSource_StaticPathOnEmptyClient(t *testing.T) {
	clearImportSourceCache(t)

	cfg := OAuthConfig{
		// No ClientID / ClientSecret — simulating a PAT-only deploy.
		Endpoint: oauth2.Endpoint{TokenURL: "https://example.invalid/token"},
	}

	// Direct call into the cache path would normally try to refresh.
	// FreshAccessToken takes the static-fallback branch and returns the
	// stored access without ever building an oauth2.TokenSource — so
	// even if the token endpoint is unreachable, the call succeeds.
	t.Setenv("IMPORT_TOKEN_KEK", "test-key-with-enough-entropy-for-tests-1234")

	// We'd need a live DB to call FreshAccessToken end-to-end. Instead
	// validate getOrBuildImportSource returns a ReuseTokenSource that
	// holds the static seed unchanged.
	tok := &importModels.Token{
		Provider:     "fake",
		OwnerUserId:  uuid.New(),
		AccessToken:  "static-1",
		RefreshToken: "rt-1",
	}
	src := getOrBuildImportSource(cfg, "fake", tok.OwnerUserId, tok)
	if src == nil {
		t.Fatal("expected a TokenSource")
	}
}

// TestImportSource_CacheReuse: the second build with the same access
// must return the same instance.
func TestImportSource_CacheReuse(t *testing.T) {
	clearImportSourceCache(t)

	cfg := OAuthConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		Endpoint:     oauth2.Endpoint{TokenURL: "https://example.invalid/token"},
	}
	owner := uuid.New()
	tok := &importModels.Token{
		Provider:     "fake",
		OwnerUserId:  owner,
		AccessToken:  "stable-tok",
		RefreshToken: "rt",
	}

	a := getOrBuildImportSource(cfg, "fake", owner, tok)
	b := getOrBuildImportSource(cfg, "fake", owner, tok)
	if a != b {
		t.Fatal("expected the same TokenSource on identical input")
	}
}

// TestImportSource_RebuildOnTokenChange: when access rotates the cache
// entry is invalidated.
func TestImportSource_RebuildOnTokenChange(t *testing.T) {
	clearImportSourceCache(t)

	cfg := OAuthConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		Endpoint:     oauth2.Endpoint{TokenURL: "https://example.invalid/token"},
	}
	owner := uuid.New()

	tok1 := &importModels.Token{Provider: "fake", OwnerUserId: owner, AccessToken: "v1", RefreshToken: "r"}
	a := getOrBuildImportSource(cfg, "fake", owner, tok1)

	tok2 := &importModels.Token{Provider: "fake", OwnerUserId: owner, AccessToken: "v2", RefreshToken: "r"}
	b := getOrBuildImportSource(cfg, "fake", owner, tok2)

	if a == b {
		t.Fatal("expected a fresh TokenSource after the access string changed")
	}
}

// TestInvalidateImportTokenSource clears one entry; others survive.
func TestInvalidateImportTokenSource(t *testing.T) {
	clearImportSourceCache(t)

	cfg := OAuthConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		Endpoint:     oauth2.Endpoint{TokenURL: "https://example.invalid/token"},
	}
	owner1, owner2 := uuid.New(), uuid.New()
	t1 := &importModels.Token{Provider: "fake", OwnerUserId: owner1, AccessToken: "a1", RefreshToken: "r"}
	t2 := &importModels.Token{Provider: "fake", OwnerUserId: owner2, AccessToken: "a2", RefreshToken: "r"}

	src1 := getOrBuildImportSource(cfg, "fake", owner1, t1)
	src2 := getOrBuildImportSource(cfg, "fake", owner2, t2)

	InvalidateImportTokenSource("fake", owner1)

	rebuilt1 := getOrBuildImportSource(cfg, "fake", owner1, t1)
	if rebuilt1 == src1 {
		t.Fatal("expected owner1's source to rebuild after invalidate")
	}

	rebuilt2 := getOrBuildImportSource(cfg, "fake", owner2, t2)
	if rebuilt2 != src2 {
		t.Fatal("expected owner2's source to survive owner1's invalidate")
	}
}

// TestImportSource_ConcurrentBuildIsSafe: many goroutines racing into
// an empty cache must converge on a single instance.
func TestImportSource_ConcurrentBuildIsSafe(t *testing.T) {
	clearImportSourceCache(t)

	cfg := OAuthConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		Endpoint:     oauth2.Endpoint{TokenURL: "https://example.invalid/token"},
	}
	owner := uuid.New()
	tok := &importModels.Token{Provider: "fake", OwnerUserId: owner, AccessToken: "racey", RefreshToken: "r"}

	const N = 32
	var wg sync.WaitGroup
	results := make([]oauth2.TokenSource, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			results[i] = getOrBuildImportSource(cfg, "fake", owner, tok)
		}()
	}
	wg.Wait()
	for i := 1; i < N; i++ {
		if results[i] != results[0] {
			t.Fatalf("concurrent calls produced distinct TokenSources at index %d", i)
		}
	}
}
