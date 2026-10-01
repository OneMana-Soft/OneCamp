package business

import "testing"

func TestNormalizeMode(t *testing.T) {
	cases := map[string]string{
		"frontend":  ModeFrontend,
		"FRONTEND":  ModeFrontend,
		" backend ": ModeBackend,
		"off":       ModeOff,
		"":          defaultMode,
		"garbage":   defaultMode,
	}
	for in, want := range cases {
		if got := normalizeMode(in); got != want {
			t.Errorf("normalizeMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeSTT(t *testing.T) {
	cases := map[string]string{
		"deepgram": STTDeepgram,
		"google":   STTGoogle,
		"OpenAI":   STTOpenAI,
		" openai ": STTOpenAI,
		"":         defaultSTTProvider,
		"whisper":  defaultSTTProvider, // unknown coerces to default
	}
	for in, want := range cases {
		if got := normalizeSTT(in); got != want {
			t.Errorf("normalizeSTT(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsValidSTTProvider(t *testing.T) {
	valid := []string{"deepgram", "google", "openai", "OPENAI", " google "}
	for _, v := range valid {
		if !IsValidSTTProvider(v) {
			t.Errorf("IsValidSTTProvider(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "whisper", "azure", "aws"}
	for _, v := range invalid {
		if IsValidSTTProvider(v) {
			t.Errorf("IsValidSTTProvider(%q) = true, want false", v)
		}
	}
}

func TestDefaultModelForProvider(t *testing.T) {
	cases := map[string]string{
		STTDeepgram: "nova-2",
		STTOpenAI:   "whisper-1",
		STTGoogle:   "", // plugin default
		"unknown":   "",
	}
	for provider, want := range cases {
		if got := defaultModelForProvider(provider); got != want {
			t.Errorf("defaultModelForProvider(%q) = %q, want %q", provider, got, want)
		}
	}
}

func TestLocalProviderIsValidAndKeyless(t *testing.T) {
	if !IsValidSTTProvider(STTLocal) {
		t.Error("the bundled server is not an accepted provider, so an admin cannot select it")
	}
	// The bundled server DOES require a bearer token, so this is not "it needs
	// no credential" but "the admin supplies none": the server passes its own
	// internal secret. Asking a person to type it would be asking them to
	// re-enter a value the process already holds.
	if UsesAPIKey(STTLocal) {
		t.Error("UsesAPIKey(local) = true, want false: the admin supplies no key")
	}
	for _, p := range []string{STTDeepgram, STTOpenAI, STTGoogle} {
		if !UsesAPIKey(p) {
			t.Errorf("UsesAPIKey(%q) = false, want true", p)
		}
	}
	// The OpenAI audio API requires a model name even when the server ignores it.
	if got := defaultModelForProvider(STTLocal); got == "" {
		t.Error("the bundled server has no default model, so a request would omit a required field")
	}
}

func TestLocalBaseURLIsNotAdminEditable(t *testing.T) {
	// The whole safety argument for skipping the SSRF guard on this provider is
	// that nobody outside the server chooses this URL. If it ever starts coming
	// from saved admin config, that argument is gone.
	t.Setenv("STT_LOCAL_BASE_URL", "")
	if got := LocalSTTBaseURL(); got != localSTTBaseURL {
		t.Errorf("LocalSTTBaseURL() = %q, want the constant %q", got, localSTTBaseURL)
	}
	t.Setenv("STT_LOCAL_BASE_URL", "http://elsewhere:9000/v1")
	if got := LocalSTTBaseURL(); got != "http://elsewhere:9000/v1" {
		t.Errorf("env override ignored: got %q", got)
	}
}

func TestLocalSTTKeyIsTheStackSecret(t *testing.T) {
	// The bundled server refuses every request without a bearer token, so an
	// empty key here is not "no auth needed", it is "nothing will work". If this
	// ever stops being wired to the stack secret, the provider silently 401s on
	// every call and the failure lands in an agent container log.
	t.Setenv("INTERNAL_SECRET", "  s3cret  ")
	if got := LocalSTTAPIKey(); got != "s3cret" {
		t.Errorf("LocalSTTAPIKey() = %q, want the trimmed stack secret", got)
	}
	t.Setenv("INTERNAL_SECRET", "")
	if got := LocalSTTAPIKey(); got != "" {
		t.Errorf("LocalSTTAPIKey() = %q, want empty when the stack secret is unset", got)
	}
}
