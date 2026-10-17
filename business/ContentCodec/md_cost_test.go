package contentcodec

// The doc reader and the Markdown guard (helpers markdownCost.go), standing.
// Every slow case CommonMark and goldmark have (tests/mdpathology) is read by
// FromMarkdown in time that grows as its length does. What the guard lets
// through reads within the guard's price, at this machine's measured cost of
// a step. A large real doc reads, nothing refused. Every bound is measured on
// the machine the test runs on, against its own reading of other Markdown
// timed back to back with it (timing.Ratio), never a fixed number of
// milliseconds.

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/tests/mdpathology"
	"github.com/akashc777/OneCamp/tests/timing"
)

// read reads md once, failing the test when it takes past a minute: a case
// the guard should have refused, read whole.
func read(t *testing.T, md string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_, _ = FromMarkdown(md, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatalf("reading %d bytes (%.40q…) took past a minute", len(md), md)
	}
}

// readTime is the fastest of runs readings of md (timing.Fastest).
func readTime(t *testing.T, runs int, md string) time.Duration {
	t.Helper()
	runtime.GC()
	return timing.Fastest(runs, func() { read(t, md) })
}

// From 64 KB to 256 KB, reading each case grows as a linear read does (four
// times, with ten allowed for a loaded machine; a quadratic read grows
// sixteen), with the machine's reading of 64 KB of prose as the floor.
func TestReadingMarkdownGrowsAsItsLength(t *testing.T) {
	const small, large = 64 << 10, 256 << 10
	floor := readTime(t, 3, mdpathology.Prose(small))
	for _, c := range mdpathology.Cases() {
		s, l := c.Make(small), c.Make(large)
		if grew := timing.Ratio(3, floor, func() { read(t, s) }, func() { read(t, l) }); grew > 10 {
			t.Errorf("%s: reading grew %.1f times from %d KB to %d KB (prose: %v)", c.Name, grew, len(s)>>10, len(l)>>10, floor)
		}
	}
}

// What the guard lets through reads within a budget measured on this
// machine, from its reading of a large real doc (300 KB of this
// repository's own Markdown, a tenth of a second or so at load): one of those
// readings for the work past a plain read (helpers.MaxMarkdownSteps' tenth of
// a second), beside six times a real doc's reading as long as the case. Each
// case is read at the largest size the guard lets through, up to 256 KB,
// back to back with the real doc (timing.Ratio), and must take no longer,
// however the machine's load moves both.
func TestWhatTheGuardLetsThroughReadsWithinABudget(t *testing.T) {
	if timing.Race {
		t.Skip("the race detector slows a parser's tight loops many times more than its reading of prose: the budget is a plain build's (the package tests), the growth is checked here")
	}
	real := realDoc(t, 300<<10)
	allowed := func(md string) bool { return !helpers.MarkdownTooCostly(md, helpers.DocMarkdown) }
	for _, c := range mdpathology.Cases() {
		// The largest size let through, to the kilobyte.
		lo, hi := 0, 256
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if allowed(c.Make(mid << 10)) {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		if lo == 0 {
			continue
		}
		if lo == 256 {
			continue // never refused: linear, as the growth test holds it
		}
		md := c.Make(lo << 10)
		budget := 1 + 6*float64(len(md))/float64(len(real))
		took := timing.Ratio(2, 0, func() { read(t, real) }, func() { read(t, md) })
		if took > budget {
			t.Errorf("%s at %d KB: read in %.2f times a real %d KB doc's reading, past its budget of %.2f",
				c.Name, lo, took, len(real)>>10, budget)
		}
		t.Logf("%-50s let through to %3d KB, read in %.2f of a real doc's reading (budget %.2f)", c.Name, lo, took, budget)
	}
	t.Logf("a real %d KB doc read in %v", len(real)>>10, readTime(t, 3, real))
}

// realDoc is this repository's own Markdown to at least size bytes
// (mdpathology.RealDoc).
func realDoc(t *testing.T, size int) string {
	t.Helper()
	md, err := mdpathology.RealDoc(size)
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// A large real doc reads whole, not refused, every heading kept. (Its price,
// a small part of the guard's most, is held in helpers markdownCost_test.go.)
func TestALargeRealDocReads(t *testing.T) {
	md := realDoc(t, 300<<10)
	if helpers.MarkdownTooCostly(md, helpers.DocMarkdown) {
		t.Fatalf("a real %d KB doc is refused", len(md)>>10)
	}
	doc, err := FromMarkdown(md, nil)
	if err != nil {
		t.Fatalf("a real %d KB doc: %v", len(md)>>10, err)
	}
	headings := 0
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.Type == "heading" {
			headings++
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(doc)
	want := 0
	fenced := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, "#") {
			want++
		}
	}
	if headings < want*9/10 {
		t.Fatalf("a real doc kept %d headings of about %d", headings, want)
	}
}
