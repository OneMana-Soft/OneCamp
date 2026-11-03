package business

import (
	"net/url"
	"strings"
)

// URLForRecord is an address as an audit row keeps it: scheme, host and path.
// The credentials, query and fragment are dropped, because any of them can
// carry a key (https://user:token@runner.example.com, ?key=...), and an audit
// row is kept, exported and shown to every admin. An address that doesn't
// parse, or names no host, is kept as "(unreadable address)", never as its raw
// text, because its credentials can't be told from the rest: an opaque one
// (admin:secret@runner.local/x reads as scheme "admin") never has a host. No
// address stays "". Pure.
func URLForRecord(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "(unreadable address)"
	}
	u.User = nil
	u.RawQuery, u.ForceQuery = "", false
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}
