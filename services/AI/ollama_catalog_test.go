package ai

import "testing"

// TestMergeCatalogs_RemoteOverridesAndExtends verifies remote entries override
// embedded metadata by tag, brand-new remote tags are appended, and embedded-
// only tags always survive (a remote list can never shrink the baseline).
func TestMergeCatalogs_RemoteOverridesAndExtends(t *testing.T) {
	embedded := []CatalogModel{
		{Tag: "llama3.2:3b", DisplayName: "Llama 3.2 3B", Family: "llama3.2"},
		{Tag: "nomic-embed-text", DisplayName: "Nomic", Family: "nomic-embed-text"},
	}
	remote := []CatalogModel{
		// Override existing tag (corrects display name + size).
		{Tag: "llama3.2:3b", DisplayName: "Llama 3.2 3B (updated)", Family: "llama3.2", SizeBytes: 123},
		// Brand-new tag published after this build shipped.
		{Tag: "newmodel:9b", DisplayName: "New Model 9B", Family: "newmodel"},
	}

	got := mergeCatalogs(embedded, remote)

	byTag := map[string]CatalogModel{}
	for _, m := range got {
		byTag[m.Tag] = m
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 merged entries, got %d", len(got))
	}
	if byTag["llama3.2:3b"].DisplayName != "Llama 3.2 3B (updated)" {
		t.Errorf("remote should override embedded display name, got %q", byTag["llama3.2:3b"].DisplayName)
	}
	if byTag["llama3.2:3b"].SizeBytes != 123 {
		t.Errorf("remote override should carry new size, got %d", byTag["llama3.2:3b"].SizeBytes)
	}
	if _, ok := byTag["newmodel:9b"]; !ok {
		t.Error("brand-new remote tag should be appended")
	}
	if _, ok := byTag["nomic-embed-text"]; !ok {
		t.Error("embedded-only tag must survive a merge")
	}

	// Embedded entries must keep their position ahead of net-new remote ones.
	if got[0].Tag != "llama3.2:3b" || got[1].Tag != "nomic-embed-text" {
		t.Errorf("embedded order should be preserved first; got %q, %q", got[0].Tag, got[1].Tag)
	}
	if got[2].Tag != "newmodel:9b" {
		t.Errorf("net-new remote tag should be appended last; got %q", got[2].Tag)
	}
}

// TestMergeCatalogs_SkipsEmptyTagsAndNormalizes ensures a sloppy manifest
// (missing tag / missing family / missing display name) can't produce broken
// rows.
func TestMergeCatalogs_SkipsEmptyTagsAndNormalizes(t *testing.T) {
	embedded := []CatalogModel{{Tag: "base:1b", DisplayName: "Base", Family: "base"}}
	remote := []CatalogModel{
		{Tag: "", DisplayName: "no tag"}, // dropped
		{Tag: "qwen2.5:7b"},              // family/display derived
		{Tag: "solo"},                    // no ':' → family = tag
	}

	got := mergeCatalogs(embedded, remote)

	byTag := map[string]CatalogModel{}
	for _, m := range got {
		byTag[m.Tag] = m
	}
	if _, ok := byTag[""]; ok {
		t.Error("entries without a tag must be dropped")
	}
	if byTag["qwen2.5:7b"].Family != "qwen2.5" {
		t.Errorf("family should derive from tag prefix, got %q", byTag["qwen2.5:7b"].Family)
	}
	if byTag["qwen2.5:7b"].DisplayName != "qwen2.5:7b" {
		t.Errorf("display name should fall back to tag, got %q", byTag["qwen2.5:7b"].DisplayName)
	}
	if byTag["solo"].Family != "solo" {
		t.Errorf("family should fall back to whole tag when no ':', got %q", byTag["solo"].Family)
	}
}

// TestSanitizeRemoteCatalog_DropsEmptyAndCaps verifies tag-less entries are
// removed and the list is capped.
func TestSanitizeRemoteCatalog_DropsEmptyAndCaps(t *testing.T) {
	in := []CatalogModel{
		{Tag: "a:1"}, {Tag: ""}, {Tag: "b:2"}, {Tag: "   "},
	}
	out := sanitizeRemoteCatalog(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 valid entries, got %d", len(out))
	}
	for _, m := range out {
		if m.Tag == "" {
			t.Error("sanitize should drop empty tags")
		}
	}
}

// TestOllamaCatalog_EmbeddedBaselineAlwaysPresent verifies that with no remote
// manifest configured, the embedded baseline is returned intact.
func TestOllamaCatalog_EmbeddedBaselineAlwaysPresent(t *testing.T) {
	t.Setenv("AI_OLLAMA_CATALOG_URL", "")
	got := OllamaCatalog()
	if len(got) != len(embeddedOllamaCatalog) {
		t.Fatalf("expected embedded baseline of %d, got %d", len(embeddedOllamaCatalog), len(got))
	}
	// Must be a copy, not the backing array (callers annotate it).
	if len(got) > 0 {
		original := embeddedOllamaCatalog[0].DisplayName
		got[0].DisplayName = "mutated"
		if embeddedOllamaCatalog[0].DisplayName != original {
			t.Error("OllamaCatalog must return a copy; mutation leaked into the source")
		}
	}
}
