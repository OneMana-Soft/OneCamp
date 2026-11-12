package contentcodec

import (
	"slices"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

const (
	ana = "0a0a0a0a-0000-4000-8000-00000000000a"
	bo  = "0b0b0b0b-0000-4000-8000-00000000000b"
	cy  = "0c0c0c0c-0000-4000-8000-00000000000c"
)

// The people text names as @[uuid]: each once, lower case, in the order
// first written; anything else that looks like one is no one.
func TestMentionTokensAreThePeopleTextNames(t *testing.T) {
	got := MentionTokens("@[" + strings.ToUpper(bo) + "] and @[" + ana + "], @[" + bo + "] again, @[not-a-uuid], @[], @" + cy)
	if !slices.Equal(got, []string{bo, ana}) {
		t.Fatalf("got %v, want [bo ana]", got)
	}
	if got := MentionTokens("no one"); got != nil {
		t.Fatalf("no tokens read %v", got)
	}
	if MentionToken(strings.ToUpper(cy)) != "@["+cy+"]" {
		t.Fatalf("token %q", MentionToken(strings.ToUpper(cy)))
	}
}

func docOf(t *testing.T, h string) *Node {
	t.Helper()
	doc, err := FromHTML(h, HTMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// Each @[uuid] of someone known becomes the chip the app's composer writes,
// which helpers.GetMentions reads, in paragraphs and lists alike; anyone
// unknown, and a token in code, stays text. Written back, the chips are the
// text as it was, and say who they were, each once.
func TestMentionsAreWrittenAsTheAppsChipsAndReadBackAsTokens(t *testing.T) {
	people := map[string]Mention{
		ana: {UUID: ana, UID: "0x1a", Label: "Ana <A&B>"},
		bo:  {UUID: bo, UID: "0x1b", Label: "Bo"},
	}
	text := "Ask @[" + strings.ToUpper(ana) + "] and @[" + bo + "], not @[" + cy + "]\n\n- @[" + bo + "] owns it"
	plain := helpers.PlainTextToHTML(text)
	doc := docOf(t, plain)
	doc.Content = append(doc.Content, &Node{Type: "paragraph", Content: []*Node{{Type: "text", Text: "@[" + ana + "]", Marks: []*Mark{{Type: "code"}}}}})
	h := ToHTML(WriteMentions(doc, people))

	if want := `<span data-type="mention" data-id="` + ana + `@0x1a" data-label="Ana <A&amp;B>" data-mention-suggestion-char="@">@Ana &lt;A&amp;B&gt;</span>`; !strings.Contains(h, want) {
		t.Fatalf("Ana's chip isn't the composer's:\n%s\nwant %s", h, want)
	}
	if strings.Count(h, `data-id="`+bo+`@0x1b"`) != 2 || !strings.Contains(h, "not @["+cy+"]") || !strings.Contains(h, "<code>@["+ana+"]</code>") {
		t.Fatalf("Bo twice, Cy and the code as text, want them in:\n%s", h)
	}
	uids, err := helpers.GetMentions(h)
	slices.Sort(uids)
	if err != nil || !slices.Equal(uids, []string{"0x1a", "0x1b"}) {
		t.Fatalf("GetMentions read %v (%v), want Ana's and Bo's nodes", uids, err)
	}

	back := docOf(t, h)
	found := MentionsAsTokens(back)
	if want := []Mention{{UUID: ana, UID: "0x1a", Label: "Ana <A&B>"}, {UUID: bo, UID: "0x1b", Label: "Bo"}}; !slices.Equal(found, want) {
		t.Fatalf("found %+v, want %+v", found, want)
	}
	if got, want := ToHTML(back), ToHTML(docOf(t, strings.Replace(plain, strings.ToUpper(ana), ana, 1)))+"<p><code>@["+ana+"]</code></p>"; got != want {
		t.Fatalf("read back as\n%s\nwant\n%s", got, want)
	}
}

// Only a person's mention is written back: a channel's, a team's ("group:…",
// no "<uuid>@") and one whose id isn't a uuid stay chips. Nothing known
// changes nothing.
func TestOnlyAPersonsMentionIsWrittenBackAsAToken(t *testing.T) {
	h := `<p><span data-type="channelMention" data-id="channel:` + cy + `" data-label="general">#general</span> ` +
		`<span data-type="mention" data-id="group:` + cy + `" data-label="Platform">@Platform</span> ` +
		`<span data-type="mention" data-id="bob@0x9" data-label="Bob">@Bob</span></p>`
	doc := docOf(t, h)
	want := ToHTML(doc)
	if found := MentionsAsTokens(doc); len(found) != 0 || ToHTML(doc) != want {
		t.Fatalf("found %+v and wrote %s, want nothing changed", found, ToHTML(doc))
	}
	plain := docOf(t, "<p>Ask @["+bo+"]</p>")
	if ToHTML(WriteMentions(plain, nil)) != "<p>Ask @["+bo+"]</p>" || ToHTML(WriteMentions(plain, map[string]Mention{bo: {UUID: bo}})) != "<p>Ask @["+bo+"]</p>" {
		t.Fatalf("no one known, or no node for them, wrote %s", ToHTML(plain))
	}
}

// CR-221's M1: an @[uuid] in a link's text or in a code block stays text, as
// WriteMentions says: a link keeps its words, and code is written as it is.
func TestAMentionInALinkOrACodeBlockStaysText(t *testing.T) {
	people := map[string]Mention{bo: {UUID: bo, UID: "0x1b", Label: "Bo"}}
	for name, h := range map[string]string{
		"a link's text": `<p><a href="https://example.test">@[` + bo + `]</a></p>`,
		"a code block":  `<pre><code>@[` + bo + `]</code></pre>`,
	} {
		want := ToHTML(docOf(t, h))
		if got := ToHTML(WriteMentions(docOf(t, h), people)); got != want || strings.Contains(got, `data-type="mention"`) {
			t.Errorf("in %s, wrote %s, want it as it was: %s", name, got, want)
		}
	}
}
