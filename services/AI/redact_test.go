package ai

import (
	"strings"
	"testing"
)

func TestRedactorDetectsDefaultTypes(t *testing.T) {
	r := NewRedactor(nil)

	cases := []struct {
		name  string
		in    string
		label string
	}{
		{"email", "reach me at jane.doe@example.com please", "EMAIL"},
		{"ssn", "SSN 123-45-6789 on file", "GOV_ID"},
		{"iban", "account GB82WEST12345698765432 here", "IBAN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, counts := r.Apply(c.in)
			if counts[c.label] != 1 {
				t.Fatalf("expected one %s redaction, got counts=%v (out=%q)", c.label, counts, out)
			}
			if strings.Contains(out, "@example.com") && c.label == "EMAIL" {
				t.Fatalf("email not redacted: %q", out)
			}
			if !strings.Contains(out, "["+c.label+"_1]") {
				t.Fatalf("expected placeholder [%s_1] in %q", c.label, out)
			}
		})
	}
}

func TestRedactorCreditCardLuhn(t *testing.T) {
	r := NewRedactor(nil)

	// 4242 4242 4242 4242 is a valid Luhn test card.
	out, counts := r.Apply("card 4242 4242 4242 4242 end")
	if counts["CARD"] != 1 {
		t.Fatalf("expected card redaction, got %v (%q)", counts, out)
	}

	// A 16-digit number that fails Luhn must NOT be redacted as a card.
	out2, counts2 := r.Apply("order 1234 5678 9012 3456 ref")
	if counts2["CARD"] != 0 {
		t.Fatalf("non-Luhn number should not be a card, got %v (%q)", counts2, out2)
	}
}

func TestRedactorStablePlaceholders(t *testing.T) {
	r := NewRedactor(nil)
	out, counts := r.Apply("a@x.com talked to a@x.com and b@x.com")
	// Same value collapses to the same placeholder; distinct value gets a new one.
	if counts["EMAIL"] != 2 {
		t.Fatalf("expected 2 distinct email placeholders, got %v", counts)
	}
	if strings.Count(out, "[EMAIL_1]") != 2 {
		t.Fatalf("repeated value should reuse [EMAIL_1]: %q", out)
	}
	if !strings.Contains(out, "[EMAIL_2]") {
		t.Fatalf("distinct value should get [EMAIL_2]: %q", out)
	}
}

func TestRedactorCustomPatternAndInvalidSkipped(t *testing.T) {
	// One valid custom pattern, one invalid (must be skipped, not panic).
	r := NewRedactor([]string{`EMP-\d{4}`, `([`})
	out, counts := r.Apply("ticket EMP-0421 raised")
	if counts["CUSTOM"] != 1 {
		t.Fatalf("expected custom redaction, got %v (%q)", counts, out)
	}
	if strings.Contains(out, "EMP-0421") {
		t.Fatalf("custom pattern not applied: %q", out)
	}
}

func TestRedactorOversizeFailsClosed(t *testing.T) {
	r := NewRedactor(nil)
	big := strings.Repeat("x", maxRedactBytes+1)
	if _, _, err := r.ApplyMessages([]ChatMessage{{Role: "user", Content: big}}); err == nil {
		t.Fatal("expected oversize content to fail closed, got nil error")
	}
	if _, _, err := r.ApplyTexts([]string{big}); err == nil {
		t.Fatal("expected oversize text to fail closed, got nil error")
	}
}

func TestNilRedactorIsNoop(t *testing.T) {
	var r *Redactor
	msgs := []ChatMessage{{Role: "user", Content: "email a@b.com"}}
	out, counts, err := r.ApplyMessages(msgs)
	if err != nil {
		t.Fatalf("nil redactor should not error: %v", err)
	}
	if len(counts) != 0 {
		t.Fatalf("nil redactor should redact nothing, got %v", counts)
	}
	if out[0].Content != "email a@b.com" {
		t.Fatalf("nil redactor must not mutate content: %q", out[0].Content)
	}
}

func TestRedactorEndpointClassification(t *testing.T) {
	cfg := &AIConfig{PIIRedactionEnabled: true, OllamaHost: "http://localhost:11434"}

	// Local Ollama endpoint → no redactor.
	if got := cfg.redactorForEndpoint(Endpoint{Kind: ProviderOllama}); got != nil {
		t.Fatal("local ollama endpoint must not get a redactor")
	}
	// Cloud OpenAI endpoint → redactor present.
	if got := cfg.redactorForEndpoint(Endpoint{Kind: ProviderOpenAI}); got == nil {
		t.Fatal("cloud openai endpoint must get a redactor when redaction is on")
	}
	// Redaction off → never a redactor.
	cfg.PIIRedactionEnabled = false
	if got := cfg.redactorForEndpoint(Endpoint{Kind: ProviderOpenAI}); got != nil {
		t.Fatal("redaction off must never produce a redactor")
	}
}
