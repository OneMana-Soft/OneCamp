package business

import "testing"

func TestValidateBaseURL(t *testing.T) {
	ok := map[string]string{
		"http://ollama:11434":        "http://ollama:11434",
		"https://api.openai.com/v1/": "https://api.openai.com/v1",
		"http://vllm:8000/v1":        "http://vllm:8000/v1",
		"  https://gw.local/v1  ":    "https://gw.local/v1",
		"http://10.0.0.5:8080":       "http://10.0.0.5:8080",
	}
	for in, want := range ok {
		got, err := validateBaseURL(in)
		if err != nil {
			t.Errorf("validateBaseURL(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("validateBaseURL(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{
		"",                          // empty
		"ftp://host/v1",             // bad scheme
		"file:///etc/passwd",        // bad scheme
		"gopher://host",             // bad scheme
		"https://user:pass@host/v1", // embedded credentials
		"not a url at all ::::",     // unparseable
		"http://",                   // no host
	}
	for _, in := range bad {
		if _, err := validateBaseURL(in); err == nil {
			t.Errorf("validateBaseURL(%q) expected error, got nil", in)
		}
	}
}

func TestValidateLabel(t *testing.T) {
	if _, err := validateLabel(""); err == nil {
		t.Error("empty label should error")
	}
	if _, err := validateLabel("   "); err == nil {
		t.Error("whitespace label should error")
	}
	long := make([]byte, maxLabelLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := validateLabel(string(long)); err == nil {
		t.Error("over-long label should error")
	}
	got, err := validateLabel("  My vLLM  ")
	if err != nil || got != "My vLLM" {
		t.Errorf("validateLabel trim failed: got %q err %v", got, err)
	}
}

func TestValidateModelName(t *testing.T) {
	ok := []string{"llama3.2:3b", "gpt-4o-mini", "org/model:tag", "nomic-embed-text"}
	for _, m := range ok {
		if _, err := validateModelName(m); err != nil {
			t.Errorf("validateModelName(%q) unexpected error: %v", m, err)
		}
	}
	if _, err := validateModelName(""); err == nil {
		t.Error("empty model should error")
	}
	if _, err := validateModelName("bad\x00name"); err == nil {
		t.Error("control char model should error")
	}
}

func TestValidateAPIKey(t *testing.T) {
	if v, err := validateAPIKey(""); err != nil || v != "" {
		t.Errorf("empty key should be allowed and empty: got %q err %v", v, err)
	}
	if _, err := validateAPIKey("sk-line1\nline2"); err == nil {
		t.Error("newline in key should error (header injection)")
	}
	if v, err := validateAPIKey("  sk-abc  "); err != nil || v != "sk-abc" {
		t.Errorf("key trim failed: got %q err %v", v, err)
	}
}
