package business

// Formatting across the bridge. Slack → OneCamp reuses the importer's mrkdwn
// renderer; OneCamp → Slack is the reverse, written here: the editor's HTML
// becomes Slack mrkdwn, keeping emphasis, code, links and lists and dropping
// everything Slack cannot show.

import (
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// slackTextLimit keeps a message under Slack's 40,000-character ceiling with
// room for the truncation note.
const slackTextLimit = 39000

var reManyNewlines = regexp.MustCompile(`\n{3,}`)

// slackEscape escapes the three characters Slack reserves for its own markup.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// htmlToMrkdwn converts OneCamp message HTML to Slack mrkdwn.
func htmlToMrkdwn(src string) string {
	z := html.NewTokenizer(strings.NewReader(src))
	var b strings.Builder

	// Links are buffered so the label can be written inside <url|label>.
	var linkHref string
	var linkText *strings.Builder
	inPre := 0
	var lists []int // item counter per open list; -1 for a bulleted list
	write := func(s string) {
		if linkText != nil {
			linkText.WriteString(s)
			return
		}
		b.WriteString(s)
	}
	newline := func() {
		if linkText != nil {
			return
		}
		if s := b.String(); s != "" && !strings.HasSuffix(s, "\n") {
			b.WriteString("\n")
		}
	}

	for {
		switch z.Next() {
		case html.ErrorToken:
			out := reManyNewlines.ReplaceAllString(b.String(), "\n\n")
			out = strings.TrimSpace(out)
			if len(out) > slackTextLimit {
				out = out[:slackTextLimit] + "\n… (shortened; the full message is in OneCamp)"
			}
			return out
		case html.TextToken:
			t := string(z.Text())
			if inPre == 0 {
				t = strings.ReplaceAll(t, "\n", " ")
			}
			write(slackEscape(t))
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				attrs[string(k)] = string(v)
			}
			switch string(name) {
			case "strong", "b":
				write("*")
			case "em", "i":
				write("_")
			case "s", "del", "strike":
				write("~")
			case "code":
				if inPre == 0 {
					write("`")
				}
			case "pre":
				newline()
				write("```\n")
				inPre++
			case "br":
				write("\n")
			case "a":
				if href := attrs["href"]; href != "" && linkText == nil {
					linkHref = href
					linkText = &strings.Builder{}
				}
			case "ul":
				newline()
				lists = append(lists, -1)
			case "ol":
				newline()
				lists = append(lists, 0)
			case "li":
				newline()
				indent := ""
				if len(lists) > 1 {
					indent = strings.Repeat("    ", len(lists)-1)
				}
				if n := len(lists); n > 0 && lists[n-1] >= 0 {
					lists[n-1]++
					write(indent + strconv.Itoa(lists[n-1]) + ". ")
				} else {
					write(indent + "• ")
				}
			case "blockquote":
				newline()
				write("&gt; ")
			case "img":
				if src := attrs["src"]; strings.HasPrefix(src, "http") {
					write("<" + src + "|image>")
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "strong", "b":
				write("*")
			case "em", "i":
				write("_")
			case "s", "del", "strike":
				write("~")
			case "code":
				if inPre == 0 {
					write("`")
				}
			case "pre":
				if inPre > 0 {
					inPre--
				}
				newline()
				write("```\n")
			case "a":
				if linkText != nil {
					label := strings.TrimSpace(linkText.String())
					linkText = nil
					href := strings.NewReplacer("|", "%7C", ">", "%3E", "<", "%3C").Replace(linkHref)
					if label == "" || label == slackEscape(linkHref) {
						b.WriteString("<" + href + ">")
					} else {
						b.WriteString("<" + href + "|" + label + ">")
					}
				}
			case "ul", "ol":
				if len(lists) > 0 {
					lists = lists[:len(lists)-1]
				}
				newline()
			case "p", "div", "li", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "tr":
				newline()
			}
		}
	}
}

// paragraphs turns the importer's rendered HTML, which keeps Slack's raw
// newlines, into paragraphs the OneCamp editor shows as lines.
func paragraphs(rendered string) string {
	rendered = strings.TrimSpace(rendered)
	if rendered == "" || strings.Contains(rendered, "<pre>") {
		// A code block owns its newlines; wrapping would break it.
		return rendered
	}
	lines := strings.Split(rendered, "\n")
	return "<p>" + strings.Join(lines, "<br/>") + "</p>"
}
