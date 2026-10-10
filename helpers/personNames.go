package helpers

// What people are called, and their @handle.
//
// A person has a display name, the name everyone sees on their messages, and
// a handle: short, unique, derived from the name when they join. The display
// name follows the web app's rule for a person (lib/validation/names.ts,
// "person"), so a name accepted there is never refused here: letters in any
// language, spaces, apostrophes, hyphens and full stops, up to 60 characters.
// José, O'Brien and priya.raman are all names.
//
// Sign-up used to check only the length, identity providers wrote whatever
// they had (a GitHub login, a UPN with an @ in it), and the profile editor
// then refused most of what they produced. Names now arrive cleaned
// (CleanPersonName) and are checked by one rule.

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// The person rule, as lib/validation/names.ts has it.
const (
	PersonNameMaxRunes = 60
	// PersonNameRuleMessage says what IsValidPersonName accepts.
	PersonNameRuleMessage = "A name can use letters, spaces, apostrophes, hyphens and full stops, up to 60 characters, with at least one letter or number."
)

var personNamePattern = regexp.MustCompile(`^[\p{L}\p{M}\p{N} .'’-]+$`)

// NormalizePersonName is a name as it is kept: trimmed, in Unicode NFC, so
// "José" typed with a separate accent (e and U+0301) is the same name as with
// é, and looks and sorts as one. Pure.
func NormalizePersonName(name string) string {
	return norm.NFC.String(strings.TrimSpace(name))
}

// IsValidPersonName reports whether name, in its kept form
// (NormalizePersonName), is a person's name the rule accepts: 1 to 60
// characters of the kinds it allows, with at least one letter or number
// ("..." or "-" is nobody's name), and nothing that can't be seen. A
// variation selector or an accent with no letter under it (a lone combining
// mark) is invisible, so two names that look identical would differ. Pure.
func IsValidPersonName(name string) bool {
	name = NormalizePersonName(name)
	n := utf8.RuneCountInString(name)
	return n >= 1 && n <= PersonNameMaxRunes && personNamePattern.MatchString(name) && seenAsTyped(name)
}

// seenAsTyped reports whether every character of name can be seen where it
// is: it has a letter or number, no format character (unicode.Cf, such as a
// zero-width space) or variation selector, and every combining mark sits on a
// letter or number (possibly after other marks). Pure.
func seenAsTyped(name string) bool {
	hasLetter, prevBase := false, false
	for _, r := range name {
		switch {
		case unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Variation_Selector, r):
			return false
		case unicode.Is(unicode.M, r):
			if !prevBase {
				return false
			}
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			hasLetter, prevBase = true, true
		default:
			prevBase = false
		}
	}
	return hasLetter
}

// CleanPersonName turns what a sign-up or an identity provider supplied into
// a name the rule accepts, or "" when nothing of a name is left. An address
// (a UPN) gives its part before the @; anything the rule does not allow
// becomes a space ("octo_cat" is "octo cat", "CORP\priya" is "CORP priya");
// spaces collapse; and it is cut to the longest whole words that fit. Pure.
func CleanPersonName(raw string) string {
	raw = NormalizePersonName(raw)
	if at := strings.IndexByte(raw, '@'); at > 0 {
		raw = raw[:at]
	}
	var b strings.Builder
	prevBase := false
	for _, r := range raw {
		switch {
		case unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Variation_Selector, r):
			// Unseen: dropped, not made a space, so "Ana\u200b" is Ana.
		case unicode.Is(unicode.M, r):
			if prevBase {
				b.WriteRune(r)
			}
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b.WriteRune(r)
			prevBase = true
			continue
		case r == ' ' || r == '.' || r == '\'' || r == '’' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
		if !unicode.Is(unicode.M, r) {
			prevBase = false
		}
	}
	name := strings.Join(strings.Fields(b.String()), " ")
	for utf8.RuneCountInString(name) > PersonNameMaxRunes {
		cut := strings.LastIndexByte(name, ' ')
		if cut <= 0 {
			name = string([]rune(name)[:PersonNameMaxRunes])
			break
		}
		name = name[:cut]
	}
	name = strings.Trim(name, " .-'’")
	if !IsValidPersonName(name) {
		return ""
	}
	return name
}

// The handle rule: lowercase, short, unique (users.username).
const (
	HandleMinRunes = 2
	HandleMaxRunes = 30
)

// HandleRuleMessage says what IsValidHandle accepts.
const HandleRuleMessage = "A handle can use 2 to 30 lowercase letters, numbers, full stops, hyphens and underscores, starting with a letter or number."

// reservedHandles are words a mention means for a group of people: nobody's
// handle may be one, or "@here" would mean one person in one message and
// everyone present in the next. HandleCandidate passes them over.
var reservedHandles = map[string]bool{"everyone": true, "here": true, "channel": true, "all": true, "admin": true}

// HandleIsReserved reports whether a (normalized) handle is one no person can
// have: see reservedHandles. Pure.
func HandleIsReserved(handle string) bool { return reservedHandles[handle] }

// NormalizeHandle is a handle as typed made into the form it is kept in:
// trimmed, without a leading @, lowercase, in Unicode NFC. Pure.
func NormalizeHandle(handle string) string {
	return norm.NFC.String(strings.ToLower(strings.TrimPrefix(strings.TrimSpace(handle), "@")))
}

// IsValidHandle reports whether handle, already normalized, is one a person
// may choose: 2 to 30 characters, lowercase letters in any script, numbers,
// and . _ - after the first, which is a letter or a number; a combining mark
// only on a letter or number, nothing unseen (a format character or a
// variation selector), and not a reserved word (reservedHandles). Pure.
func IsValidHandle(handle string) bool {
	n := utf8.RuneCountInString(handle)
	if n < HandleMinRunes || n > HandleMaxRunes || handle != strings.ToLower(handle) || HandleIsReserved(handle) {
		return false
	}
	prevBase := false
	for i, r := range []rune(handle) {
		switch {
		case unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Variation_Selector, r):
			return false
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			prevBase = true
			continue
		case i > 0 && unicode.Is(unicode.M, r) && prevBase:
			continue
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
		prevBase = false
	}
	return true
}

// HandleFromName is the handle a person who joins with this name is given:
// lowercased, an apostrophe dropped (O'Brien is obrien), the separators a
// handle keeps (. _ -) kept, and anything else between words made a hyphen,
// so "José O'Brien" is josé-obrien and priya.raman stays priya.raman. When
// the name gives too little, the address's part before the @ is used, and
// then "member". Collisions are resolved by HandleCandidate. Pure.
func HandleFromName(name, email string) string {
	name = norm.NFC.String(name)
	if h := handleFrom(name); h != "" {
		return h
	}
	if at := strings.IndexByte(email, '@'); at > 0 {
		if h := handleFrom(email[:at]); h != "" {
			return h
		}
	}
	return "member"
}

// handleFrom does HandleFromName's work on one string, or returns "" when
// fewer than HandleMinRunes characters are left. Pure.
func handleFrom(s string) string {
	var out []rune
	pendingSep := rune(0)
	for _, r := range strings.ToLower(s) {
		switch {
		case r == '\'' || r == '’' || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Variation_Selector, r):
			continue
		case unicode.IsLetter(r) || unicode.IsNumber(r) || (unicode.Is(unicode.M, r) && len(out) > 0):
			if pendingSep != 0 && len(out) > 0 {
				out = append(out, pendingSep)
			}
			pendingSep = 0
			out = append(out, r)
		case r == '.' || r == '_' || r == '-':
			if pendingSep == 0 {
				pendingSep = r
			}
		default:
			if pendingSep == 0 {
				pendingSep = '-'
			}
		}
	}
	if len(out) > HandleMaxRunes {
		out = out[:HandleMaxRunes]
	}
	h := strings.TrimRight(string(out), "._-")
	if utf8.RuneCountInString(h) < HandleMinRunes {
		return ""
	}
	return h
}

// HandleCandidate is the nth handle to try for base: base itself first, then
// base-2, base-3 and so on, cut so the whole stays within HandleMaxRunes.
// Pure.
func HandleCandidate(base string, n int) string {
	if n <= 1 {
		return base
	}
	suffix := fmt.Sprintf("-%d", n)
	runes := []rune(base)
	if keep := HandleMaxRunes - utf8.RuneCountInString(suffix); len(runes) > keep {
		runes = runes[:keep]
	}
	return strings.TrimRight(string(runes), "._-") + suffix
}

// PersonDisplayName is the one rule for the name people see for a member: on
// their messages, in a notification or an email, as a comment's author, in
// search results. It is their display name (Dgraph user_name) when they have
// one, else their full name (user_full_name), else their address's part
// before the @, else "". Each is trimmed. The web app applies the same rule.
//
// Places used to choose for themselves: most showed the display name, some
// the full name first (an urgent ping, a project update, a booking page),
// and none fell back to the address, so someone with no display name was
// shown as nobody. Pure.
func PersonDisplayName(displayName, fullName, email string) string {
	if n := strings.TrimSpace(displayName); n != "" {
		return n
	}
	if n := strings.TrimSpace(fullName); n != "" {
		return n
	}
	email = strings.TrimSpace(email)
	if at := strings.IndexByte(email, '@'); at >= 0 {
		email = email[:at]
	}
	return strings.TrimSpace(email)
}
