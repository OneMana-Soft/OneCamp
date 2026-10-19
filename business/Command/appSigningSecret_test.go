package business

import (
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
)

// An app checks our signature with the secret it was given, not with what we
// store: the stored secret is encrypted, and commands were signed with the
// ciphertext, which no app could check.
func TestAnAppIsSignedForWithItsSecretNotItsCiphertext(t *testing.T) {
	t.Setenv("APP_SECRET_KEK", "app-signing-secret-test-kek")
	enc, err := helpers.EncryptSecret("deploy-bot-secret")
	if err != nil || enc == "deploy-bot-secret" {
		t.Fatalf("encrypt: %q %v", enc, err)
	}
	if got := appSigningSecret(&slashModel.App{SigningSecret: &enc}); got != "deploy-bot-secret" {
		t.Fatalf("signed with %q", got)
	}
	legacy := "stored-before-encryption"
	if got := appSigningSecret(&slashModel.App{SigningSecret: &legacy}); got != legacy {
		t.Fatalf("a secret from before encryption: %q", got)
	}
	if got := appSigningSecret(&slashModel.App{}); got != "" {
		t.Fatalf("no secret: %q", got)
	}
}
