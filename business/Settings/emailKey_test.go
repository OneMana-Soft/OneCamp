package business

import (
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	emailService "github.com/akashc777/OneCamp/services/Email"
)

// The email service asks this package for its key, so a key saved in Admin
// (or, with none saved, the environment's) is the one it sends with. Without a
// database in this test, the environment's is the one there is.
func TestTheEmailServiceUsesTheWorkspacesKey(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	if emailService.IsEmailEnabled() {
		t.Fatal("email reads as on with no key anywhere")
	}
	t.Setenv("RESEND_API_KEY", "re_from_the_environment")
	forget()
	if !emailService.IsEmailEnabled() {
		t.Fatal("the environment's key is not used")
	}
	if got := ResendAPIKey(); got != "re_from_the_environment" {
		t.Fatalf("ResendAPIKey = %q", got)
	}
}

// A key saved in Admin sends from the sender saved beside it. On OneCamp
// Cloud the environment lends a key and a sender on OneCamp's domain; an
// admin's own key cannot send from that domain, so with theirs in use the
// environment's sender is not used, and with the lent key it still is.
func TestASavedKeySendsFromTheSavedSender(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "re_lent")
	t.Setenv("SENDER_EMAIL", "workspace-7@onemana.dev")
	t.Setenv("APP_SECRET_KEK", "a key for this test")
	saved, err := helpers.EncryptSecret("re_their_own")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(values map[string]string) {
		settingsMu.Lock()
		settingsCache, settingsExpires = values, time.Now().Add(time.Minute)
		settingsMu.Unlock()
	}
	t.Cleanup(forget)

	seed(map[string]string{keySenderEmail: "hello@acme.example"})
	if ResendAPIKey() != "re_lent" || emailService.SenderAddress() != "workspace-7@onemana.dev" {
		t.Errorf("the lent key: key %q, sender %q", ResendAPIKey(), emailService.SenderAddress())
	}
	seed(map[string]string{keyResendAPIKey: saved, keySenderEmail: "hello@acme.example"})
	if ResendAPIKey() != "re_their_own" || emailService.SenderAddress() != "hello@acme.example" {
		t.Errorf("their own key: key %q, sender %q", ResendAPIKey(), emailService.SenderAddress())
	}
	seed(map[string]string{keyResendAPIKey: saved, keySenderEmail: "noreply@onemana.dev"})
	if got := emailService.SenderAddress(); got != "workspace-7@onemana.dev" {
		t.Errorf("their own key with the seeded sender: %q, want the environment's", got)
	}
}

// The boot asks for the sending key before the database is connected. That
// answer is not remembered: remembered, every setting would read as unset for
// the first half minute after boot, and a key saved in Admin would not start
// the email worker.
func TestAReadBeforeTheDatabaseIsNotRemembered(t *testing.T) {
	forget()
	_ = emailService.IsEmailEnabled()
	settingsMu.RLock()
	cached := settingsCache != nil
	settingsMu.RUnlock()
	if cached {
		t.Fatal("settings read before the database was connected were cached")
	}
}
