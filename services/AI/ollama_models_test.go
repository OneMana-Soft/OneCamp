package ai

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.14", "0.4.0", -1},
		{"0.4.0", "0.3.14", 1},
		{"0.4.0", "0.4.0", 0},
		{"v0.4.0", "0.4.0", 0}, // leading v ignored
		{"0.4", "0.4.0", 0},    // missing segments = 0
		{"0.4.1", "0.4.0", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.4.0-rc1", "0.4.0", 0}, // pre-release suffix stripped to 0
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsUpdateRequiredError(t *testing.T) {
	yes := []string{
		"this model requires newer version of ollama",
		"please update ollama to use this model",
		"unsupported model architecture 'llama4'",
	}
	for _, m := range yes {
		if !isUpdateRequiredError(m) {
			t.Errorf("expected update-required for %q", m)
		}
	}
	no := []string{
		"connection refused",
		"model not found",
		"out of memory",
	}
	for _, m := range no {
		if isUpdateRequiredError(m) {
			t.Errorf("did not expect update-required for %q", m)
		}
	}
}

func TestEmbeddingModelHint(t *testing.T) {
	embed := []string{"nomic-embed-text", "mxbai-embed-large", "bge-m3", "all-minilm", "gte-small"}
	for _, m := range embed {
		if !embeddingModelHint(m) {
			t.Errorf("expected %q to be detected as embedding model", m)
		}
	}
	chat := []string{"llama3.2:3b", "qwen2.5:7b", "mistral:7b"}
	for _, m := range chat {
		if embeddingModelHint(m) {
			t.Errorf("did not expect %q to be detected as embedding model", m)
		}
	}
}
