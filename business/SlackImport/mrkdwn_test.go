package business

import (
	"context"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// render is RenderSlackText of text with r, and <br/> read as <br>.
func render(t *testing.T, text string, r mrkdwnNames) string {
	t.Helper()
	return strings.ReplaceAll(RenderSlackText(context.Background(), &SlackMessage{Text: text, Ts: "1.1"}, r), "<br/>", "<br>")
}

// A message's lines stay lines and its paragraphs stay paragraphs: the editor
// collapses raw newlines, so a message of five lines used to arrive as one.
func TestSlackTextKeepsItsLinesAndParagraphs(t *testing.T) {
	got := render(t, "one\ntwo\n\n\nthree  \n", mrkdwnNames{})
	if want := "<p>one<br>two</p><p>three</p>"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
	if got := render(t, "   \n  ", mrkdwnNames{}); got != "" {
		t.Errorf("only spaces: %q", got)
	}
}

// Code is left exactly as written: no emphasis inside a span or a block, and
// what Slack escaped reads as its characters.
func TestSlackCodeIsLeftAsWritten(t *testing.T) {
	got := render(t, "use `my_var_name` and *x*\n```\nif a_b_c &lt; 2 {\n  run(*p*)\n}\n```\nafter", mrkdwnNames{})
	for _, want := range []string{
		"<code>my_var_name</code>",
		"<strong>x</strong>",
		"<pre><code>if a_b_c &lt; 2 {\n  run(*p*)\n}</code></pre>",
		"<p>after</p>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "<em>") {
		t.Errorf("emphasis inside code: %q", got)
	}
	// An odd fence opens nothing.
	if got := render(t, "a ``` b", mrkdwnNames{}); got != "<p>a ``` b</p>" {
		t.Errorf("an odd fence: %q", got)
	}
}

// Emphasis applies where Slack applies it, between word boundaries.
func TestSlackEmphasisKeepsToWordBoundaries(t *testing.T) {
	got := render(t, "snake_case_name and 2*3*4 stay; _this_ and *that* and ~gone~ don't, nor * this *", mrkdwnNames{})
	want := "<p>snake_case_name and 2*3*4 stay; <em>this</em> and <strong>that</strong> and <del>gone</del> don&#39;t, nor * this *</p>"
	if got != want {
		t.Errorf("%q\nwant %q", got, want)
	}
	if got := render(t, "*_both_*", mrkdwnNames{}); got != "<p><strong><em>both</em></strong></p>" {
		t.Errorf("nested: %q", got)
	}
}

// A link keeps its address whole, however many underscores it has.
func TestSlackLinksKeepTheirAddresses(t *testing.T) {
	got := render(t, "see <https://example.com/a_b_c?x=1&amp;y=2|the _docs_> and <https://x.dev> and <mailto:ada@x.dev|Ada>", mrkdwnNames{})
	for _, want := range []string{
		`href="https://example.com/a_b_c?x=1&amp;y=2"`, ">the _docs_</a>",
		`href="https://x.dev"`, ">https://x.dev</a>",
		`href="mailto:ada@x.dev"`, ">Ada</a>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "<em>") {
		t.Errorf("emphasis inside a link: %q", got)
	}
}

// Slack's other tokens are their words, never a link to nowhere, and nothing
// unsafe gets through.
func TestSlackTokensReadAsWords(t *testing.T) {
	got := render(t, "ask <!subteam^S1|@design>, by <!date^1392734382^{date_short}|Feb 18, 2014>, <slack://open|open Slack>, <javascript:alert(1)|click>, &lt;script&gt;x&lt;/script&gt;", mrkdwnNames{})
	want := "<p>ask @design, by Feb 18, 2014, open Slack, click, &lt;script&gt;x&lt;/script&gt;</p>"
	if got != want {
		t.Errorf("%q\nwant %q", got, want)
	}
}

// A mention of someone who came across is OneCamp's own chip, whose graph uid
// the edit path reads (helpers.GetMentions); anyone else is their Slack name.
// A broadcast is the composer's chip, and a channel that came across its own.
func TestSlackMentionsAreOneCampChips(t *testing.T) {
	r := mrkdwnNames{
		people:   map[string]mentionee{"U1": {UUID: "11111111-1111-4111-8111-111111111111", UID: "0x1", Name: `Ada "A" Lovelace`}, "U4": {}},
		slack:    map[string]string{"U2": "Bo"},
		channels: map[string]channelChip{"C1": {UUID: "c-1", Name: "launch"}, "C9": {}},
	}
	got := render(t, "<@U1> and <@U2> and <@U3|cy> and <@U4>: <!here> in <#C1> and <#C9|old>", r)
	for _, want := range []string{
		`<span class="mention" data-type="mention" data-id="11111111-1111-4111-8111-111111111111@0x1" data-label="Ada &#34;A&#34; Lovelace">@Ada &#34;A&#34; Lovelace</span>`,
		" and @Bo and @cy and @someone: ",
		`<span class="mention" data-type="mention" data-id="broadcast:here" data-label="here">@here</span>`,
		`<span class="channel-mention" data-type="channelMention" data-id="channel:c-1" data-label="launch">#launch</span>`,
		" and #old",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q\nlacks %q", got, want)
		}
	}
	if uids, _ := helpers.GetMentions(got); len(uids) != 1 || uids[0] != "0x1" {
		t.Errorf("the edit path reads %v as the people named, want [0x1]", uids)
	}
	// Emphasis around a chip wraps the chip.
	if got := render(t, "*<@U1>*", r); !strings.HasPrefix(got, "<p><strong><span class=\"mention\"") {
		t.Errorf("bold chip: %q", got)
	}
}

// "> " lines are a quote, and ">>> " quotes the rest.
func TestSlackQuotes(t *testing.T) {
	got := render(t, "&gt; quoted\n&gt;more\nnot quoted\n&gt;&gt;&gt; all\nof this", mrkdwnNames{})
	want := "<blockquote><p>quoted<br>more</p></blockquote><p>not quoted</p><blockquote><p>all<br>of this</p></blockquote>"
	if got != want {
		t.Errorf("%q\nwant %q", got, want)
	}
}

// An app's message is led by the app's name in plain words: no chip for the
// edit path to fail on.
func TestASlackAppsMessageIsLedByItsName(t *testing.T) {
	m := &SlackMessage{Subtype: "bot_message", BotProfile: &SlackBotProfile{Name: "GitHub"}, Text: "PR merged\nby Ada"}
	got := strings.ReplaceAll(withBotLead(m, RenderSlackText(context.Background(), m, mrkdwnNames{})), "<br/>", "<br>")
	if want := `<p><span class="bot-attrib">[bot: GitHub]</span> PR merged<br>by Ada</p>`; got != want {
		t.Errorf("%q\nwant %q", got, want)
	}
	if strings.Contains(got, "data-id") {
		t.Errorf("a chip in the lead: %q", got)
	}
	code := &SlackMessage{Subtype: "bot_message", Username: "ci", Text: "```ok```"}
	if got := withBotLead(code, RenderSlackText(context.Background(), code, mrkdwnNames{})); got != `<p><span class="bot-attrib">[bot: ci]</span></p><pre><code>ok</code></pre>` {
		t.Errorf("before a block: %q", got)
	}
	person := &SlackMessage{User: "U1", Text: "hi"}
	if got := withBotLead(person, "<p>hi</p>"); got != "<p>hi</p>" {
		t.Errorf("a person's message: %q", got)
	}
}

// The bridge's live messages name their people, who have no OneCamp chips.
func TestRenderMrkdwnNamesLivePeople(t *testing.T) {
	got := strings.ReplaceAll(RenderMrkdwn(context.Background(), "hi <@U1>\nbye <@U2>", func(id string) string {
		if id == "U1" {
			return "Ada"
		}
		return ""
	}), "<br/>", "<br>")
	if want := "<p>hi @Ada<br>bye @someone</p>"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

// The export's names: display name, else real name, else handle.
func TestSlackNameBook(t *testing.T) {
	book := slackNameBook([]SlackUser{
		{ID: "U1", Name: "ada", RealName: "Ada L", Profile: SlackUserProfile{DisplayName: "Ada"}},
		{ID: "U2", Name: "bo", Profile: SlackUserProfile{RealName: "Bo B"}},
		{ID: "U3", Name: "cy"},
	})
	if book["U1"] != "Ada" || book["U2"] != "Bo B" || book["U3"] != "cy" {
		t.Errorf("%v", book)
	}
}

// A message's mentions are found before it is rendered, each once.
func TestMentionedIDs(t *testing.T) {
	users, channels := mentionedIDs("<@U1> and <@U2|bo>, <@U1> in <#C1|launch> and <#C2>, <https://x.dev>, <!here> and <@U3")
	if strings.Join(users, ",") != "U1,U2" || strings.Join(channels, ",") != "C1,C2" {
		t.Errorf("users %v, channels %v", users, channels)
	}
}

// A /me message reads as Slack shows it, in italics.
func TestASlackMeMessageIsInItalics(t *testing.T) {
	me := &SlackMessage{Subtype: "me_message", Text: "is out this afternoon"}
	if got := asMeMessage(me, "<p>is out this afternoon</p>"); got != "<p><em>is out this afternoon</em></p>" {
		t.Errorf("%q", got)
	}
	if got := asMeMessage(&SlackMessage{Text: "hi"}, "<p>hi</p>"); got != "<p>hi</p>" {
		t.Errorf("a plain message: %q", got)
	}
	if got := asMeMessage(me, "<p>a</p><p>b</p>"); got != "<p>a</p><p>b</p>" {
		t.Errorf("two paragraphs: %q", got)
	}
}
