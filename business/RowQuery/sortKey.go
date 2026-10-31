package business

// The tables' natural sort key (coordinator, 21 Oct 2026): one definition,
// written once here, for the cell index's sort column (s) and the evaluator's
// sorts, and once in TypeScript (the web's lib/tables/sortKey.ts), for the
// browser's, each held to testdata/sortkey/natural.json, which both
// repositories keep byte for byte. A column never changes its order because
// its table passed the 500 rows the browser sorts itself, and "Row 2" comes
// before "Row 10".
//
// The key: the text in NFKD with its combining marks (Mn) dropped; folded to
// one case (lower case, rune by rune, then ß as ss, ς as σ, and Cherokee small
// letters as capitals, which is where Unicode folds them); trimmed of
// whitespace at both ends; each run of ASCII digits read as a number, its
// leading zeros dropped and zero-padded to 20 digits, or past 20 written as
// ':', its length in two digits (99 at most) and the run, so it sorts after
// every padded number; and cut to its first 256 code points, so its index
// entry always fits (Postgres refuses a btree entry over 2,704 bytes, and a
// long text's would be one). Keys compare byte by byte, as Go compares
// strings and Postgres's C collation compares text: UTF-8's byte order is
// code point order.
//
// JavaScript's toLowerCase differs from a rune's own lower case only for İ,
// which NFKD has already split into I and a mark, and for a final Σ, which
// the fold makes σ either way. Both sides read Unicode 15.0 (Go's unicode and
// x/text's norm tables; Node's ICU 73); a character a later version assigned
// may fold differently in a newer browser.

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// digitsPad is the width every run of digits is padded to.
const digitsPad = 20

// keyRunes is the most code points a key keeps.
const keyRunes = 256

// keyEdges is the whitespace trimmed from a key's ends, one list in both
// languages: ASCII's, NEL, the Ogham space, the line and paragraph separators
// and the byte-order mark (NFKD makes the other spaces a plain one).
const keyEdges = "\t\n\v\f\r \u0085\u1680\u2028\u2029\ufeff"

// naturalKey is text's natural sort key. Pure.
func naturalKey(text string) string {
	var folded strings.Builder
	folded.Grow(len(text))
	for _, r := range norm.NFKD.String(text) {
		if r < utf8.RuneSelf {
			if 'A' <= r && r <= 'Z' {
				r += 'a' - 'A'
			}
			folded.WriteByte(byte(r))
			continue
		}
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		switch r = unicode.ToLower(r); {
		case r == 'ß':
			folded.WriteString("ss")
		case r == 'ς':
			folded.WriteRune('σ')
		case 0xab70 <= r && r <= 0xabbf: // Cherokee small letters, to U+13A0..U+13EF
			folded.WriteRune(r - 0xab70 + 0x13a0)
		case 0x13f8 <= r && r <= 0x13fd: // to U+13F0..U+13F5
			folded.WriteRune(r - 8)
		default:
			folded.WriteRune(r)
		}
	}
	s := strings.Trim(folded.String(), keyEdges)
	var key strings.Builder
	key.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] < '0' || s[i] > '9' {
			// A digit is one byte, and no byte of a longer UTF-8 sequence is one.
			key.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && '0' <= s[j] && s[j] <= '9' {
			j++
		}
		writeRun(&key, s[i:j])
		i = j
	}
	return cutRunes(key.String(), keyRunes)
}

// writeRun writes a run of digits as it sorts: by its value, padded to
// digitsPad; past it, ':' and its length first.
func writeRun(b *strings.Builder, run string) {
	digits := strings.TrimLeft(run, "0")
	if digits == "" {
		digits = "0"
	}
	if len(digits) <= digitsPad {
		b.WriteString(strings.Repeat("0", digitsPad-len(digits)))
		b.WriteString(digits)
		return
	}
	n := min(len(digits), 99)
	b.WriteByte(':')
	b.WriteByte(byte('0' + n/10))
	b.WriteByte(byte('0' + n%10))
	b.WriteString(digits)
}

// cutRunes is s's first n code points.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s // fewer bytes than n: fewer code points too
	}
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}
