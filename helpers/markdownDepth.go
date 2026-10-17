package helpers

import "strings"

// How deep Markdown may nest before a parser reads it: the first thing the
// Markdown guard asks (MarkdownTooCostly, markdownCost.go), for every goldmark
// entry point (business/BotPost's chat replies, business/ContentCodec's doc
// reader).
//
// goldmark's time and memory grow much faster than the nesting it is given.
// Measured 16 Oct 2026: a chat reply of 50,000 list items nested on one line
// (100 KB) took 12 s and 7.5 GB of heap to render, and 50,000 nested quotes
// 0.9 s and 6 GB; a doc of 500,000 nested quotes took six minutes and half a
// gigabyte of stack. Nesting carried from line to line by indentation is the
// same: 128 list markers a line, each line indented to carry on every list
// before it, took 3.2 s for 210 KB. Up to MaxMarkdownNesting it is
// milliseconds.

// MaxMarkdownNesting is how many quotes and lists a line of Markdown may sit
// inside. Generous: chat and docs show a handful of levels, and HTML is
// refused past 512 open elements by its own parser.
const MaxMarkdownNesting = 128

// MarkdownTooDeep says whether any line of md may sit more than
// MaxMarkdownNesting quotes and lists deep. Linear in md.
func MarkdownTooDeep(md string) bool {
	open := 0 // the deepest any line so far may have left open
	for len(md) > 0 {
		line := md
		if i := strings.IndexAny(md, "\n\r"); i >= 0 {
			line, md = md[:i], md[i+1:]
		} else {
			md = ""
		}
		depth := lineDepth(line, open)
		if depth > MaxMarkdownNesting {
			return true
		}
		open = max(open, depth)
	}
	return false
}

// lineDepth is how deep the blocks on one line may sit, when the lines before
// it may have left at most open quotes and lists open: the quote and list
// markers it starts with (">", or "-", "+", "*", or up to nine digits and "."
// or ")", followed by a space or a tab), and the lists its indentation
// carries on. A list item's text starts at least two columns past where its
// marker did, so a line carries on at most one list for every two columns of
// space before or between its markers (a tab counted as four), and never more
// than were open; a quote is carried on only by its own marker. A line of
// nothing but spaces opens nothing. Pure.
func lineDepth(line string, open int) int {
	markers, cols, i := 0, 0, 0
scan:
	for i < len(line) {
		switch c := line[i]; {
		case c == ' ':
			cols++
			i++
		case c == '\t':
			cols += 4
			i++
		case c == '>':
			markers++
			i++
			if i < len(line) && line[i] == ' ' { // the quote's own space
				i++
			}
		case (c == '-' || c == '+' || c == '*') && i+1 < len(line) && (line[i+1] == ' ' || line[i+1] == '\t'):
			markers++
			i += 2
		case c >= '0' && c <= '9':
			j := i
			for j < len(line) && j-i < 9 && line[j] >= '0' && line[j] <= '9' {
				j++
			}
			if j+1 < len(line) && (line[j] == '.' || line[j] == ')') && (line[j+1] == ' ' || line[j+1] == '\t') {
				markers++
				i = j + 2
				continue
			}
			break scan
		default:
			break scan
		}
	}
	if markers == 0 && i == len(line) {
		return 0 // only spaces
	}
	return markers + min(cols/2, open)
}
