package business

import (
	"strings"
	"testing"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
)

// A supplied key must replace the stored one for the duration of a test.
//
// The bug: TestConnection loaded the provider by id and then never looked at req.APIKey, so the
// endpoint accepted the field and ignored it. An admin pasting a new key and pressing "Test
// connection" was testing the key already in the database.
func TestApplyTestKeyOverrideReplacesTheStoredKey(t *testing.T) {
	p := &aiModels.AIProvider{APIKey: "stored-key", HasAPIKey: true}

	if err := applyTestKeyOverride(p, "gsk_freshly_pasted_key"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.APIKey != "gsk_freshly_pasted_key" {
		t.Errorf("the supplied key must be used, got %q", p.APIKey)
	}
}

// AN UNREADABLE STORED KEY MUST NOT BLOCK A TEST OF A NEW ONE.
//
// This is the case that made the original bug so confusing to diagnose. buildProviderClient refuses
// to build a client when KeyUnreadable is set, and rightly so — an undecryptable key arrives as an
// empty string, no Authorization header is sent, and the provider answers "Invalid API Key", which
// blames the admin's credential for a request that carried none.
//
// But that guard is about the STORED key. Once a caller supplies one, the stored key is not in play.
// Leaving the flag set means an admin can never test their way out of an unreadable key, which is
// precisely when they need to.
func TestApplyTestKeyOverrideClearsUnreadableFlag(t *testing.T) {
	p := &aiModels.AIProvider{
		APIKey:        "", // what an undecryptable key looks like after scanProvider tolerates it
		HasAPIKey:     false,
		KeyUnreadable: true,
	}

	if err := applyTestKeyOverride(p, "gsk_replacement_key"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.KeyUnreadable {
		t.Error("KeyUnreadable must be cleared, or buildProviderClient refuses and the admin can " +
			"never test a replacement key")
	}
	if !p.HasAPIKey {
		t.Error("HasAPIKey must follow the supplied key, or the guard downstream sees a keyless provider")
	}
	if p.APIKey != "gsk_replacement_key" {
		t.Errorf("the supplied key must be used, got %q", p.APIKey)
	}
}

// A blank key means "test what is stored", and must not blank the stored key.
//
// The frontend sends `api_key: apiKey || undefined` for exactly this: an untouched field is not an
// instruction to test with no credential. If a blank key overwrote the stored one, pressing Test
// without touching the field would strip the Authorization header and report a 401 on a provider
// that is perfectly healthy.
func TestApplyTestKeyOverrideIgnoresABlankKey(t *testing.T) {
	p := &aiModels.AIProvider{APIKey: "stored-key", HasAPIKey: true, KeyUnreadable: false}

	if err := applyTestKeyOverride(p, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.APIKey != "stored-key" {
		t.Errorf("a blank supplied key must leave the stored key alone, got %q", p.APIKey)
	}
	if !p.HasAPIKey {
		t.Error("a blank supplied key must not change HasAPIKey")
	}
}

// A blank key must NOT resurrect a provider whose stored key is unreadable.
//
// Pinned because the obvious implementation — clear the flag unconditionally — would make the test
// button appear to work while the client is built with an empty key, putting back the misleading
// "Invalid API Key" this change removes.
func TestApplyTestKeyOverrideLeavesUnreadableAloneWithoutAKey(t *testing.T) {
	p := &aiModels.AIProvider{KeyUnreadable: true}

	if err := applyTestKeyOverride(p, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !p.KeyUnreadable {
		t.Error("with no key supplied there is nothing to test but the unreadable stored key, so the " +
			"flag must stand and buildProviderClient must still refuse")
	}
}

// Validation still applies to a supplied key.
//
// The key goes into an HTTP header, so a newline is a request-splitting hazard, not a typo to pass
// through. Rejected as a failed test with the reason rather than as a 500.
func TestApplyTestKeyOverrideRejectsAnInvalidKey(t *testing.T) {
	p := &aiModels.AIProvider{APIKey: "stored-key"}

	err := applyTestKeyOverride(p, "line-one\nline-two")
	if err == nil {
		t.Fatal("a key containing a newline must be rejected before it reaches a header")
	}
	if !strings.Contains(err.Error(), "invalid characters") {
		t.Errorf("the reason should say what is wrong: %v", err)
	}
	if p.APIKey != "stored-key" {
		t.Errorf("a rejected key must not have been applied, got %q", p.APIKey)
	}
}

// Whitespace around a pasted key must not be sent to the provider.
//
// Copying from a console reliably picks up a trailing newline or space. A key with trailing
// whitespace is rejected by the provider as invalid, which sends the admin looking for a problem
// with the key itself.
func TestApplyTestKeyOverrideTrimsAPastedKey(t *testing.T) {
	p := &aiModels.AIProvider{}

	if err := applyTestKeyOverride(p, "  gsk_pasted_with_padding \t"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.APIKey != "gsk_pasted_with_padding" {
		t.Errorf("expected the key to be trimmed, got %q", p.APIKey)
	}
}

// A nil provider must not panic.
func TestApplyTestKeyOverrideHandlesNil(t *testing.T) {
	if err := applyTestKeyOverride(nil, "anything"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
