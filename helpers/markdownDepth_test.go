package helpers

import (
	"strings"
	"testing"
)

// How deep a line opens is counted from its quote and list markers alone, and
// every line counts, whatever its line endings.
func TestHowDeepALineOfMarkdownOpens(t *testing.T) {
	for line, want := range map[string]int{
		"":                0,
		"plain":           0,
		">>> x":           3,
		"> > > x":         3,
		"\t> x":           1,
		"- item":          1,
		"* > + x":         3,
		"1. one":          1,
		"12) one":         1,
		"1. 2) - x":       3,
		"1.x":             0, // no space: not a list marker
		"-x":              0,
		"1234567890. ten": 0, // ten digits: not a list marker
		"    code":        0,
		"- - -":           2, // a rule, counted as markers: harmless below the limit
	} {
		if got := markersOpening(line); got != want {
			t.Errorf("%q opens %d deep, want %d", line, got, want)
		}
	}
}

// Markdown is too deep past MaxMarkdownNesting on any one line, and not at it.
func TestMarkdownTooDeepIsPastTheLimitOnAnyLine(t *testing.T) {
	at := strings.Repeat(">", MaxMarkdownNesting)
	over := strings.Repeat("- ", MaxMarkdownNesting+1)
	for md, want := range map[string]bool{
		at + " x":                          false,
		over + "x":                         true,
		"a\n" + at + "\r\n" + over + "\rb": true,
		"a\r\n" + at + "\nb":               false,
		strings.Repeat(">", 1<<20):         true,
		strings.Repeat("> - ", MaxMarkdownNesting/2+1) + "x": true,
	} {
		if got := MarkdownTooDeep(md); got != want {
			t.Errorf("%.30q… (%d bytes) too deep = %v, want %v", md, len(md), got, want)
		}
	}
}
