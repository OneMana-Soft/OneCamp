package dgraphquery

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// trigramMin is the fewest literal characters a regexp needs before Dgraph
// will answer it from a trigram index: with fewer, a root regexp fails
// ("regular expression is too wide-ranging").
const trigramMin = 3

// TrigramReady reports whether an escaped search term (EscapeRegexLiteral or
// regexp.QuoteMeta output, the form written into /.*term.*/i) holds enough
// literal characters for a root regexp to use the trigram index.
func TrigramReady(escaped string) bool {
	return utf8.RuneCountInString(strings.ReplaceAll(escaped, `\`, "")) >= trigramMin
}

// RegexpUnionRoot is where a search for an escaped term in any of preds (each
// with a trigram index) starts: var blocks naming each predicate's matches,
// and a root function over their union, so the query reads only the nodes
// that match instead of every node that has the fallback predicate. A term too
// short for the index keeps the fallback root (as in "has(user_uuid)") and no
// var blocks. Either way the caller keeps its full match in @filter: the union
// holds every node the filter can keep, so what comes back is the same. extra
// is more var names to fold into the union (a block the caller writes itself),
// or none. prefix keeps the var names apart from the caller's.
func RegexpUnionRoot(escaped, fallback, prefix string, preds []string, extra ...string) (vars, root string) {
	if !TrigramReady(escaped) || len(preds) == 0 {
		return "", fallback
	}
	var b strings.Builder
	names := make([]string, 0, len(preds)+len(extra))
	for i, p := range preds {
		name := fmt.Sprintf("%s%d", prefix, i)
		fmt.Fprintf(&b, "%s as var(func: regexp(%s, /.*%s.*/i))\n", name, p, escaped)
		names = append(names, name)
	}
	names = append(names, extra...)
	return b.String(), "uid(" + strings.Join(names, ", ") + ")"
}
