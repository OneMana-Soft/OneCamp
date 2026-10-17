package helpers

import (
	"runtime"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/tests/mdpathology"
	"github.com/akashc777/OneCamp/tests/timing"
)

// Each kind of work is priced as goldmark does it, on text small enough to
// count by hand.
func TestMarkdownWorkCountsEachKind(t *testing.T) {
	for _, c := range []struct {
		md   string
		p    MarkdownProfile
		want markdownWork
	}{
		// Two delimiter runs of each of * and _: four runs, sixteen walks at most.
		{"*a* _b_", DocMarkdown, markdownWork{crowded: 4, delimiters: 16}},
		// An escaped * is no delimiter, and the next one starts a run.
		{`\**a*`, DocMarkdown, markdownWork{crowded: 2, delimiters: 4}},
		// Backticks: the first run looks to the next as long (2 bytes), the
		// second to the end (3).
		{"`a` x", DocMarkdown, markdownWork{crowded: 2, code: 5}},
		// An unclosed comment looks to the end of its paragraph.
		{"a <!-- b", DocMarkdown, markdownWork{crowded: 1, html: 6}},
		// ]( reads its destination to the space (3 bytes from the ]). A line
		// that starts with [ may be a link definition: priced as one.
		{"[a](b c)", DocMarkdown, markdownWork{crowded: 2, links: 3, brackets: 2, references: 1}},
		// Each ] walks back to a [, paired first opened, first closed.
		{"[a] [b]", DocMarkdown, markdownWork{crowded: 4, brackets: 2 + 2, references: 1}},
		{"[[a]]", DocMarkdown, markdownWork{crowded: 4, brackets: 3 + 3, references: 1}},
		{"a] [b", DocMarkdown, markdownWork{crowded: 2}},
		// $` looks for its closing ticks, on its line (3 bytes from the $).
		{"$`x` y", DocMarkdown, markdownWork{crowded: 2, code: 5, maths: 3}},
		// Chat's linkify reads the word at the line's start and after the space.
		{"a bc", ChatMarkdown, markdownWork{linkify: 1 + 2}},
		// Two link definitions at a paragraph's start, of three lines.
		{"[a]: u\n[b]: v\nx", DocMarkdown, markdownWork{crowded: 4, brackets: 2 + 2, references: 2 * 3}},
		// A code block is read raw: nothing in it is priced.
		{"```\n*a_ *a_ [a](b\n```", DocMarkdown, markdownWork{}},
		// Nesting: a line one list deep, then one two deep, 1 + 4.
		{"- a\n  - b", DocMarkdown, markdownWork{nesting: 1 + 4}},
	} {
		if got := markdownWorkOf(c.md, c.p); got != c.want {
			t.Errorf("%q as %d: %+v, want %+v", c.md, c.p, got, c.want)
		}
	}
}

// fastest is timing.Fastest after a collection: what the cases before left
// is no one's time.
func fastest(runs int, f func()) time.Duration {
	runtime.GC()
	return timing.Fastest(runs, f)
}

// The guard itself is linear on every slow case, CommonMark's and goldmark's:
// from 64 KB to 256 KB its time grows as a linear read's does (four times,
// with ten allowed for a loaded machine; a quadratic read's grows sixteen),
// each size timed back to back with the other (timing.Ratio), with its own
// time on ordinary prose on this machine as the floor, never a fixed number
// of milliseconds. Timed apart, the fastest of five of each, two cases grew
// past ten times at a load of 55 (unit-4.log, 16 Oct 2026).
func TestTheMarkdownGuardIsLinear(t *testing.T) {
	const small, large = 64 << 10, 256 << 10
	prose := mdpathology.Prose(small)
	for _, p := range []MarkdownProfile{ChatMarkdown, DocMarkdown} {
		floor := fastest(3, func() { MarkdownTooCostly(prose, p) })
		for _, c := range mdpathology.Cases() {
			s, l := c.Make(small), c.Make(large)
			if grew := timing.Ratio(5, floor, func() { MarkdownTooCostly(s, p) }, func() { MarkdownTooCostly(l, p) }); grew > 10 {
				t.Errorf("profile %d, %s: the guard's time grew %.1f times from %d KB to %d KB (prose: %v)",
					p, c.Name, grew, len(s)>>10, len(l)>>10, floor)
			}
		}
	}
}

// A large real doc, this repository's own Markdown, is priced at a small part
// of the guard's most: under a tenth (it was half a percent, 16 Oct 2026), so
// ten times as much real Markdown in one doc is still read.
func TestARealDocIsPricedAtASmallPartOfTheMost(t *testing.T) {
	md, err := mdpathology.RealDoc(300 << 10)
	if err != nil {
		t.Fatal(err)
	}
	w := markdownWorkOf(md, DocMarkdown)
	if w.steps() > MaxMarkdownSteps/10 || MarkdownTooCostly(md, DocMarkdown) {
		t.Fatalf("a real %d KB doc is priced at %d steps (%+v), past a tenth of the most (%d), or refused",
			len(md)>>10, w.steps(), w, MaxMarkdownSteps)
	}
	t.Logf("a real %d KB doc is priced at %d steps", len(md)>>10, w.steps())
}

// Every case goldmark reads in more than linear time (measured 16 Oct 2026,
// logs/chatmd-depth/goldmark-v1-v2.log and calibration-3.log) is refused at
// 256 KB, as a doc and as a chat reply.
func TestTheMarkdownGuardRefusesTheSlowCases(t *testing.T) {
	both := []MarkdownProfile{ChatMarkdown, DocMarkdown}
	chat := []MarkdownProfile{ChatMarkdown} // slow through linkify, which docs don't run
	slow := map[string][]MarkdownProfile{
		"mismatched openers and closers": both, "openers and closers multiple of 3": both,
		"unclosed links A": both, "unclosed links B": both, "nested brackets": both,
		"nested block quotes": both, "deeply nested lists": both, "empty lines in deeply nested lists": both,
		"lists carried by indentation": both, "many references": both, "many link openers with no closers": both,
		"comments unclosed in a paragraph": both, "processing instructions unclosed": both,
		"email-like words after delimiters": chat, "maths unclosed": chat, "code spans after nodes": chat,
	}
	seen := 0
	for _, c := range mdpathology.Cases() {
		profiles, ok := slow[c.Name]
		if !ok {
			continue
		}
		seen++
		md := c.Make(256 << 10)
		for _, p := range profiles {
			if !MarkdownTooCostly(md, p) {
				t.Errorf("%s at %d KB is let through as profile %d: %+v", c.Name, len(md)>>10, p, markdownWorkOf(md, p))
			}
		}
	}
	if seen != len(slow) {
		t.Fatalf("%d of the %d slow cases are in mdpathology", seen, len(slow))
	}
}
