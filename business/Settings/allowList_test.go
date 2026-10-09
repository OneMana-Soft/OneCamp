package business

import (
	"slices"
	"strings"
	"testing"
)

// A public email domain can't be an allow-list entry: "@outlook.com" would let
// anyone in. Nor can an entry outside ASCII, which no sign-in matches. Both
// are refused before anything is written (no database in this test).
func TestAnAllowListRefusesPublicDomains(t *testing.T) {
	for _, list := range [][]string{{"ana@acme.com", "@Gmail.com"}, {"@outlook.com"}, {"@proton.me"}, {"Kate@acme.com"}} {
		err := Save(SaveInput{AllowedUsers: &list})
		refusal, ok := IsAllowListRefusal(err)
		if !ok || !strings.Contains(refusal.Msg, "@") {
			t.Errorf("%v: %v, want a refusal", list, err)
		}
	}
	if err := checkAllowList([]string{"ana@outlook.com", "@acme.com", "@mail.outlook.com.example"}); err != nil {
		t.Errorf("a person's own address and a company's domain: %v", err)
	}
}

// The audit log names what a save added and removed.
func TestAnAllowListChangeNamesItsEntries(t *testing.T) {
	added, removed := AllowListChange([]string{"ana@acme.com", "@old.example"}, []string{"ana@acme.com", "@acme.com", "bo@acme.com"})
	if !slices.Equal(added, []string{"@acme.com", "bo@acme.com"}) || !slices.Equal(removed, []string{"@old.example"}) {
		t.Errorf("added %v, removed %v", added, removed)
	}
}
