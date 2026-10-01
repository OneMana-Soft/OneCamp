package business

import "testing"

func TestNormalizeSocialPlatforms(t *testing.T) {
	// Valid + dupes + invalid → de-duped, order preserved, invalid dropped.
	got := normalizeSocialPlatforms([]string{"x_tweet", "X_TWEET", "reddit", "myspace"})
	if len(got) != 2 || got[0] != "x_tweet" || got[1] != "reddit" {
		t.Fatalf("unexpected normalize result: %v", got)
	}
	// Empty → default set.
	def := normalizeSocialPlatforms(nil)
	if len(def) != len(defaultSocialPlatforms) {
		t.Fatalf("expected default platforms, got %v", def)
	}
}

func TestParseSocialPosts(t *testing.T) {
	raw := "preamble noise\n" +
		"@@@x_tweet@@@\nShipped dark mode today. Your eyes can thank us.\n@@@end@@@\n" +
		"@@@reddit@@@\nTitle: We added dark mode\n\nBody here, honest and non-salesy.\n@@@end@@@\n"
	posts := parseSocialPosts(raw, []string{"x_tweet", "x_thread", "reddit"})

	if len(posts) != 2 {
		t.Fatalf("expected 2 parsed posts (x_thread absent), got %d: %+v", len(posts), posts)
	}
	if posts[0].Platform != "x_tweet" || posts[1].Platform != "reddit" {
		t.Errorf("unexpected order/platforms: %+v", posts)
	}
	if posts[0].Content == "" || posts[1].Content == "" {
		t.Errorf("empty content parsed: %+v", posts)
	}
}

func TestParseSocialPostsNoBlocks(t *testing.T) {
	// No delimiters → caller falls back; parser returns nothing.
	if got := parseSocialPosts("just some text", []string{"x_tweet"}); len(got) != 0 {
		t.Errorf("expected no posts for unformatted output, got %v", got)
	}
}
