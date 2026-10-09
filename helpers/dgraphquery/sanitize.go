// Package dgraphquery centralises every input-sanitisation helper used
// when assembling DQL strings via fmt.Sprintf.
//
// Why this matters:
//
//	Dgraph DQL is *not* a parameterised language for full-text search
//	filters like regexp(...). Variable bindings (e.g. $user_id) only
//	work for top-level argument types, not inside DQL function calls
//	like regexp() or anyofterms(). Every search endpoint in the
//	codebase therefore interpolates user-supplied search text into the
//	query string with fmt.Sprintf, e.g.:
//
//	    regexp(ch_name, /.*<text>.*/i)
//
//	Without sanitisation an authenticated user can inject DQL operators
//	to break out of the regex literal, leak data they shouldn't see, or
//	hang Dgraph with catastrophic regexes like (a+)+$.
//
// EscapeRegexLiteral and SafeSearchTerm are the two callers should
// reach for. Use them at every controller / business / domain entry
// point that accepts a search-text from the request.
package dgraphquery

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxSearchTermLen bounds the search text we accept from a client.
// Real-world chat/channel/doc names are well under 100 chars; 100 is
// the cap. Tunable per call via SanitizeSearchTerm if needed.
const MaxSearchTermLen = 100

// ErrEmpty is returned when input contains no usable characters after
// sanitisation. Callers usually want to short-circuit and return an
// empty result list.
var ErrEmpty = errors.New("search term is empty after sanitisation")

// ErrTooLong is returned when input exceeds MaxSearchTermLen. Callers
// should surface a 400 to the client.
var ErrTooLong = errors.New("search term exceeds maximum length")

// regexMetaChars are the characters that have special meaning inside a
// Dgraph (Go RE2) regular expression literal. We backslash-escape every
// occurrence so a search term cannot terminate the regex or inject new
// alternations / classes.
//
// We deliberately escape both the `/` and `\` characters in addition to
// regex metacharacters because the search filter wraps the term in
// /.../i delimiters; an un-escaped `/` would close the literal and
// allow injection of arbitrary DQL after it.
var regexMetaChars = regexp.MustCompile(`[\\/.+*?()\[\]{}|^$]`)

// EscapeRegexLiteral makes a string safe to substitute into a Dgraph
// regexp(field, /.*<term>.*/i) filter. It does NOT bound length; pair
// with SanitizeSearchTerm for the full guard.
func EscapeRegexLiteral(s string) string {
	return regexMetaChars.ReplaceAllString(s, `\${0}`)
}

// SanitizeSearchTerm runs the full sanitisation pipeline on a user-
// supplied search term: strip control characters, trim, length-cap,
// regex-escape. Returns the safe-to-interpolate value or an error.
//
// Callers in controllers should treat ErrEmpty as "return empty list,
// HTTP 200" because an over-aggressive sanitiser stripping a user's
// input to "" should not surface as a 400 — that's hostile UX. ErrTooLong,
// on the other hand, indicates an explicit bad request.
func SanitizeSearchTerm(in string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", ErrEmpty
	}
	if utf8.RuneCountInString(in) > MaxSearchTermLen {
		return "", ErrTooLong
	}
	// Drop control characters and any zero-width / format runes that
	// are the lever for unicode-confusion attacks. Keep printable
	// graphemes and CJK / emoji; users should be able to search those.
	var b strings.Builder
	for _, r := range in {
		if r == utf8.RuneError {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "", ErrEmpty
	}
	return EscapeRegexLiteral(out), nil
}

// IsAllowedColumnName returns true when name matches the strict
// `[a-z_][a-z0-9_]{0,63}` shape that DQL field names use. Useful when
// callers accept "filter by column X" inputs and need to reject
// anything that could be a DQL keyword or operator.
func IsAllowedColumnName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		isAlpha := (r >= 'a' && r <= 'z') || r == '_'
		isAlnum := isAlpha || (r >= '0' && r <= '9')
		if i == 0 && !isAlpha {
			return false
		}
		if !isAlnum {
			return false
		}
	}
	return true
}

// IsAllowedFilterValue accepts the strict shape used by anyofterms()
// and uid_in() value lists: ASCII letters, digits, `-`, `_`, `.`, `:`.
// This matches Slack-style status/priority enum values, ISO timestamps,
// UUIDs, and Dgraph UIDs while rejecting anything that could break out
// of the surrounding double-quoted token list.
//
// Empty input returns false. Length is capped at 64 chars; longer values
// are almost certainly bogus or hostile.
func IsAllowedFilterValue(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == ':'
		if !ok {
			return false
		}
	}
	return true
}

// uidPattern is a Dgraph uid as the graph writes one.
var uidPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{1,16}$`)

// AllUIDs reports whether every id is a Dgraph uid, so a list of them can be
// written into a query's uid(...). The lists come from requests and from the
// mention chips in messages anyone writes; one item like `0x1) OR has(...`
// used to become part of the query.
func AllUIDs(ids []string) bool {
	for _, id := range ids {
		if !uidPattern.MatchString(id) {
			return false
		}
	}
	return true
}
