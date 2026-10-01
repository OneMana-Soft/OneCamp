package helpers

// hasFormatVerb reports whether s contains a printf verb — a '%' that introduces a conversion
// rather than an escaped literal percent.
//
// This decides whether LogWithContext renders a message against its args or leaves it alone, and
// it has to be conservative in both directions:
//
//   - Miss a verb and the message ships with a literal "%+v" in it, which is the bug this was
//     written to fix.
//   - See a verb where there is none and fmt.Sprintf appends "%!(EXTRA ...)" to a message that
//     was fine, so a site that passes structured args without a template gets mangled.
//
// "%%" is an escaped percent, not a verb: "100%% done" with an arg beside it should keep its args
// attribute rather than be Sprintf'd. A trailing "%" is likewise not a verb — it introduces
// nothing — and Sprintf would turn it into "%!(NOVERB)".
func hasFormatVerb(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			continue
		}
		if i+1 >= len(s) {
			return false // a trailing '%' introduces nothing
		}
		if s[i+1] == '%' {
			i++ // escaped literal percent; skip the pair
			continue
		}
		return true
	}
	return false
}
