package domain

import "strings"

// MemberName is how a member is named to other members: the first of names
// that is set and isn't an email address, else "". The users model's
// DisplayNamesByUUIDs falls back to the address, which suits the admin screens
// it was written for; a screen every member can open (an agent's card, a doc's
// suggestions) never shows one. Pure, beside SearchName, so a list may call it
// per row.
func MemberName(names ...string) string {
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" && !strings.Contains(n, "@") {
			return n
		}
	}
	return ""
}
