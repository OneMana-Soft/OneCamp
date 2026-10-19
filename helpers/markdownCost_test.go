package helpers

import (
	"runtime"
	"strings"
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

// letThrough is the largest size, in KB to 256, at which the guard lets each
// case in tests/mdpathology through, read as a chat reply and as a doc: 0 is
// refused at 1 KB, 256 never refused. The budget tests read each case at
// exactly this size, back to back with a real doc or reply, and it reads
// within its budget at a load of 70 (business/ContentCodec
// TestWhatTheGuardLetsThroughReadsWithinABudget, business/BotPost
// TestWhatTheGuardLetsThroughRendersWithinABudget; logs/chatmd-depth/
// tenx-load70.log, 16 Oct 2026). They leave out a size of 0 or 256, read as
// linear by the growth tests, and chat's from 16 KB, its own cap. Measured
// again whenever a price, MaxMarkdownSteps or MaxInlineNodes moves.
var letThrough = map[string]struct{ chat, doc int }{
	"nested strong emph":                               {14, 14},
	"many emph closers with no openers":                {12, 12},
	"many emph openers with no closers":                {12, 12},
	"many link closers with no openers":                {8, 8},
	"many link openers with no closers":                {8, 8},
	"mismatched openers and closers":                   {8, 8},
	"openers and closers multiple of 3":                {11, 11},
	"link openers and emph closers":                    {8, 8},
	"pattern [ (]( repeated":                           {10, 10},
	"pattern ![[]() repeated":                          {6, 8},
	"hard link/emph case":                              {11, 11},
	"nested brackets":                                  {4, 4},
	"nested block quotes":                              {0, 0},
	"deeply nested lists":                              {16, 16},
	"U+0000 in input":                                  {256, 256},
	"backticks":                                        {7, 105},
	"unclosed links A":                                 {8, 8},
	"unclosed links B":                                 {7, 10},
	"unclosed <!--":                                    {15, 15},
	"empty lines in deeply nested lists":               {0, 0},
	"empty lines in deeply nested lists in blockquote": {0, 0},
	"nested invalid link references":                   {4, 4},
	"many references":                                  {11, 11},
	"lists carried by indentation":                     {0, 0},
	"processing instructions unclosed":                 {8, 8},
	"declarations unclosed":                            {12, 12},
	"CDATA unclosed":                                   {12, 12},
	"comments unclosed in a paragraph":                 {15, 15},
	"link titles unclosed":                             {16, 16},
	"reference labels unclosed":                        {5, 5},
	"maths unclosed":                                   {10, 10},
	"code spans after nodes":                           {9, 15},
	"email-like words after delimiters":                {7, 8},
	"www after spaces":                                 {256, 256},
	"dotted words":                                     {256, 256},
	"many emphasised words":                            {8, 8},
	"many links":                                       {14, 14},
	"a long table":                                     {13, 256},
	"strikethrough":                                    {16, 16},
}

// The guard lets each slow case through to the size its reading was measured
// at (letThrough), and refuses it a kilobyte past. What the guard lets through
// is its own count of goldmark's work against its calibrated prices, so this
// needs no clock. A price halved, or MaxMarkdownSteps or MaxInlineNodes
// doubled, lets through what costs up to twice as much to read, and moves the
// cases it governs past their size here, or perKind's blocks past their count
// (logs/chatmd-depth/mutant-cost-every.log: all eleven such mutants fail). The
// budget tests, which time the reading, hold the guard's most (a tenth of a
// second) with room for a loaded machine, and so pass a doubled cost
// (mutant-double-steps.log).
func TestEachSlowCaseIsLetThroughToTheSizeItWasReadAt(t *testing.T) {
	largest := func(c mdpathology.Case, p MarkdownProfile) int {
		lo, hi := 0, 256
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if MarkdownTooCostly(c.Make(mid<<10), p) {
				hi = mid - 1
			} else {
				lo = mid
			}
		}
		return lo
	}
	cases := mdpathology.Cases()
	if len(cases) != len(letThrough) {
		t.Errorf("mdpathology has %d cases and letThrough %d: measure the new ones' reading and add them", len(cases), len(letThrough))
	}
	for _, c := range cases {
		want, ok := letThrough[c.Name]
		if !ok {
			t.Errorf("%s: not in letThrough", c.Name)
			continue
		}
		for _, x := range []struct {
			p    MarkdownProfile
			want int
		}{{ChatMarkdown, want.chat}, {DocMarkdown, want.doc}} {
			letAt := x.want == 0 || !MarkdownTooCostly(c.Make(x.want<<10), x.p)
			refusedPast := x.want == 256 || MarkdownTooCostly(c.Make((x.want+1)<<10), x.p)
			if !letAt || !refusedPast {
				got := largest(c, x.p)
				t.Errorf("profile %d, %s: let through to %d KB, read at %d KB (%+v at %d KB)",
					x.p, c.Name, got, x.want, markdownWorkOf(c.Make(got<<10), x.p), got)
			}
		}
	}
}

// perKind is a block for each kind of work the guard prices, small enough that
// only its price stops it (under MaxInlineNodes and MaxMarkdownNesting), and
// how many of them, as separate blocks, the guard lets through as a chat reply
// and as a doc (to 64): each kind's price against MaxMarkdownSteps. Many of
// mdpathology's cases stop at MaxInlineNodes first, so these are what halving
// one price moves. Read once as docs at those counts, back to back with a
// real doc, each kind's blocks took at most 0.34 of the budget
// business/ContentCodec gives what the guard lets through
// (logs/chatmd-depth/perkind-reading.log, 16 Oct 2026; linkify is chat's
// alone).
var perKind = []struct {
	kind      string
	block     string
	chat, doc int
}{
	{"nesting", nestedList(100), 4, 4},
	{"delimiters", strings.Repeat("*a_ ", 1000), 6, 6},
	{"code", growingTickRuns("e", "", 300), 0, 3},
	{"html", "a " + strings.Repeat("<!--", 2000), 4, 4},
	{"links", strings.Repeat("[a](b", 1000), 2, 4},
	{"brackets", strings.Repeat("[", 1000) + "a" + strings.Repeat("]", 1000), 6, 9},
	{"maths", growingTickRuns("$", "e", 250), 0, 3},
	{"linkify", strings.Repeat("a(", 2048), 7, 64},
	{"references", strings.Repeat("[a]: u\n", 2000) + "x", 6, 6},
}

// nestedList is a list nested depth deep, one item a level, then a paragraph.
func nestedList(depth int) string {
	var b strings.Builder
	for i := 0; i < depth; i++ {
		b.WriteString(strings.Repeat("  ", i) + "* a\n")
	}
	b.WriteString("\nx")
	return b.String()
}

// growingTickRuns is runs of 1 to n backticks, each after before and before
// after: every run a length of its own, so each looks to the end.
func growingTickRuns(before, after string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(before + strings.Repeat("`", i) + after)
	}
	return b.String()
}

// Each price is what stops its own kind of work: the guard lets through as
// many of each perKind block as it was counted at, and refuses one more. A
// price halved lets through more of its own blocks, about twice as many where
// it is the only price (logs/chatmd-depth/mutant-cost-every.log), and
// MaxMarkdownSteps doubled moves every count from 1 to 63.
func TestEachPriceIsWhatStopsItsOwnKindOfWork(t *testing.T) {
	blocks := func(block string, n int) string { return strings.Repeat(block+"\n\n", n) }
	for _, k := range perKind {
		for _, x := range []struct {
			p    MarkdownProfile
			want int
		}{{ChatMarkdown, k.chat}, {DocMarkdown, k.doc}} {
			letAt := x.want == 0 || !MarkdownTooCostly(blocks(k.block, x.want), x.p)
			refusedPast := x.want == 64 || MarkdownTooCostly(blocks(k.block, x.want+1), x.p)
			if !letAt || !refusedPast {
				got := 0
				for got < 64 && !MarkdownTooCostly(blocks(k.block, got+1), x.p) {
					got++
				}
				t.Errorf("profile %d, %s: %d blocks let through, counted at %d (%+v)",
					x.p, k.kind, got, x.want, markdownWorkOf(blocks(k.block, max(got, 1)), x.p))
			}
		}
	}
}
