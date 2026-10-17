package helpers

import (
	"bytes"
	"sort"
	"strings"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// The Markdown guard: whether goldmark may be asked to read some Markdown, as
// a chat reply (business/BotPost) or as a doc (business/ContentCodec).
//
// goldmark reads most Markdown in time linear in its length, but some in time
// that grows with the square of the length of one paragraph or one line,
// because of how it looks for what closes what. From its source (v1.8.6):
//   - each closing *, _ or ~ walks back over the delimiters before it in its
//     paragraph to find an opener (emphasis);
//   - each run of backticks looks through the rest of its paragraph for a run
//     as long (code spans);
//   - each <!--, <?, <!X or <![CDATA[ looks through the rest of its paragraph
//     for its end (raw HTML);
//   - each ]( reads a destination to a space or the line's end, then a title
//     to its closing quote or bracket; each ][ a label to its ] (links);
//   - each ] that closes a label walks and copies everything since its [
//     (brackets);
//   - each $` reads its line for closing ticks (doc maths);
//   - each space, *, _, ~ or ( makes linkify read the word after it as an
//     email address (chat);
//   - each link reference definition at a paragraph's start copies the lines
//     after it (references);
//   - and every level of nesting is held open line by line, each asking every
//     line for its indentation (MarkdownTooDeep bounds the depth; nesting is
//     its price below that).
//
// The guard prices that work from goldmark's own block structure, read with
// no inline parsers (linear once the nesting is bounded), in steps of about a
// nanosecond at load, and MarkdownTooCostly refuses Markdown priced past
// MaxMarkdownSteps, or holding a block of more than MaxInlineNodes inline
// nodes. Each price is an upper bound: it counts every candidate (an escaped
// character excepted) as goldmark would at worst, so text built to look cheap
// is never read as cheap. What is refused, the caller reads another way: a
// chat reply as its words, escaped; a doc through the API as a 400, an
// imported file as plain text.
//
// Measured 16 Oct 2026 on the build machine (32 threads, load 15 to 45),
// goldmark v1.8.6 on one paragraph of each of CommonMark's pathological
// inputs, before this guard:
//
//	input                          16 KB      64 KB      256 KB
//	"*a_ " repeated                54 ms      1.0 s      over 45 s
//	"a**b" + "c* " repeated        81 ms      1.5 s      over 45 s
//	"[a](b" repeated               59 ms      894 ms     14 s
//	"[a](<b" repeated              39 ms      267 ms     4.3 s
//	"[" × n, "a", "]" × n          87 ms      313 ms     1.5 s
//	">" × n, " a"                  164 ms     1.9 s      24 s
//	lists carried by indentation   50 ms      250 ms     1.4 s
//	link definitions               5 ms       61 ms      978 ms
//	"a_" repeated (chat linkify)   39 ms      2.6 s      -
//
// goldmark is at the newest v1; goldmark/v2 (v2.1.6, 27 Sep 2026) fixed three
// of these and made others worse ("_a " repeated at 256 KB: 70 ms in v1,
// over 45 s in v2), and changes its whole API.
//
// The weights are each kind of work's cost per unit at load, measured on
// those inputs and rounded up: a step is about a nanosecond, MaxMarkdownSteps
// about a tenth of a second past the plain read. With them, and the guard,
// the slowest of every case in tests/mdpathology at the largest size each is
// let through (business/ContentCodec md_cost_test.go and business/BotPost
// markdown_cost_test.go, which hold these bounds; load 50 to 90):
//
//	the doc reader (FromMarkdown)                   chat (renderMarkdown, 16 KB at most)
//	backticks, cmark's (105 KB)      86 ms          "a**b" + "c* " repeated (11 KB)   79 ms
//	"[a](b" repeated (10 KB)         69 ms          nested lists (15 KB)              35 ms
//	"a**b" + "c* " repeated (11 KB)  41 ms          "[a](<b" repeated (8 KB)          31 ms
//	nested brackets (4 KB)           39 ms          nested brackets (4 KB)            30 ms
//	nested emphasis (14 KB)          38 ms
//
// A real 385 KB doc (this repository's own Markdown) is priced at half a
// percent of MaxMarkdownSteps (markdownCost_test.go holds it under a tenth),
// and reads in about a tenth of a second at load.

// MarkdownProfile is a goldmark set-up the guard prices: what reads the text.
type MarkdownProfile int

const (
	// ChatMarkdown is business/BotPost's: CommonMark with strikethrough and
	// linkify.
	ChatMarkdown MarkdownProfile = iota + 1
	// DocMarkdown is business/ContentCodec's: CommonMark with tables, task
	// lists, ~~strikethrough~~ and $`maths`$.
	DocMarkdown
)

// MaxMarkdownSteps is the most work past a plain read the guard lets goldmark
// do for one piece of Markdown: about a tenth of a second at load.
const MaxMarkdownSteps = 100_000_000

// MaxInlineNodes is the most inline nodes (brackets, runs of backticks and of
// delimiters, < and $`) one paragraph, heading or table cell may make. Each
// is linear work, but heavy: goldmark and the doc reader take up to 16 µs at
// load for each, so a paragraph of 64,000 took most of a second. A paragraph
// of real text holds a few dozen; a doc may hold any number of paragraphs.
const MaxInlineNodes = 4096

// Steps per unit of each kind of work.
const (
	stepsPerNestedLevel   = 60 // a line's quotes and lists, squared: each held open is asked of the line
	stepsPerDelimiterPair = 4  // delimiter runs in a paragraph, squared
	stepsPerCodeByte      = 3  // bytes a run of backticks looks through
	stepsPerHTMLByte      = 3  // bytes a <!--, <?, <!X or <![CDATA[ looks through
	stepsPerLinkByte      = 8  // bytes a ]( or ][ reads
	stepsPerBracketByte   = 10 // bytes a closing ] walks back over
	stepsPerMathByte      = 3  // bytes a $` reads for its closing ticks
	stepsPerLinkifyByte   = 3  // bytes linkify reads after a space, *, _, ~ or (
	stepsPerReferenceLine = 4  // lines a link definition copies
)

// MarkdownTooCostly says whether goldmark should not be asked to read md as
// p: too deep (MarkdownTooDeep), a block with more than MaxInlineNodes inline
// nodes, or more work than MaxMarkdownSteps.
func MarkdownTooCostly(md string, p MarkdownProfile) bool {
	if MarkdownTooDeep(md) {
		return true
	}
	w := markdownWorkOf(md, p)
	return w.crowded > MaxInlineNodes || w.steps() > MaxMarkdownSteps
}

// markdownWorkOf is the units of each kind of work reading md as p takes (its
// steps, goldmark's work past a plain read). md must not be too deep
// (MarkdownTooDeep): its blocks are read to price it.
func markdownWorkOf(md string, p MarkdownProfile) markdownWork {
	src := []byte(md)
	root := probeFor(p).Parse(text.NewReader(src))
	w := markdownWork{nesting: nestingWork(md)}
	var t inlineText // one block's text at a time, its room kept from block to block
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || n.Type() != ast.TypeBlock || n.IsRaw() || n.Lines().Len() == 0 {
			return ast.WalkContinue, nil
		}
		w.add(src, n.Lines(), n.Kind() == ast.KindParagraph || n.Kind() == ast.KindTextBlock, p, &t)
		return ast.WalkSkipChildren, nil
	})
	return w
}

// nestingWork is goldmark's work holding md's quotes and lists open: each
// line asks every block open around it whether it goes on, reading the line's
// indentation for each, so a line deep d is about d squared. Linear in md.
func nestingWork(md string) int64 {
	open, work := 0, int64(0)
	for len(md) > 0 {
		line := md
		if i := strings.IndexAny(md, "\n\r"); i >= 0 {
			line, md = md[:i], md[i+1:]
		} else {
			md = ""
		}
		depth := lineDepth(line, open)
		work += int64(depth) * int64(depth)
		open = max(open, depth)
	}
	return work
}

// The probes read blocks as each profile's parser does, and nothing inline:
// no inline parsers, and no link reference definitions taken out of
// paragraphs (their copying is priced instead).
var (
	chatProbe = parser.NewParser(parser.WithBlockParsers(parser.DefaultBlockParsers()...))
	docProbe  = parser.NewParser(parser.WithBlockParsers(parser.DefaultBlockParsers()...),
		parser.WithParagraphTransformers(util.Prioritized(extension.NewTableParagraphTransformer(), 200)))
)

func probeFor(p MarkdownProfile) parser.Parser {
	if p == DocMarkdown {
		return docProbe
	}
	return chatProbe
}

// markdownWork is the units of each kind of work found.
type markdownWork struct {
	// crowded is the most inline nodes one block makes (MaxInlineNodes).
	crowded                                                                      int64
	nesting, delimiters, code, html, links, brackets, maths, linkify, references int64
}

func (w markdownWork) steps() int64 {
	return w.nesting*stepsPerNestedLevel + w.delimiters*stepsPerDelimiterPair + w.code*stepsPerCodeByte +
		w.html*stepsPerHTMLByte + w.links*stepsPerLinkByte + w.brackets*stepsPerBracketByte +
		w.maths*stepsPerMathByte + w.linkify*stepsPerLinkifyByte + w.references*stepsPerReferenceLine
}

// add prices one block whose text goldmark reads inline (a paragraph, a
// list item's text, a heading, a table cell), from its lines as the inline
// parsers get them; paragraph says link definitions are taken from its start.
func (w *markdownWork) add(src []byte, lines *text.Segments, paragraph bool, p MarkdownProfile, t *inlineText) {
	definitions := 0
	var b []byte
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		v := seg.Value(src)
		if lines.Len() == 1 {
			b = v
		} else {
			b = append(b, v...)
		}
		if trimmed := bytes.TrimLeft(v, " "); len(v)-len(trimmed) <= 3 && len(trimmed) > 0 && trimmed[0] == '[' {
			definitions++
		}
	}
	if paragraph {
		w.references += int64(definitions) * int64(lines.Len())
	}
	t.reset(b)
	w.crowded = max(w.crowded, t.nodes())
	w.delimiters += t.delimiterPairs()
	w.code += t.codeSpans()
	w.html += t.rawHTML()
	w.links += t.links()
	w.brackets += t.brackets()
	if p == DocMarkdown {
		w.maths += t.maths()
	}
	if p == ChatMarkdown {
		w.linkify += t.linkify()
	}
}

// inlineText is one block's text, with what the prices look up in it. Its
// slices are kept from block to block, so a doc of many small blocks (a
// table's cells) is priced without allocating for each.
type inlineText struct {
	b       []byte
	escaped []bool  // a punctuation character after a backslash that isn't itself escaped
	lineEnd []int32 // where the line holding each byte ends (len(b) past the end)
	wordEnd []int32 // linkify's: the next space, tab or line end
}

// reset makes t the text b.
func (t *inlineText) reset(b []byte) {
	n := len(b)
	t.b = b
	t.escaped = grow(t.escaped, n)
	t.lineEnd = grow(t.lineEnd, n+1)
	for i := 1; i < n; i++ {
		t.escaped[i] = b[i-1] == '\\' && !t.escaped[i-1] && util.IsPunct(b[i])
	}
	if n > 0 {
		t.escaped[0] = false
	}
	end := n
	t.lineEnd[n] = int32(n)
	for i := n - 1; i >= 0; i-- {
		if b[i] == '\n' || b[i] == '\r' {
			end = i
		}
		t.lineEnd[i] = int32(end)
	}
}

// grow is s with room for n, its contents not kept.
func grow[E any](s []E, n int) []E {
	if cap(s) < n {
		return make([]E, n)
	}
	return s[:n]
}

// is says whether b[i] is the character c, unescaped.
func (t *inlineText) is(i int, c byte) bool {
	return i >= 0 && i < len(t.b) && t.b[i] == c && !t.escaped[i]
}

// unescaped is where c occurs unescaped, in order.
func (t *inlineText) unescaped(c byte) []int {
	var at []int
	for i, x := range t.b {
		if x == c && !t.escaped[i] {
			at = append(at, i)
		}
	}
	return at
}

// nodes is how many inline nodes goldmark may make of the block, each a
// fixed price however cheap the rest of the read: every unescaped bracket and
// <, every run of backticks, and every delimiter run (*, _ or ~ repeated).
func (t *inlineText) nodes() int64 {
	n := int64(0)
	for i, c := range t.b {
		switch {
		case t.escaped[i]:
		case c == '[' || c == ']' || c == '<':
			n++
		case (c == '`' || c == '*' || c == '_' || c == '~') && (i == 0 || t.b[i-1] != c || t.escaped[i-1]):
			n++
		}
	}
	return n
}

// delimiterPairs is the walks goldmark's emphasis and strikethrough may make:
// every delimiter run (*, _ or ~ repeated) may walk back over every other, so
// the runs in the block, squared.
func (t *inlineText) delimiterPairs() int64 {
	runs := int64(0)
	for i, c := range t.b {
		if (c == '*' || c == '_' || c == '~') && !t.escaped[i] && (i == 0 || t.b[i-1] != c || t.escaped[i-1]) {
			runs++
		}
	}
	return runs * runs
}

// backtickRuns calls each with every run of backticks, last first: where it
// starts, how long it is, and where the next run (after it) as long as it,
// and as long as it less one, start (len(b) for none).
func (t *inlineText) backtickRuns(each func(at, n, nextSame, nextShorter int)) {
	next := map[int]int{} // run length -> where the next run that long starts
	end := len(t.b)
	for i := len(t.b) - 1; i >= 0; {
		if t.b[i] != '`' {
			i--
			continue
		}
		j := i
		for j > 0 && t.b[j-1] == '`' {
			j--
		}
		n := i - j + 1
		same, ok := next[n]
		if !ok {
			same = end
		}
		shorter, ok := next[n-1]
		if !ok {
			shorter = end
		}
		each(j, n, same, shorter)
		next[n] = j
		i = j - 1
	}
}

// codeSpans is the bytes goldmark's code span parser looks through: from
// each run of backticks (but an escaped first one) to the next run exactly
// as long, or to the end.
func (t *inlineText) codeSpans() int64 {
	work := int64(0)
	t.backtickRuns(func(at, n, nextSame, nextShorter int) {
		switch {
		case !t.escaped[at]:
			work += int64(nextSame - at)
		case n > 1:
			work += int64(nextShorter - (at + 1))
		}
	})
	return work
}

// maths is the bytes the doc reader's $`…`$ looks through: from each $` to
// the next run of backticks as long as its own, or its line's end.
func (t *inlineText) maths() int64 {
	work := int64(0)
	t.backtickRuns(func(at, n, nextSame, _ int) {
		if t.is(at-1, '$') {
			work += int64(min(nextSame, int(t.lineEnd[at])) - (at - 1))
		}
	})
	return work
}

// rawHTML is the bytes looked through from each <!--, <?, <![CDATA[ and <!X
// to its end, or to the end.
func (t *inlineText) rawHTML() int64 {
	if !bytes.Contains(t.b, []byte("<!")) && !bytes.Contains(t.b, []byte("<?")) {
		return 0
	}
	work := int64(0)
	for _, k := range [][2]string{{"<!--", "-->"}, {"<?", "?>"}, {"<![CDATA[", "]]>"}} {
		closers := occurrences(t.b, []byte(k[1]))
		for _, at := range occurrences(t.b, []byte(k[0])) {
			if !t.escaped[at] {
				work += int64(nextAt(closers, at+len(k[0]), len(t.b)) - at)
			}
		}
	}
	var gts []int
	read := false
	for i := 0; i+2 < len(t.b); i++ {
		if t.b[i] == '<' && !t.escaped[i] && t.b[i+1] == '!' && t.b[i+2] >= 'A' && t.b[i+2] <= 'Z' {
			if !read {
				gts, read = occurrences(t.b, []byte(">")), true
			}
			work += int64(nextAt(gts, i+3, len(t.b)) - i)
		}
	}
	return work
}

// links is the bytes each ]( reads, a destination to a space or its line's
// end (to a > for <…>), then a title to its closing quote or bracket, and
// each ][ to the next [ or ].
func (t *inlineText) links() int64 {
	b, n := t.b, len(t.b)
	if !bytes.Contains(b, []byte("](")) && !bytes.Contains(b, []byte("][")) {
		return 0
	}
	// Where each character the reads stop at occurs, unescaped, read the
	// first time a read needs it.
	seen := map[byte][]int{}
	where := func(c byte) []int {
		at, ok := seen[c]
		if !ok {
			at = t.unescaped(c)
			seen[c] = at
		}
		return at
	}
	var spaces []int32 // the next space or tab at or after each byte
	nextSpace := func(i int) int {
		if spaces == nil {
			spaces = make([]int32, n+1)
			spaces[n] = int32(n)
			for j := n - 1; j >= 0; j-- {
				if b[j] == ' ' || b[j] == '\t' {
					spaces[j] = int32(j)
				} else {
					spaces[j] = spaces[j+1]
				}
			}
		}
		return int(spaces[i])
	}
	work := int64(0)
	for i := 0; i+1 < n; i++ {
		if !t.is(i, ']') {
			continue
		}
		switch {
		case t.is(i+1, '['):
			work += int64(min(nextAt(where('['), i+2, n), nextAt(where(']'), i+2, n)) - i)
		case t.is(i+1, '('):
			at := skipSpace(b, i+2)
			stop := int(t.lineEnd[at])
			if t.is(at, '<') {
				stop = min(stop, nextAt(where('>'), at+1, n))
			} else {
				stop = min(stop, nextSpace(at))
			}
			work += int64(stop - i)
			title := skipSpace(b, stop)
			switch {
			case t.is(title, '"'):
				work += int64(nextAt(where('"'), title+1, n) - title)
			case t.is(title, '\''):
				work += int64(nextAt(where('\''), title+1, n) - title)
			case t.is(title, '('):
				work += int64(min(nextAt(where('('), title+1, n), nextAt(where(')'), title+1, n)) - title)
			}
		}
	}
	return work
}

// brackets is the bytes the closing ]s walk back over and copy: each closes
// one label, walking back to its [ (goldmark's containsLink and the label's
// copy). They are paired first opened, first closed: of every way to pair
// them, the one that walks farthest, so no pairing goldmark makes (a [ in a
// code span is no label, a ] in a link's address closes none) walks more.
// Pure.
func (t *inlineText) brackets() int64 {
	var open []int // the [s not paired yet, from head on
	head := 0
	work := int64(0)
	for i, c := range t.b {
		if t.escaped[i] {
			continue
		}
		switch c {
		case '[':
			open = append(open, i)
		case ']':
			if head < len(open) {
				work += int64(i - open[head])
				head++
			}
		}
	}
	return work
}

// linkify is the bytes chat's linkify reads, as a possible email address (a
// URL it reads once, and then past): the word after each of its triggers (a
// space, *, _, ~ or (), at each line's start, and after every inline node,
// since goldmark asks the space's parsers again wherever a node ends. A node
// ends after a run of backticks or delimiters, a ], a ), a > or a [ (a link
// label opening). Pure.
func (t *inlineText) linkify() int64 {
	b, n := t.b, len(t.b)
	t.wordEnd = grow(t.wordEnd, n+1)
	wordEnd := t.wordEnd
	wordEnd[n] = int32(n)
	for i := n - 1; i >= 0; i-- {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			wordEnd[i] = int32(i)
		default:
			wordEnd[i] = wordEnd[i+1]
		}
	}
	work := int64(0)
	for i := 0; i < n; i++ {
		if i == 0 || linkifyAfter(b[i-1]) {
			work += int64(wordEnd[i]) - int64(i)
		}
	}
	return work
}

// linkifyAfter says whether linkify reads the word after c: a trigger, a line
// end, or what an inline node ends with.
func linkifyAfter(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '*', '_', '~', '(', '`', ']', ')', '>', '[':
		return true
	}
	return false
}

// skipSpace is the first byte at or after i that isn't a space, a tab or a
// line end.
func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// occurrences is where s starts in b, in order.
func occurrences(b, s []byte) []int {
	var at []int
	for from := 0; ; {
		i := bytes.Index(b[from:], s)
		if i < 0 {
			return at
		}
		at = append(at, from+i)
		from += i + 1
	}
}

// nextAt is the first of at (in order) at or after from, else end.
func nextAt(at []int, from, end int) int {
	k := sort.SearchInts(at, from)
	if k == len(at) {
		return end
	}
	return at[k]
}
