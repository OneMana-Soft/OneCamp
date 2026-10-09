package business

import (
	"strings"
	"testing"
)

// An install from before addresses were lowercased can hold two accounts that
// differ only in capital letters. The check says so by address, as a note
// (every account still works), and says nothing when there are none.
func TestTwoAccountsAtOneAddressAreNamed(t *testing.T) {
	if sharedAddressesNote(nil) != nil {
		t.Error("a workspace with no shared addresses gets a note")
	}
	note := sharedAddressesNote([]string{"ana@acme.test", "lee@acme.test"})
	if note == nil || !strings.Contains(note.Error(), "ana@acme.test, lee@acme.test") || !strings.Contains(note.Error(), "Deactivate") {
		t.Errorf("note = %v, want both addresses and what to do", note)
	}
	// An address is lowercased before it is looked up, so "as typed" was
	// never what decided: the all-lowercase account is the one reached.
	if strings.Contains(note.Error(), "as typed") || !strings.Contains(note.Error(), "all lowercase") {
		t.Errorf("note = %v, want it to say which account a sign-in reaches", note)
	}
}
