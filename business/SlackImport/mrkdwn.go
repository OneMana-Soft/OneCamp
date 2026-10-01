package business

import (
	"context"
	"regexp"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/microcosm-cc/bluemonday"
)

// renderMrkdwn converts Slack rich-text source (`text` field) into safe
// HTML suitable for storing in posts/chats/comments. Slack's syntax:
//
//   *bold*               → <strong>
//   _italic_             → <em>
//   ~strike~             → <del>
//   `code`               → <code>
//   ```code block```     → <pre><code>
//   <http://x|label>     → <a href="x">label</a>
//   <@U123|name>         → @mention (resolved via id_map)
//   <#C123|general>      → #channel link
//   <!here>, <!channel>  → highlighted spans
//   :emoji_name:         → emoji shortcode (left as-is; FE renders)
//
// Resolver callbacks let the caller plug in user/channel id_map lookups
// without coupling this file to the import context.

type mrkdwnResolvers struct {
	// resolveUser maps a Slack user id (U02ABC) to a OneCamp user UUID
	// string. Returning "" causes the renderer to fall back to the @name
	// supplied in the markup.
	resolveUser func(slackId string) (uuid string, displayName string, ok bool)
	// resolveChannel maps a Slack channel id (C03DEF) to a OneCamp channel
	// UUID string. Returning "" falls back to plain #name.
	resolveChannel func(slackId string) (uuid string, name string, ok bool)
}

var (
	// Pre-compiled at package init. Regex compilation is not free and these
	// are hot-path on every imported message.
	//
	// User-id prefixes Slack uses today: U (workspace user), W (Enterprise
	// Grid org-wide user), B (bot user). Channel prefixes: C (public),
	// G (legacy private; deprecated but still in old exports), D (DM).
	// We accept all of them to avoid silently dropping mentions in older
	// or Enterprise-Grid exports.
	reMrkdwnBold          = regexp.MustCompile(`(?m)\*([^\*\n]+)\*`)
	reMrkdwnItalic        = regexp.MustCompile(`(?m)_([^_\n]+)_`)
	reMrkdwnStrike        = regexp.MustCompile(`(?m)~([^~\n]+)~`)
	reMrkdwnCodeBlock     = regexp.MustCompile("(?s)```([^`]+)```")
	reMrkdwnInlineCode    = regexp.MustCompile("`([^`\\n]+)`")
	reSlackLink           = regexp.MustCompile(`<([^>|]+)\|([^>]+)>`)
	reSlackBareLink       = regexp.MustCompile(`<((?:https?://|mailto:)[^>|]+)>`)
	reSlackUserMention    = regexp.MustCompile(`<@([UWB][A-Z0-9]+)(?:\|([^>]+))?>`)
	reSlackChannelMention = regexp.MustCompile(`<#([CGD][A-Z0-9]+)(?:\|([^>]+))?>`)
	reSlackBroadcast      = regexp.MustCompile(`<!(here|channel|everyone)>`)
)

// htmlSanitizer is shared (bluemonday policies are safe for concurrent use
// and creating one is cheap but not free).
var htmlSanitizer = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	// We render @mention as <span data-id="user@<uuid>">; allow this
	// shape so the FE TipTap renderer picks it up the same way it does
	// for native posts.
	p.AllowAttrs("data-id").OnElements("span")
	p.AllowAttrs("class").OnElements("span", "code", "pre", "a")
	p.AllowURLSchemes("http", "https", "mailto")
	return p
}()

// RenderSlackText converts a SlackMessage's text+blocks to safe HTML.
// blocks (Block Kit) takes precedence; if blocks is non-empty we render
// from there. The text fallback is what most exports actually have.
//
// Stage ordering matters:
//  1. Replace Slack-specific tokens (<@U>, <#C>, <http|label>, <!here>)
//     BEFORE mrkdwn so a `_label_` inside a link doesn't get italicized.
//  2. Run mrkdwn on the result.
//  3. Sanitize. Sanitization is the last line of defence; never trust
//     export contents.
func RenderSlackText(ctx context.Context, m *SlackMessage, r mrkdwnResolvers) string {
	// We render from the message's `text` field. Slack always populates
	// `text` for human-typed messages even when `blocks` is also present
	// (the `text` is the textual fallback for clients that can't render
	// Block Kit). For app-bot messages composed exclusively in Block Kit
	// (interactive layouts, buttons, structured elements) this fallback
	// is also Slack's canonical text representation, so we get a
	// reasonable plain-text rendering without parsing the block tree.
	//
	// A deeper Block Kit→HTML renderer would let us preserve formatting
	// like section dividers and styled buttons; in practice the
	// downstream OneCamp post UI doesn't have analogous primitives, so
	// the textual fallback round-trips into a usable post body either way.
	src := m.Text
	if src == "" {
		return ""
	}

	// 1a. Resolve user mentions <@U123|name>.
	src = reSlackUserMention.ReplaceAllStringFunc(src, func(match string) string {
		sub := reSlackUserMention.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		slackId := sub[1]
		fallbackName := ""
		if len(sub) >= 3 {
			fallbackName = sub[2]
		}
		if r.resolveUser != nil {
			if uuidStr, dispName, ok := r.resolveUser(slackId); ok {
				if dispName == "" {
					dispName = fallbackName
				}
				if dispName == "" {
					dispName = "user"
				}
				// data-id="user@<uuid>" matches GetMentions() in helpers.go
				// which extracts the second component after "@" as the
				// dgraph user id. We use the postgres uuid here; the
				// existing renderer accepts both since it just stores the
				// mention as-is in HTML and re-resolves at read time.
				return `<span class="mention" data-id="user@` +
					htmlEscape(uuidStr) + `">@` + htmlEscape(dispName) + `</span>`
			}
		}
		if fallbackName != "" {
			return "@" + htmlEscape(fallbackName)
		}
		return "@unknown-user"
	})

	// 1b. Resolve channel mentions <#C123|general>.
	src = reSlackChannelMention.ReplaceAllStringFunc(src, func(match string) string {
		sub := reSlackChannelMention.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		slackId := sub[1]
		fallbackName := ""
		if len(sub) >= 3 {
			fallbackName = sub[2]
		}
		if r.resolveChannel != nil {
			if uuidStr, name, ok := r.resolveChannel(slackId); ok {
				if name == "" {
					name = fallbackName
				}
				if name == "" {
					name = "channel"
				}
				_ = uuidStr // future: deep-link via FE router
				return "#" + htmlEscape(name)
			}
		}
		if fallbackName != "" {
			return "#" + htmlEscape(fallbackName)
		}
		return "#unknown-channel"
	})

	// 1c. <!here|@here|channel|everyone> become a generic mention span.
	src = reSlackBroadcast.ReplaceAllStringFunc(src, func(match string) string {
		sub := reSlackBroadcast.FindStringSubmatch(match)
		if len(sub) >= 2 {
			return `<span class="mention broadcast">@` + sub[1] + `</span>`
		}
		return match
	})

	// 1d. Hyperlinks <http|label> and <http>.
	src = reSlackLink.ReplaceAllString(src, `<a href="$1" target="_blank" rel="noopener noreferrer">$2</a>`)
	src = reSlackBareLink.ReplaceAllString(src, `<a href="$1" target="_blank" rel="noopener noreferrer">$1</a>`)

	// 2. Mrkdwn formatting. Code blocks first so we don't *bold* inside them.
	src = reMrkdwnCodeBlock.ReplaceAllStringFunc(src, func(s string) string {
		// Strip the triple backticks and HTML-escape the contents.
		inner := strings.TrimPrefix(s, "```")
		inner = strings.TrimSuffix(inner, "```")
		return "<pre><code>" + htmlEscape(inner) + "</code></pre>"
	})
	src = reMrkdwnInlineCode.ReplaceAllStringFunc(src, func(s string) string {
		inner := strings.Trim(s, "`")
		return "<code>" + htmlEscape(inner) + "</code>"
	})
	src = reMrkdwnBold.ReplaceAllString(src, "<strong>$1</strong>")
	src = reMrkdwnItalic.ReplaceAllString(src, "<em>$1</em>")
	src = reMrkdwnStrike.ReplaceAllString(src, "<del>$1</del>")

	// 3. Sanitize. bluemonday strips anything we didn't allow above.
	clean := htmlSanitizer.Sanitize(src)

	if clean == "" && m.Text != "" {
		// Sanitizer ate everything (extremely unlikely but possible with
		// pathological inputs). Fall back to the escaped raw text so we
		// never lose content silently.
		helpers.LogWarnWithContext(ctx,
			"SlackImport.RenderSlackText sanitizer produced empty output ts=%s", m.Ts)
		return htmlEscape(m.Text)
	}
	return clean
}

// htmlEscape is a tiny wrapper around helpers.EscapeHTML so we don't
// import helpers throughout this file.
func htmlEscape(s string) string {
	return helpers.EscapeHTML(s)
}

// RenderMrkdwn renders the text of a LIVE Slack message (the bridge) to safe
// HTML. Live events carry bare <@U123> mentions with no name, so userName
// supplies one; a mention it cannot name renders as @someone rather than a
// raw id. Mentions stay plain text: a Slack person has no OneCamp account to
// link to.
func RenderMrkdwn(ctx context.Context, text string, userName func(slackID string) string) string {
	text = reSlackUserMention.ReplaceAllStringFunc(text, func(match string) string {
		sub := reSlackUserMention.FindStringSubmatch(match)
		if len(sub) >= 3 && sub[2] != "" {
			return match
		}
		name := ""
		if userName != nil {
			name = userName(sub[1])
		}
		if name == "" {
			name = "someone"
		}
		return "<@" + sub[1] + "|" + name + ">"
	})
	return RenderSlackText(ctx, &SlackMessage{Text: text}, mrkdwnResolvers{})
}
