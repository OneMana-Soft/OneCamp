package contentcodec

import "regexp"

// B2's formatting (docs programme B2), as the codec reads and writes it: the
// same rules, rule for rule, as the web app's lib/codec/docFormat.ts.
//
//   - Colours are names (N15), by colours.go's list and rules.
//   - A block's own colour (blockColor) and a large quote (quoteSize) are
//     global attributes: HTML carries them, Markdown has no way to.
//   - A highlight is a mark by colour name (<mark> in Markdown, its colour
//     left out); a table of
//     contents is a block ([TOC] in Markdown); a toggle's first line is its
//     summary, and an old toggle's title becomes that line (normalize.go).

// highlightColour is a highlight's colour: its name, yellow when it names none of ours.
func highlightColour(v any) string {
	if s, _ := v.(string); IsDocColour(s) {
		return s
	}
	return "sun"
}

func quoteSizeValue(v any) string {
	if s, _ := v.(string); s == "large" {
		return "large"
	}
	return ""
}

// orNil is a string value, nil when empty.
func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// docFormatAttrs are B2's global attributes.
var docFormatAttrs = []globalAttr{
	{
		name: "blockColor", html: "data-block-color",
		on:    []string{"paragraph", "heading", "blockquote", "listItem", "taskItem", "collapsible"},
		typed: func(v any) any { return orNil(blockColourValue(v)) },
		read:  func(raw string, _ bool) any { return orNil(blockColourValue(raw)) },
		write: func(v any) string { return blockColourValue(v) },
	},
	{
		name: "quoteSize", html: "data-quote-size",
		on:    []string{"blockquote"},
		typed: func(v any) any { return orNil(quoteSizeValue(v)) },
		read:  func(raw string, _ bool) any { return orNil(quoteSizeValue(raw)) },
		write: func(v any) string { return quoteSizeValue(v) },
	},
}

// calloutIcon is a callout's icon: a Lucide name ("rocket",
// "triangle-alert"), or "" for its kind's own.
var lucideName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+){0,3}$`)

func calloutIcon(v any) string {
	if s, _ := v.(string); len(s) <= 40 && lucideName.MatchString(s) {
		return s
	}
	return ""
}

// isTocLine reports whether a paragraph, as written in Markdown, is a table
// of contents: [TOC] (as Python-Markdown, Typora and the codec write it), or
// GitLab's [[_TOC_]]. Written \[TOC\], it is text.
func isTocLine(src string) bool {
	switch asciiLower(src) {
	case "[toc]", "[[toc]]", "[[_toc_]]":
		return true
	}
	return false
}
