package business

import (
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
)

// HTML draws a card as a message's body, for a post that can't hold the card
// itself: an incoming webhook's blocks, and an app's answer posted in a
// channel. It was business/Webhook's RenderBlocksToHTML; there is one now.
//
// A post can't be pressed, so the only actions kept are link buttons, as
// links; a button or menu that would call its owner is left out rather than
// drawn as a button that does nothing. Text is escaped before its mrkdwn is
// read, and the result goes through the same sanitiser as any user's HTML.
func HTML(c Card) string {
	var sb strings.Builder
	for _, b := range c.Blocks {
		switch b.Type {
		case BlockHeader:
			if b.Text != nil {
				sb.WriteString("<h3>" + html.EscapeString(b.Text.Text) + "</h3>")
			}
		case BlockSection:
			if b.Text != nil {
				sb.WriteString(paragraph(*b.Text))
			}
			if len(b.Fields) > 0 {
				sb.WriteString("<ul>")
				for _, f := range b.Fields {
					sb.WriteString("<li>" + inline(f) + "</li>")
				}
				sb.WriteString("</ul>")
			}
		case BlockContext:
			var parts []string
			for _, e := range b.Elements {
				if e.Text != nil {
					parts = append(parts, inline(*e.Text))
				}
			}
			if len(parts) > 0 {
				sb.WriteString("<p><small>" + strings.Join(parts, " · ") + "</small></p>")
			}
		case BlockImage:
			alt := b.AltText
			if alt == "" && b.Title != nil {
				alt = b.Title.Text
			}
			sb.WriteString(`<p><img src="` + html.EscapeString(b.ImageURL) + `" alt="` + html.EscapeString(alt) + `"/></p>`)
			if b.Title != nil {
				sb.WriteString("<p><small>" + html.EscapeString(b.Title.Text) + "</small></p>")
			}
		case BlockDivider:
			sb.WriteString("<hr/>")
		case BlockActions:
			var links []string
			for _, e := range b.Elements {
				if e.Type == ElementButton && e.URL != "" && e.Text != nil {
					links = append(links, `<a href="`+html.EscapeString(e.URL)+`">`+html.EscapeString(e.Text.Text)+`</a>`)
				}
			}
			if len(links) > 0 {
				sb.WriteString("<p>" + strings.Join(links, " · ") + "</p>")
			}
		}
	}
	return postPolicy.Sanitize(sb.String())
}

var postPolicy = bluemonday.UGCPolicy()

func paragraph(t Text) string { return "<p>" + inline(t) + "</p>" }

// inline is a text object as inline HTML: plain text escaped, mrkdwn's bold,
// italics, code, strike-through and links read after escaping, and its line
// breaks kept.
func inline(t Text) string {
	s := html.EscapeString(t.Text)
	if t.Type == TextMarkdown {
		s = reLabelled.ReplaceAllStringFunc(s, func(m string) string {
			sub := reLabelled.FindStringSubmatch(m)
			return anchor(sub[1], sub[2])
		})
		s = reBare.ReplaceAllStringFunc(s, func(m string) string {
			sub := reBare.FindStringSubmatch(m)
			return anchor(sub[1], sub[1])
		})
		s = reCode.ReplaceAllString(s, "<code>$1</code>")
		s = reBold.ReplaceAllString(s, "<strong>$1</strong>")
		s = reItalic.ReplaceAllString(s, "$1<em>$2</em>")
		s = reStrike.ReplaceAllString(s, "<del>$1</del>")
	}
	return strings.ReplaceAll(s, "\n", "<br/>")
}

// anchor is a mrkdwn link (already escaped) as a link when its address is
// http, https or mailto, and as its words otherwise.
func anchor(escapedHref, label string) string {
	u, err := url.Parse(html.UnescapeString(escapedHref))
	if err != nil {
		return label
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto":
		return `<a href="` + escapedHref + `">` + label + `</a>`
	}
	return label
}

var (
	reLabelled = regexp.MustCompile(`&lt;([^|\s]+?)\|(.+?)&gt;`)
	reBare     = regexp.MustCompile(`&lt;((?:https?|mailto):[^\s|]+?)&gt;`)
	reCode     = regexp.MustCompile("`([^`]+)`")
	reBold     = regexp.MustCompile(`\*([^*\n]+)\*`)
	reItalic   = regexp.MustCompile(`(^|[\s(])_([^_\n]+)_`)
	reStrike   = regexp.MustCompile(`~([^~\n]+)~`)
)
