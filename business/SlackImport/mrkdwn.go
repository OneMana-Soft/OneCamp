package business

// Slack's message text (mrkdwn) as OneCamp's message HTML, for the import and
// the live bridge alike (business/SlackBridge), so a team that moves reads its
// history as it read it in Slack:
//
//   - Lines stay lines and paragraphs stay paragraphs: a line break is <br>, a
//     blank line starts a <p>. The editor that shows a message collapses raw
//     newlines, so a message of five lines used to arrive as one.
//   - ```code``` is a <pre> block and `code` is <code>, each with its text
//     exactly: no formatting runs inside either.
//   - *bold*, _italic_ and ~strike~ apply where Slack applies them, between
//     word boundaries: my_var_name and an address with two underscores stay as
//     they are.
//   - "> " lines are a quote.
//   - <@U…> is OneCamp's own mention chip ("<uuid>@<graph uid>", the composer's
//     shape) for someone who came across, so the chip opens their profile and
//     an edit of the message keeps it; else their name as text. <#C…> is a
//     channel's chip, <!here>, <!channel> and <!everyone> the composer's
//     broadcast chips (an import notifies nobody); <!subteam^…|@team> and
//     <!date^…|fallback> are their words. <https://…|label> is a link: only
//     http, https and mailto, as the sanitizer allows.
//
// Slack escapes &, < and > in a message's text; what is escaped is read back
// to its characters and escaped once for HTML. The sanitizer is the last word:
// nothing an export holds reaches a page unchecked.

import (
	"context"
	"strings"
	"unicode"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/microcosm-cc/bluemonday"
)

// mentionee is the OneCamp person a Slack mention names.
type mentionee struct {
	UUID, UID, Name string
}

// channelChip is the OneCamp channel a Slack channel mention names.
type channelChip struct {
	UUID, Name string
}

// mrkdwnNames is what a message's text needs of the people and channels it
// mentions, read before it is rendered, so rendering reads nothing: for the
// import, a message's mentions a batch at a time (textNames). Any map may be
// nil.
type mrkdwnNames struct {
	// people is the OneCamp person each Slack user came across as (a zero
	// mentionee, or none, for one who didn't).
	people map[string]mentionee
	// slack is a Slack user's name as the export or Slack knows it, for
	// someone with no OneCamp person.
	slack map[string]string
	// channels is the OneCamp channel each Slack channel came across as.
	channels map[string]channelChip
}

// mentionedIDs is the Slack users and channels text mentions (<@U…>, <#C…>),
// each once, in the order written. Pure.
func mentionedIDs(text string) (users, channels []string) {
	seen := map[string]bool{}
	for rest := text; ; {
		open := strings.IndexByte(rest, '<')
		if open < 0 {
			return users, channels
		}
		end := strings.IndexByte(rest[open+1:], '>')
		if end < 0 {
			return users, channels
		}
		body, _, _ := strings.Cut(rest[open+1:open+1+end], "|")
		rest = rest[open+1+end+1:]
		if len(body) < 2 || seen[body] {
			continue
		}
		seen[body] = true
		switch body[0] {
		case '@':
			users = append(users, body[1:])
		case '#':
			channels = append(channels, body[1:])
		}
	}
}

// htmlSanitizer is shared (bluemonday policies are safe for concurrent use
// and creating one is cheap but not free).
var htmlSanitizer = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	// The chips: a person's and a broadcast's (data-type "mention"), and a
	// channel's ("channelMention"), as the composer writes them.
	p.AllowAttrs("data-id", "data-type", "data-label").OnElements("span")
	p.AllowAttrs("class").OnElements("span", "code", "pre", "a")
	p.AllowURLSchemes("http", "https", "mailto")
	return p
}()

// RenderSlackText is a Slack message's text as safe message HTML: block
// elements (<p>, <blockquote>, <pre>), "" for no text. names holds what its
// mentions name (mrkdwnNames); it reads nothing itself.
func RenderSlackText(ctx context.Context, m *SlackMessage, r mrkdwnNames) string {
	if strings.TrimSpace(m.Text) == "" {
		return ""
	}
	clean := htmlSanitizer.Sanitize(renderBlocks(m.Text, r))
	if strings.TrimSpace(helpers.RemoveHTMLTags(clean)) == "" {
		// The sanitizer ate everything (a pathological input): keep the words
		// rather than lose the message.
		helpers.LogWarnWithContext(ctx, "SlackImport.RenderSlackText: the sanitizer left nothing of ts=%s", m.Ts)
		return "<p>" + htmlEscape(slackUnescape(m.Text)) + "</p>"
	}
	return clean
}

// RenderMrkdwn renders the text of a LIVE Slack message (the bridge) as safe
// message HTML. Its people have no OneCamp accounts to chip, so a mention is
// their name as text: userName supplies it (live events carry bare <@U123>),
// and one it can't name reads "@someone".
func RenderMrkdwn(ctx context.Context, text string, userName func(slackID string) string) string {
	users, _ := mentionedIDs(text)
	names := mrkdwnNames{slack: make(map[string]string, len(users))}
	if userName != nil {
		for _, id := range users {
			names.slack[id] = userName(id)
		}
	}
	return RenderSlackText(ctx, &SlackMessage{Text: text}, names)
}

// withBotLead puts a Slack app's name before its message, as "[bot: Name]":
// OneCamp files an app's message under the person who ran the import, so the
// lead says whose it was. Plain words, never a chip, so an edit of the
// message has nobody to resolve.
func withBotLead(m *SlackMessage, body string) string {
	if body == "" || !isBotMessage(m) {
		return body
	}
	name := pickBotName(m)
	if name == "" {
		name = "bot"
	}
	lead := `<span class="bot-attrib">[bot: ` + htmlEscape(name) + `]</span> `
	if strings.HasPrefix(body, "<p>") {
		return "<p>" + lead + strings.TrimPrefix(body, "<p>")
	}
	return "<p>" + strings.TrimSpace(lead) + "</p>" + body
}

// asMeMessage is a /me message's body as Slack shows it, in italics: a
// one-paragraph body of a me_message is wrapped in <em>. Pure.
func asMeMessage(m *SlackMessage, body string) string {
	if m.Subtype != "me_message" || !strings.HasPrefix(body, "<p>") || !strings.HasSuffix(body, "</p>") || strings.Count(body, "<p>") != 1 {
		return body
	}
	return "<p><em>" + strings.TrimSuffix(strings.TrimPrefix(body, "<p>"), "</p>") + "</em></p>"
}

// renderBlocks is text's code blocks, quotes and paragraphs, unsanitized.
func renderBlocks(text string, r mrkdwnNames) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	parts := strings.Split(text, "```")
	if len(parts)%2 == 0 {
		// An odd fence opens nothing: it and what follows are text.
		last := len(parts) - 1
		parts[last-1] += "```" + parts[last]
		parts = parts[:last]
	}
	var b strings.Builder
	for i, part := range parts {
		if i%2 == 1 {
			b.WriteString(codeBlock(part))
			continue
		}
		b.WriteString(paragraphs(part, r))
	}
	return b.String()
}

// codeBlock is a ```fenced``` block's text as a <pre>, exactly as written.
func codeBlock(code string) string {
	code = strings.TrimPrefix(code, "\n")
	code = strings.TrimSuffix(code, "\n")
	if strings.TrimSpace(code) == "" {
		return ""
	}
	return "<pre><code>" + htmlEscape(slackUnescape(code)) + "</code></pre>"
}

// paragraphs is a run of text's paragraphs and quotes: a blank line ends one,
// "> " lines are a quote (">>> " quotes every line after it), and the lines of
// either are kept with <br>.
func paragraphs(text string, r mrkdwnNames) string {
	var b strings.Builder
	var lines []string
	quoting, quoteRest := false, false
	flush := func() {
		if len(lines) == 0 {
			return
		}
		inner := strings.Join(lines, "<br>")
		if quoting {
			b.WriteString("<blockquote><p>" + inner + "</p></blockquote>")
		} else {
			b.WriteString("<p>" + inner + "</p>")
		}
		lines = nil
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		quote, rest := quoteLine(line)
		if strings.HasPrefix(strings.TrimLeft(line, " "), "&gt;&gt;&gt;") {
			quoteRest, rest = true, strings.TrimPrefix(strings.TrimPrefix(strings.TrimLeft(line, " "), "&gt;&gt;&gt;"), " ")
		}
		quote = quote || quoteRest
		if quote != quoting {
			flush()
			quoting = quote
		}
		if quote {
			line = rest
		}
		lines = append(lines, renderInline(line, r))
	}
	flush()
	return b.String()
}

// quoteLine is whether a line is a quote ("> " as Slack escapes it), and its
// words without the marker. Pure.
func quoteLine(line string) (bool, string) {
	trimmed := strings.TrimLeft(line, " ")
	if !strings.HasPrefix(trimmed, "&gt;") {
		return false, line
	}
	return true, strings.TrimPrefix(strings.TrimPrefix(trimmed, "&gt;"), " ")
}

// A placeholder stands for a code span or a Slack token while a line is
// formatted, so no formatting runs inside either: a rune of Unicode's private
// use area, the first at placeholderBase.
const (
	placeholderBase = 0xE000
	placeholderMax  = 0xF8FF
)

func isPlaceholder(r rune) bool { return r >= placeholderBase && r <= placeholderMax }

// renderInline is one line of text as HTML: its code spans and tokens, its
// words escaped, and Slack's emphasis.
func renderInline(line string, r mrkdwnNames) string {
	var (
		text   strings.Builder
		pieces []string
	)
	// hold puts a placeholder for html in the text; past the area (a line of
	// thousands of tokens) the token's own text is kept as words instead.
	hold := func(html, raw string) {
		if placeholderBase+len(pieces) > placeholderMax {
			text.WriteString(raw)
			return
		}
		text.WriteRune(rune(placeholderBase + len(pieces)))
		pieces = append(pieces, html)
	}
	// The private use area is the placeholders': a character of it in the
	// text itself would be read as one.
	line = strings.Map(func(c rune) rune {
		if isPlaceholder(c) {
			return -1
		}
		return c
	}, line)
	for i := 0; i < len(line); {
		switch line[i] {
		case '`':
			if end := strings.IndexByte(line[i+1:], '`'); end > 0 {
				hold("<code>"+htmlEscape(slackUnescape(line[i+1:i+1+end]))+"</code>", line[i:i+end+2])
				i += end + 2
				continue
			}
		case '<':
			if end := strings.IndexByte(line[i+1:], '>'); end > 0 {
				hold(renderToken(line[i+1:i+1+end], r), line[i:i+end+2])
				i += end + 2
				continue
			}
		}
		text.WriteByte(line[i])
		i++
	}
	out := emphasise(htmlEscape(slackUnescape(text.String())))
	if len(pieces) == 0 {
		return out
	}
	var b strings.Builder
	for _, c := range out {
		if isPlaceholder(c) && int(c-placeholderBase) < len(pieces) {
			b.WriteString(pieces[c-placeholderBase])
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// renderToken is a Slack <token> (its inside, as Slack escapes it) as HTML.
func renderToken(tok string, r mrkdwnNames) string {
	body, label, _ := strings.Cut(tok, "|")
	label = strings.TrimSpace(slackUnescape(label))
	switch {
	case strings.HasPrefix(body, "@"):
		return personToken(strings.TrimPrefix(body, "@"), label, r)
	case strings.HasPrefix(body, "#"):
		return channelToken(strings.TrimPrefix(body, "#"), label, r)
	case strings.HasPrefix(body, "!"):
		return specialToken(strings.TrimPrefix(body, "!"), label)
	}
	return linkToken(slackUnescape(body), label)
}

// personToken is <@U…>: the person's chip when they came across, else their
// name.
func personToken(id, label string, r mrkdwnNames) string {
	if who := r.people[id]; who.UUID != "" && who.UID != "" {
		name := who.Name
		if name == "" {
			name = label
		}
		if name == "" {
			name = "someone"
		}
		return `<span class="mention" data-type="mention" data-id="` + htmlEscape(who.UUID+"@"+who.UID) +
			`" data-label="` + htmlEscape(name) + `">@` + htmlEscape(name) + `</span>`
	}
	name := strings.TrimPrefix(label, "@")
	if name == "" {
		name = r.slack[id]
	}
	if name == "" {
		name = "someone"
	}
	return "@" + htmlEscape(name)
}

// channelToken is <#C…>: the channel's chip when it came across, else its
// name.
func channelToken(id, label string, r mrkdwnNames) string {
	if ch := r.channels[id]; ch.UUID != "" {
		name := ch.Name
		if name == "" {
			name = strings.TrimPrefix(label, "#")
		}
		if name == "" {
			name = "channel"
		}
		return `<span class="channel-mention" data-type="channelMention" data-id="channel:` + htmlEscape(ch.UUID) +
			`" data-label="` + htmlEscape(name) + `">#` + htmlEscape(name) + `</span>`
	}
	if label == "" {
		label = "channel"
	}
	return "#" + htmlEscape(strings.TrimPrefix(label, "#"))
}

// specialToken is <!…>: a broadcast's chip, or the words a team mention, a
// date and anything else Slack writes this way say.
func specialToken(word, label string) string {
	name, _, _ := strings.Cut(word, "^")
	switch name {
	case "here", "channel", "everyone":
		return `<span class="mention" data-type="mention" data-id="broadcast:` + name +
			`" data-label="` + name + `">@` + name + `</span>`
	case "subteam":
		if team := strings.TrimPrefix(label, "@"); team != "" {
			return "@" + htmlEscape(team)
		}
		return "@team"
	case "date":
		return htmlEscape(label)
	}
	if label != "" {
		return htmlEscape(label)
	}
	return "@" + htmlEscape(name)
}

// linkToken is a <url|label>: a link for an address the sanitizer allows,
// else its words.
func linkToken(url, label string) string {
	url = strings.TrimSpace(url)
	if label == "" {
		label = strings.TrimPrefix(url, "mailto:")
	}
	lower := strings.ToLower(url)
	if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "mailto:") {
		return `<a href="` + htmlEscape(url) + `">` + htmlEscape(label) + `</a>`
	}
	return htmlEscape(label)
}

// The emphasis Slack writes, and the tag each is.
var emphases = []struct {
	mark rune
	tag  string
}{{'*', "strong"}, {'_', "em"}, {'~', "del"}}

// emphasise is s (escaped HTML text, with placeholders) with Slack's
// emphasis as tags: a mark opens after a non-word character (or at the start)
// before a non-space, and closes after a non-space before a non-word
// character (or at the end), so snake_case and 2*3*4 stay as they are. Pure.
func emphasise(s string) string {
	for _, e := range emphases {
		s = mark(s, e.mark, e.tag)
	}
	return s
}

func mark(s string, m rune, tag string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); i++ {
		if rs[i] == m && opensAt(rs, i) {
			if j := closesAt(rs, i, m); j > 0 {
				b.WriteString("<" + tag + ">" + string(rs[i+1:j]) + "</" + tag + ">")
				i = j
				continue
			}
		}
		b.WriteRune(rs[i])
	}
	return b.String()
}

func opensAt(rs []rune, i int) bool {
	return (i == 0 || !isWordRune(rs[i-1])) && i+1 < len(rs) && !unicode.IsSpace(rs[i+1]) && rs[i+1] != rs[i]
}

func closesAt(rs []rune, i int, m rune) int {
	for j := i + 2; j < len(rs); j++ {
		if rs[j] == m && !unicode.IsSpace(rs[j-1]) && (j+1 == len(rs) || !isWordRune(rs[j+1])) {
			return j
		}
	}
	return -1
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// slackUnescape reads back the three characters Slack escapes in a message's
// text. Pure.
func slackUnescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

// htmlEscape is helpers.EscapeHTML.
func htmlEscape(s string) string {
	return helpers.EscapeHTML(s)
}
