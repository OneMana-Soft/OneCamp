package helpers

import (
	"strings"
	"testing"
)

// How deep a line may sit: its quote and list markers, and the lists its
// indentation carries on, one for every two columns (a tab four), never more
// than the lines before it left open. A line of only spaces opens nothing.
func TestHowDeepALineOfMarkdownMaySit(t *testing.T) {
	for _, c := range []struct {
		line       string
		open, want int
	}{
		{"", 0, 0},
		{"plain", 0, 0},
		{">>> x", 0, 3},
		{"> > > x", 0, 3}, // a quote's own space is its marker's
		{"\t> x", 0, 1},
		{"\t> x", 10, 3},
		{"- item", 0, 1},
		{"* > + x", 0, 3},
		{"1. one", 0, 1},
		{"12) one", 0, 1},
		{"1. 2) - x", 0, 3},
		{"1.x", 0, 0}, // no space: not a list marker
		{"-x", 0, 0},
		{"1234567890. ten", 0, 0}, // ten digits: not a list marker
		{"    code", 0, 0},        // nothing open to carry on
		{"    code", 5, 2},
		{"- - -", 0, 2}, // a rule, counted as markers: harmless below the limit
		{"      - x", 1, 2},
		{"      - x", 10, 4},
		{">   - x", 9, 3}, // two columns inside the quote carry on one list
		{"     ", 100, 0},
	} {
		if got := lineDepth(c.line, c.open); got != c.want {
			t.Errorf("%q with %d open sits %d deep, want %d", c.line, c.open, got, c.want)
		}
	}
}

// Markdown is too deep past MaxMarkdownNesting on any one line, and not at
// it. Nesting carried from line to line by indentation counts as one line of
// markers does, and indentation alone carries on no more than was open.
func TestMarkdownTooDeepIsPastTheLimitOnAnyLine(t *testing.T) {
	at := strings.Repeat(">", MaxMarkdownNesting)
	over := strings.Repeat("- ", MaxMarkdownNesting+1)
	half := strings.Repeat("- ", MaxMarkdownNesting/2) + "x\n"
	carried := strings.Repeat(" ", MaxMarkdownNesting) + half // carries on 64, opens 64 more
	for md, want := range map[string]bool{
		at + " x":                          false,
		over + "x":                         true,
		"a\n" + at + "\r\n" + over + "\rb": true,
		"a\r\n" + at + "\nb":               false,
		strings.Repeat(">", 1<<20):         true,
		strings.Repeat("> - ", MaxMarkdownNesting/2+1) + "x": true,
		half + carried: false,
		half + carried + strings.Repeat(" ", 2*MaxMarkdownNesting) + "- x\n":       true,
		half + strings.Repeat(" ", 1000) + "deeply indented, nothing new\n":        false,
		"text\n" + strings.Repeat(" ", 1000) + "code, nothing open\n":              false,
		half + "\n" + strings.Repeat(" ", 4000) + "\n" + strings.Repeat("\t", 900): false,
	} {
		if got := MarkdownTooDeep(md); got != want {
			t.Errorf("%.30q… (%d bytes) too deep = %v, want %v", md, len(md), got, want)
		}
	}
}
