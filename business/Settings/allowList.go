package business

// What the sign-up allow-list may hold, and what changed when it is saved.

import (
	"errors"
	"fmt"
	"slices"

	"github.com/akashc777/OneCamp/helpers"
)

// publicMailDomains are domains anyone can have an address at. As an
// allow-list entry ("@outlook.com") one would let everybody in, so it can't be
// saved; the people's own addresses can.
var publicMailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true,
	"outlook.com": true, "hotmail.com": true, "live.com": true, "msn.com": true,
	"yahoo.com": true, "ymail.com": true,
	"icloud.com": true, "me.com": true,
	"aol.com":   true,
	"proton.me": true, "protonmail.com": true,
	"gmx.com": true, "gmx.net": true,
	"mail.com":   true,
	"yandex.com": true,
	"zoho.com":   true,
}

// AllowListRefusal is why an allow-list can't be saved, in words for the
// admin who tried.
type AllowListRefusal struct{ Msg string }

func (e *AllowListRefusal) Error() string { return e.Msg }

// IsAllowListRefusal reports whether err is an AllowListRefusal, and returns it.
func IsAllowListRefusal(err error) (*AllowListRefusal, bool) {
	var r *AllowListRefusal
	ok := errors.As(err, &r)
	return r, ok
}

// checkAllowList refuses entries that can't be on the allow-list: a public
// email domain, and anything written outside ASCII, which no sign-in is
// matched to (helpers.NormalizeEmail). Entries are as splitEmails leaves
// them. Pure.
func checkAllowList(entries []string) error {
	for _, entry := range entries {
		if !helpers.AddressIsASCII(entry) {
			return &AllowListRefusal{Msg: fmt.Sprintf("%s has characters other than plain letters, digits and symbols. No sign-in is matched to it, so it can't be on the allow-list.", entry)}
		}
		if len(entry) > 1 && entry[0] == '@' && publicMailDomains[entry[1:]] {
			return &AllowListRefusal{Msg: fmt.Sprintf("%s is a public email domain: anyone can have an address there, so it would let anyone in. Add people's own addresses, or your company's domain.", entry)}
		}
	}
	return nil
}

// AllowListChange is what saving after over before added and removed, for
// the audit log to name. Pure.
func AllowListChange(before, after []string) (added, removed []string) {
	for _, entry := range after {
		if !slices.Contains(before, entry) && !slices.Contains(added, entry) {
			added = append(added, entry)
		}
	}
	for _, entry := range before {
		if !slices.Contains(after, entry) && !slices.Contains(removed, entry) {
			removed = append(removed, entry)
		}
	}
	return added, removed
}
