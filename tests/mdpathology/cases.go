// Package mdpathology is Markdown that makes parsers slow, for the standing
// tests of the Markdown guard (helpers markdownCost.go) and the two goldmark
// entry points (business/BotPost's chat replies, business/ContentCodec's doc
// reader): CommonMark's pathological inputs (cmark's
// test/pathological_tests.py), and the ones found in goldmark v1.8.6 on
// 16 Oct 2026. Each is made at about a size, so a test reads it at two and
// sees how its cost grows.
package mdpathology

import (
	"fmt"
	"strings"
)

// Case is one kind of slow Markdown.
type Case struct {
	Name string
	// Make is the case at about size bytes.
	Make func(size int) string
}

// repeat is unit repeated to about size bytes.
func repeat(unit string, size int) string {
	return strings.Repeat(unit, max(1, size/len(unit)))
}

// Cases is every case, CommonMark's first.
func Cases() []Case {
	return []Case{
		{"nested strong emph", func(n int) string {
			k := max(1, n/14)
			return strings.Repeat("*a **a ", k) + "b" + strings.Repeat(" a** a*", k)
		}},
		{"many emph closers with no openers", func(n int) string { return repeat("a_ ", n) }},
		{"many emph openers with no closers", func(n int) string { return repeat("_a ", n) }},
		{"many link closers with no openers", func(n int) string { return repeat("a]", n) }},
		{"many link openers with no closers", func(n int) string { return repeat("[a", n) }},
		{"mismatched openers and closers", func(n int) string { return repeat("*a_ ", n) }},
		{"openers and closers multiple of 3", func(n int) string { return "a**b" + repeat("c* ", n) }},
		{"link openers and emph closers", func(n int) string { return repeat("[ a_", n) }},
		{"pattern [ (]( repeated", func(n int) string { return repeat("[ (](", n) }},
		{"pattern ![[]() repeated", func(n int) string { return repeat("![[]()", n) }},
		{"hard link/emph case", func(n int) string { return repeat("**x [a*b**c*](d) ", n) }},
		{"nested brackets", func(n int) string {
			k := max(1, n/2)
			return strings.Repeat("[", k) + "a" + strings.Repeat("]", k)
		}},
		{"nested block quotes", func(n int) string { return repeat("> ", n) + "a" }},
		{"deeply nested lists", func(n int) string {
			var b strings.Builder
			for i := 0; b.Len() < n; i++ {
				b.WriteString(strings.Repeat("  ", i) + "* a\n")
			}
			return b.String()
		}},
		{"U+0000 in input", func(n int) string { return repeat("abc\x00de\x00", n) }},
		{"backticks", func(n int) string {
			var b strings.Builder
			for i := 1; b.Len() < n; i++ {
				b.WriteString("e" + strings.Repeat("`", i))
			}
			return b.String()
		}},
		{"unclosed links A", func(n int) string { return repeat("[a](<b", n) }},
		{"unclosed links B", func(n int) string { return repeat("[a](b", n) }},
		{"unclosed <!--", func(n int) string { return "</" + repeat("<!--", n) }},
		{"empty lines in deeply nested lists", func(n int) string { return repeat("- ", n/2) + "a" + strings.Repeat("\n", max(1, n/4)) }},
		{"empty lines in deeply nested lists in blockquote", func(n int) string {
			return repeat("> - ", n/2) + "a" + strings.Repeat("\n>", max(1, n/4))
		}},
		{"nested invalid link references", func(n int) string {
			k := max(1, n/2)
			return strings.Repeat("[", k) + strings.Repeat("]", k)
		}},
		{"many references", func(n int) string {
			var b strings.Builder
			for i := 0; b.Len() < n/2; i++ {
				fmt.Fprintf(&b, "[%d]: /url\n", i)
			}
			return b.String() + repeat("[0] ", n/2)
		}},
		// Found in goldmark: nesting carried line to line by indentation, 128
		// list markers a line (past a per-line count of markers).
		{"lists carried by indentation", func(n int) string {
			var b strings.Builder
			for k := 0; b.Len() < n; k++ {
				b.WriteString(strings.Repeat(" ", 256*k) + strings.Repeat("- ", 128) + "x\n")
			}
			return b.String()
		}},
		{"processing instructions unclosed", func(n int) string { return "a " + repeat("<?", n) }},
		{"declarations unclosed", func(n int) string { return "a " + repeat("<!A", n) }},
		{"CDATA unclosed", func(n int) string { return "a " + repeat("<![CDATA[", n) }},
		{"comments unclosed in a paragraph", func(n int) string { return "a " + repeat("<!--", n) }},
		{"link titles unclosed", func(n int) string { return repeat("[a](b (x", n) }},
		{"reference labels unclosed", func(n int) string { return repeat("[a][", n) }},
		{"maths unclosed", func(n int) string { return repeat("$``a`", n) }},
		{"code spans after nodes", func(n int) string { return repeat("``a`", n) }},
		{"email-like words after delimiters", func(n int) string { return repeat("a_", n) }},
		{"www after spaces", func(n int) string { return repeat(" www.", n) }},
		{"dotted words", func(n int) string { return repeat("a.", n) }},
		{"many emphasised words", func(n int) string { return repeat("*a* ", n) }},
		{"many links", func(n int) string { return repeat("[a](b) ", n) }},
		{"a long table", func(n int) string { return "| a | b |\n| - | - |\n" + repeat("| *x* `y` [z](w) | **v** |\n", n) }},
		{"strikethrough", func(n int) string { return repeat("~~a ", n) }},
	}
}

// Prose is ordinary Markdown of about size bytes: paragraphs of words with
// some emphasis, code and links, what a plain read is measured on.
func Prose(size int) string {
	words := strings.Fields("the quick brown fox jumps over the lazy dog while a *careful* reader checks `code` and follows [a link](https://example.com/docs) to **the end** of the line")
	var b strings.Builder
	for i := 0; b.Len() < size; i++ {
		b.WriteString(words[i%len(words)])
		if i%40 == 39 {
			b.WriteString(".\n\n")
		} else {
			b.WriteString(" ")
		}
	}
	return b.String()
}
