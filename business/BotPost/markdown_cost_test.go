package botpost

// Chat replies and the Markdown guard (helpers markdownCost.go), standing.
// Past maxChatMarkdown a reply is shown as its words, which grows as its
// length does. Below it, whatever the guard lets through of every slow case
// CommonMark and goldmark have (tests/mdpathology) renders within a budget
// measured on this machine, from its rendering of a real reply as long as the
// cap. A long real reply is still Markdown. No bound is a fixed number of
// milliseconds: each is a ratio of two renderings timed back to back
// (timing.Ratio).

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/tests/mdpathology"
	"github.com/akashc777/OneCamp/tests/timing"
)

// render renders md once, failing the test when it takes past a minute: a
// case the guard should have refused, read whole.
func render(t *testing.T, md string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		_ = renderMarkdown(md, true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatalf("rendering %d bytes (%.40q…) took past a minute", len(md), md)
	}
}

// realReply is this repository's own Markdown (mdpathology.RealDoc, its docs
// first) cut at a paragraph to at most size bytes: a long reply of headings,
// lists, code and links.
func realReply(t *testing.T, size int) string {
	t.Helper()
	s, err := mdpathology.RealDoc(size)
	if err != nil {
		t.Fatal(err)
	}
	s = s[:size]
	if i := strings.LastIndex(s, "\n\n"); i > 0 {
		s = s[:i]
	}
	if len(s) < size/2 {
		t.Fatalf("only %d bytes of Markdown found to reply with", len(s))
	}
	return s
}

// Past the cap, a reply is its words: from 64 KB to 256 KB its rendering
// grows as a linear one does (four times, ten allowed on a loaded machine;
// sixteen for a quadratic one), with a real 16 KB reply's rendering as the
// floor.
func TestAReplyPastTheCapRendersAsItsLength(t *testing.T) {
	const small, large = 64 << 10, 256 << 10
	reply := realReply(t, maxChatMarkdown)
	runtime.GC()
	floor := timing.Fastest(3, func() { render(t, reply) })
	for _, c := range mdpathology.Cases() {
		s, l := c.Make(small), c.Make(large)
		if grew := timing.Ratio(3, floor, func() { render(t, s) }, func() { render(t, l) }); grew > 10 {
			t.Errorf("%s: rendering grew %.1f times from %d KB to %d KB (a real reply: %v)", c.Name, grew, len(s)>>10, len(l)>>10, floor)
		}
	}
}

// Below the cap, what the guard lets through renders within a budget measured
// on this machine: fifty renderings of a real reply as long as the cap. A real
// 16 KB reply renders in a few milliseconds, so fifty is about a tenth of a
// second at load, the guard's most (helpers.MaxMarkdownSteps). Each case is
// rendered at the largest size the guard lets through, back to back with the
// real reply.
func TestWhatTheGuardLetsThroughRendersWithinABudget(t *testing.T) {
	if timing.Race {
		t.Skip("the race detector slows a parser's tight loops many times more than its reading of prose: the budget is a plain build's (the package tests), the growth is checked here")
	}
	const budget = 50
	reply := realReply(t, maxChatMarkdown)
	for _, c := range mdpathology.Cases() {
		lo, hi := 0, maxChatMarkdown>>10
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if md := c.Make(mid << 10); len(md) <= maxChatMarkdown && !helpers.MarkdownTooCostly(md, helpers.ChatMarkdown) {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		if lo == 0 {
			continue
		}
		if lo == maxChatMarkdown>>10 {
			continue // never refused below the cap: what the cap bounds
		}
		md := c.Make(lo << 10)
		took := timing.Ratio(2, 0, func() { render(t, reply) }, func() { render(t, md) })
		if took > budget {
			t.Errorf("%s at %d KB: rendered in %.1f times a real reply's rendering, past the budget of %d",
				c.Name, lo, took, budget)
		}
		t.Logf("%-50s let through to %2d KB, rendered in %.1f real replies' time", c.Name, lo, took)
	}
}

// A long real reply, as long as the cap, is still Markdown.
func TestALongRealReplyIsStillMarkdown(t *testing.T) {
	md := realReply(t, maxChatMarkdown)
	got := renderMarkdown(md, false)
	if !strings.Contains(got, "<li>") || !strings.Contains(got, "<strong>") || strings.Contains(got, "\n## ") {
		t.Fatalf("a real %d KB reply was not rendered as Markdown: %.300s", len(md)>>10, got)
	}
}
