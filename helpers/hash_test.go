package helpers

import "testing"

// The fingerprint's whole job is to be the same for the same text and different
// for different text, stably, across restarts and versions. A hash that is
// merely "probably stable" is useless for proving what a run was told.
func TestSHA256Hex(t *testing.T) {
	const prompt = "You are a helpful teammate.\n\n### Tone\nBe brief."

	got := SHA256Hex(prompt)
	if len(got) != 64 {
		t.Fatalf("fingerprint is %d chars, want 64 hex", len(got))
	}
	if got != SHA256Hex(prompt) {
		t.Fatal("same text produced two different fingerprints")
	}
	// A single character changes the whole thing, which is what makes a silent
	// edit to a shared skill detectable after the fact.
	if got == SHA256Hex(prompt+" ") {
		t.Fatal("a trailing space did not change the fingerprint")
	}
	// Pinned, so an implementation change that alters every stored fingerprint
	// has to be a deliberate act rather than a quiet one.
	if want := SHA256Hex(""); want != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty-string fingerprint changed: %s", want)
	}
}
