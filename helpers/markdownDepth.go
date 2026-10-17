package helpers

import "strings"

// How deep Markdown may nest before a parser reads it: the one guard every
// goldmark entry point asks first (business/BotPost's chat replies, and on the
// docs line business/ContentCodec's reader).
//
// goldmark's time and memory grow much faster than the nesting it is given.
// Measured 16 Oct 2026: a chat reply of 50,000 list items nested on one line
// (100 KB) took 12 s and 7.5 GB of heap to render, and 50,000 nested quotes
// 0.9 s and 6 GB; a doc of 500,000 nested quotes took six minutes and half a
// gigabyte of stack. Up to MaxMarkdownNesting it is milliseconds.

// MaxMarkdownNesting is how many quotes and lists one line of Markdown may
// open one inside another. Generous: chat and docs show a handful of levels,
// and HTML is refused past 512 open elements by its own parser.
const MaxMarkdownNesting = 128

// MarkdownTooDeep says whether any line of md opens more than
// MaxMarkdownNesting quotes and lists one inside another: Markdown no parser
// should be asked to read. Linear in md.
func MarkdownTooDeep(md string) bool {
	for len(md) > 0 {
		line := md
		if i := strings.IndexAny(md, "\n\r"); i >= 0 {
			line, md = md[:i], md[i+1:]
		} else {
			md = ""
		}
		if markersOpening(line) > MaxMarkdownNesting {
			return true
		}
	}
	return false
}

// markersOpening counts the quote markers (">") and list markers ("-", "+",
// "*", or up to nine digits and "." or ")", each followed by a space or a
// tab) a line opens with, past the spaces and tabs around them: how deep the
// blocks it starts nest. Pure.
func markersOpening(line string) int {
	n := 0
	for i := 0; i < len(line); {
		switch c := line[i]; {
		case c == ' ' || c == '\t':
			i++
		case c == '>':
			n++
			i++
		case (c == '-' || c == '+' || c == '*') && i+1 < len(line) && (line[i+1] == ' ' || line[i+1] == '\t'):
			n++
			i += 2
		case c >= '0' && c <= '9':
			j := i
			for j < len(line) && j-i < 9 && line[j] >= '0' && line[j] <= '9' {
				j++
			}
			if j+1 < len(line) && (line[j] == '.' || line[j] == ')') && (line[j+1] == ' ' || line[j+1] == '\t') {
				n++
				i = j + 2
				continue
			}
			return n
		default:
			return n
		}
	}
	return n
}
