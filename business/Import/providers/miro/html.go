package miro

// Miro keeps an item's words as HTML ("<p>Ship <strong>it</strong></p>"). A
// board's words are plain text, so the HTML is read for its lines: a
// paragraph, a break or a list item starts a line, a list item is marked
// "• ", entities are read, and the first link is kept for the item's link.
// Bold, italic, underline, colour and size are what is lost; the report says
// so once for the board.

import (
	"html"
	"regexp"
	"strings"
)

var (
	tagRe    = regexp.MustCompile(`(?is)<(/?)([a-z][a-z0-9]*)([^>]*)>`)
	hrefRe   = regexp.MustCompile(`(?is)\bhref\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	spaceRe  = regexp.MustCompile(`[ \t\r\f\v\x{00a0}]+`)
	anySpace = regexp.MustCompile(`\s+`)
)

// formatting are the tags whose look a board's plain words can't keep.
var formatting = map[string]bool{
	"b": true, "strong": true, "i": true, "em": true, "u": true, "s": true, "strike": true,
	"del": true, "mark": true, "span": true, "font": true, "sub": true, "sup": true, "code": true,
}

// blocks are the tags that end a line before and after them.
var blocks = map[string]bool{
	"p": true, "div": true, "li": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true, "pre": true, "tr": true, "ul": true, "ol": true,
}

// plainText is HTML's words as lines, the first link in it, and whether it
// carried formatting the words lose. The HTML's own line breaks are spaces, as
// a browser reads them; a block ends a line, a break always does, and an
// empty paragraph is a blank line. Pure.
func plainText(s string) (text, link string, formatted bool) {
	if !strings.Contains(s, "<") {
		var lines []string
		for _, l := range strings.Split(html.UnescapeString(s), "\n") {
			lines = append(lines, strings.TrimSpace(collapse(l)))
		}
		return tidyLines(lines), "", false
	}
	var lines []string
	var cur strings.Builder
	// pending is the line so far, read for words: a bullet alone isn't any.
	pending := func() string {
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(collapse(cur.String())), "•"))
	}
	flush := func(always bool) {
		if pending() != "" || always {
			lines = append(lines, strings.TrimSpace(collapse(cur.String())))
			cur.Reset()
		}
	}
	type opened struct {
		lines int
		empty bool
	}
	var open []opened
	// The HTML's own spaces and line breaks are one space each.
	words := func(t string) { cur.WriteString(html.UnescapeString(anySpace.ReplaceAllString(t, " "))) }
	last := 0
	for _, m := range tagRe.FindAllStringSubmatchIndex(s, -1) {
		words(s[last:m[0]])
		last = m[1]
		closing := s[m[2]:m[3]] == "/"
		name := strings.ToLower(s[m[4]:m[5]])
		attrs := s[m[6]:m[7]]
		switch {
		case name == "br":
			flush(true)
		case blocks[name] && !closing:
			flush(false)
			open = append(open, opened{lines: len(lines), empty: pending() == ""})
			if name == "li" {
				cur.WriteString("• ")
			}
		case blocks[name] && closing:
			wrote := pending() != ""
			flush(false)
			if !wrote {
				cur.Reset() // a stray bullet or space of an empty block
			}
			if n := len(open); n > 0 {
				o := open[n-1]
				open = open[:n-1]
				if !wrote && o.empty && len(lines) == o.lines && name == "p" {
					lines = append(lines, "") // an empty paragraph: a blank line
				}
			}
		case name == "a":
			if !closing && link == "" {
				if h := hrefRe.FindStringSubmatch(attrs); h != nil {
					link = strings.TrimSpace(html.UnescapeString(h[2] + h[3] + h[4]))
				}
			}
		default:
			if formatting[name] {
				formatted = true
			}
		}
	}
	words(s[last:])
	flush(false)
	if !safeLink(link) {
		link = ""
	}
	return tidyLines(lines), link, formatted
}

// tidyLines joins lines, without blank lines first or last, and never two
// blank lines running.
func tidyLines(lines []string) string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, l)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func collapse(s string) string { return spaceRe.ReplaceAllString(s, " ") }

// safeLink is a link a board may open: http, https or mailto, never a script.
func safeLink(u string) bool {
	l := strings.ToLower(strings.TrimSpace(u))
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "mailto:")
}
