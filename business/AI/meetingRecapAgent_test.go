package business

import (
	"strings"
	"testing"

	transcriptDomain "github.com/akashc777/OneCamp/domain/LiveKit"
)

func TestFormatTranscriptForRecap_BoundsAndSpeakers(t *testing.T) {
	names := map[string]string{
		"0x1": "alice",
		"0x2": "bob",
	}
	lines := []transcriptDomain.TranscriptLine{
		{ParticipantIdentity: "0x1", Text: "hello team", Timestamp: 1},
		{ParticipantIdentity: "0x2", Text: "let's ship friday", Timestamp: 2},
		{ParticipantIdentity: "0x1", Text: "", Timestamp: 3}, // empty skipped
		{ParticipantIdentity: "0x2", Text: "agreed", Timestamp: 4},
	}
	out, speakers := formatTranscriptForRecap(lines, names)

	if !strings.Contains(out, "alice: hello team") || !strings.Contains(out, "bob: let's ship friday") {
		t.Fatalf("formatted transcript missing expected lines:\n%s", out)
	}
	if strings.Contains(out, "alice: \n") {
		t.Fatalf("empty utterance should be skipped:\n%s", out)
	}
	// Distinct human speakers in first-seen order.
	if len(speakers) != 2 || speakers[0] != "0x1" || speakers[1] != "0x2" {
		t.Fatalf("unexpected speakers: %v", speakers)
	}
}

func TestFormatTranscriptForRecap_ExcludesAgentSpeaker(t *testing.T) {
	// The transcription agent identity must never be a candidate author.
	names := map[string]string{
		"transcriber-bot": "bot",
		"0x9":             "carol",
	}
	lines := []transcriptDomain.TranscriptLine{
		{ParticipantIdentity: "transcriber-bot", Text: "system line", Timestamp: 1},
		{ParticipantIdentity: "0x9", Text: "real human line", Timestamp: 2},
	}
	_, speakers := formatTranscriptForRecap(lines, names)
	for _, s := range speakers {
		if s == "transcriber-bot" {
			t.Fatalf("agent identity must be excluded from authors, got %v", speakers)
		}
	}
	if len(speakers) != 1 || speakers[0] != "0x9" {
		t.Fatalf("expected only the human speaker, got %v", speakers)
	}
}

func TestRecapToHTML_EscapesAndStructures(t *testing.T) {
	html := recapToHTML("**Summary**\n- shipped <script>alert(1)</script>", "", "", "")
	if strings.Contains(html, "<script>") {
		t.Fatalf("recap HTML must strip raw tags: %s", html)
	}
	if !strings.Contains(html, "Meeting Recap") {
		t.Fatalf("recap HTML must include the header: %s", html)
	}
	if !strings.Contains(html, "<p>") {
		t.Fatalf("recap HTML must be paragraph-structured: %s", html)
	}
	if !strings.HasPrefix(html, `<div data-ai-generated="true"><p>`) {
		t.Fatalf("the marker must wrap the paragraphs, not sit inside them: %s", html)
	}
}

func TestRecapToHTML_AppendsRecordingEmbed(t *testing.T) {
	// Channel surface: kind=channel + sid.
	attrs := recordingPlayAttrs("550e8400-e29b-41d4-a716-446655440000", "EG_123")
	html := recapToHTML("**Summary**\n- done", attrs, "", "")
	if !strings.Contains(html, `data-type="recording-embed"`) {
		t.Fatalf("recap HTML must contain the recording-embed node: %s", html)
	}
	if !strings.Contains(html, `data-egress="EG_123"`) || !strings.Contains(html, `data-kind="channel"`) {
		t.Fatalf("recap HTML must carry egress + kind attrs: %s", html)
	}

	// DM surface: kind=dm carries both participant ids.
	dmAttrs := recordingPlayAttrs("uuidA uuidB", "EG_9")
	if !strings.Contains(dmAttrs, `data-kind="dm"`) || !strings.Contains(dmAttrs, `data-u1="uuidA"`) || !strings.Contains(dmAttrs, `data-u2="uuidB"`) {
		t.Fatalf("dm recording attrs must carry both participants: %s", dmAttrs)
	}

	// No egress → no node appended.
	if got := recapToHTML("x", recordingPlayAttrs("c", ""), "", ""); strings.Contains(got, "recording-embed") {
		t.Fatalf("no egress id must not append a recording node: %s", got)
	}
}

func TestComposeRecapPrompt(t *testing.T) {
	// Blank instructions → base prompt verbatim.
	if got := composeRecapPrompt("   "); got != recapSystemPrompt {
		t.Fatalf("blank instructions should return the base prompt verbatim")
	}

	// Custom instructions are appended after the base rules.
	out := composeRecapPrompt("Always add a Risks section. Write in Spanish.")
	if !strings.HasPrefix(out, recapSystemPrompt) {
		t.Fatalf("base prompt must lead the composed prompt")
	}
	if !strings.Contains(out, "Additional workspace instructions") {
		t.Fatalf("composed prompt missing the custom-instructions delimiter: %q", out)
	}
	if !strings.Contains(out, "Always add a Risks section") {
		t.Fatalf("composed prompt missing the custom text")
	}
	// The base grounding rules remain present (custom can't remove them).
	if !strings.Contains(out, "SKIP_RECAP") {
		t.Fatalf("composed prompt dropped the base SKIP_RECAP contract")
	}
}

func TestComposeRecapPromptCaps(t *testing.T) {
	long := strings.Repeat("x", maxRecapInstructionsLen+500)
	out := composeRecapPrompt(long)
	// The appended custom portion is capped; the composed prompt is the base
	// plus a bounded suffix.
	if len(out) > len(recapSystemPrompt)+maxRecapInstructionsLen+200 {
		t.Fatalf("composed prompt exceeded the custom-instructions cap: %d", len(out))
	}
}

func TestRecapToHTMLLinksTheNotesDocument(t *testing.T) {
	// Without this link the document sits in the docs list with no path from
	// the call it describes, which is a document nobody finds.
	html := recapToHTML("Summary", "", "doc-abc-123", "")
	if !strings.Contains(html, `href="/app/doc/doc-abc-123"`) {
		t.Errorf("recap does not link the notes document:\n%s", html)
	}

	// No document, no link and no empty href pointing at the docs root.
	if got := recapToHTML("Summary", "", "", ""); strings.Contains(got, "/app/doc/") {
		t.Errorf("a recap with no document should carry no document link:\n%s", got)
	}

	// The id reaches an HTML attribute, so it is escaped like every other value
	// that does, even though it is a server-generated uuid today.
	if got := recapToHTML("Summary", "", `a" onmouseover="x`, ""); strings.Contains(got, `onmouseover="x`) {
		t.Errorf("the document id was not escaped into the attribute:\n%s", got)
	}
}

func TestRecapBodyHTMLRendersTheMarkdownThePromptAsksFor(t *testing.T) {
	// The prompt asks for "**📝 Summary**" headings and "- " bullets. Nothing
	// converted them, so every recap ever posted showed literal asterisks.
	recap := "**📝 Summary**\n- shipped the thing\n- fixed the other\n**✅ Decisions**\n- None"
	got := modelTextToHTML(recap)

	if strings.Contains(got, "**") {
		t.Errorf("literal asterisks survived into the output:\n%s", got)
	}
	if !strings.Contains(got, "<strong>📝 Summary</strong>") {
		t.Errorf("a whole-line bold heading was not rendered as one:\n%s", got)
	}
	if !strings.Contains(got, "<ul><li>shipped the thing</li>") {
		t.Errorf("bullets were not rendered as a list:\n%s", got)
	}
	// The list must close before the next heading, or the heading lands inside it.
	if !strings.Contains(got, "</ul><p><strong>✅ Decisions</strong></p>") {
		t.Errorf("the list did not close before the next heading:\n%s", got)
	}
	// A heading and its bullets separated by a blank line is the other shape the
	// prompt produces, and it must render the same way.
	spaced := modelTextToHTML("**📝 Summary**\n\n- one\n- two")
	if !strings.Contains(spaced, "<strong>📝 Summary</strong>") || !strings.Contains(spaced, "<ul><li>one</li>") {
		t.Errorf("a blank line between heading and bullets broke the render:\n%s", spaced)
	}
}

func TestRecapBodyHTMLEscapesBeforeAddingMarkup(t *testing.T) {
	// The recap is model output derived from meeting audio. Every tag in the
	// result must be one this function wrote.
	got := modelTextToHTML(`- <img src=x onerror="alert(1)"> and **bold**`)
	if strings.Contains(got, "<img") {
		t.Errorf("a tag from the model survived:\n%s", got)
	}
	if !strings.Contains(got, "<strong>bold</strong>") {
		t.Errorf("bold should still render after escaping:\n%s", got)
	}
}

func TestRecapBodyHTMLLeavesUnpairedAsterisksAlone(t *testing.T) {
	// A dangling <strong> would swallow the rest of the document, so an
	// unpaired ** stays the text it is.
	got := modelTextToHTML("a **dangling start")
	if strings.Contains(got, "<strong>") {
		t.Errorf("an unpaired ** opened a tag:\n%s", got)
	}
}

func TestModelTextToHTMLKeepsNewlinesApart(t *testing.T) {
	// The bug in the assistant's send-message tools: text was wrapped in a
	// single <p>, and HTML collapses newlines to spaces, so a message written
	// as several lines arrived as one run-on sentence.
	got := modelTextToHTML("First line.\nSecond line.\n\nA second block.")
	// Soft newlines become <br/> and a blank line starts a new paragraph, which
	// is exactly how renderParagraphs in business/BotPost already renders a
	// streamed reply. The same sentence must not look different depending on
	// which path produced the message.
	if !strings.Contains(got, "First line.<br/>Second line.") {
		t.Errorf("a soft newline did not become a line break:\n%s", got)
	}
	if n := strings.Count(got, "<p>"); n != 2 {
		t.Errorf("got %d paragraphs, want 2 (one per blank-line block):\n%s", n, got)
	}
	// The failure it replaces: everything collapsed onto one line.
	if strings.Contains(got, "First line. Second line.") {
		t.Errorf("lines were run together:\n%s", got)
	}
}

func TestModelTextToHTMLIsSafeForEmptyAndBlankInput(t *testing.T) {
	// A model can return nothing, or only whitespace. Neither should produce a
	// stray empty element in a channel.
	for _, in := range []string{"", "   ", "\n\n\n"} {
		if got := modelTextToHTML(in); got != "" {
			t.Errorf("modelTextToHTML(%q) = %q, want empty", in, got)
		}
	}
}

func TestRecapToHTMLCarriesTheTranscriptGap(t *testing.T) {
	// A recap that summarises half a conversation must say so. The gap is
	// invisible otherwise: silence and "could not be heard" look identical in a
	// transcript.
	got := recapToHTML("Summary", "", "", "Note: 1 person was on a browser that cannot transcribe speech.")
	if !strings.Contains(got, "cannot transcribe speech") {
		t.Errorf("the gap note is missing from the recap:\n%s", got)
	}

	// And silence when nothing is missing, which is the normal case: a caveat
	// on every recap teaches people to skip the caveat.
	if got := recapToHTML("Summary", "", "", ""); strings.Contains(got, "<em>") {
		t.Errorf("a complete transcript should carry no caveat:\n%s", got)
	}
}

func TestMarkAIGeneratedIsPresentOnEverySurface(t *testing.T) {
	// The AI Act wants the CONTENT marked, not only its author disclosed. Every
	// surface that emits machine-written text has to carry it, so this asserts
	// the marker at each producer rather than trusting one to imply the others.
	recap := recapToHTML("Summary", "", "", "")
	if !strings.Contains(recap, `data-ai-generated="true"`) {
		t.Errorf("the recap message is not marked:\n%s", recap)
	}

	// It wraps, so the marker is the outermost element and covers the parts
	// this code adds around the model's words, not only the model's words.
	if !strings.HasPrefix(recap, `<div data-ai-generated="true">`) {
		t.Errorf("the marker is not the outermost element:\n%s", recap)
	}
}

func TestMarkAIGeneratedLeavesNothingBehindForEmptyInput(t *testing.T) {
	// An empty body must not become an empty marked div: a message that says
	// "this was AI generated" and contains nothing is worse than no message.
	for _, in := range []string{"", "   "} {
		if got := MarkAIGenerated(in); strings.Contains(got, "data-ai-generated") {
			t.Errorf("MarkAIGenerated(%q) = %q, want it left alone", in, got)
		}
	}
}

// An agent that posts a ```chart block into a channel, DM or group chat gets a
// chart, not a fence: send_message, send_dm and send_group_chat all render
// through modelTextToHTML, and the prose around the chart keeps the rules
// every other model post follows.
func TestModelTextToHTMLDrawsCharts(t *testing.T) {
	in := "**Weekly**\n- 3 open\n\n```chart\n{\"type\":\"bar\",\"labels\":[\"Mon\"],\"series\":[{\"name\":\"Open\",\"values\":[3]}]}\n```\n\nThat is a &lt;fact&gt;."
	got := modelTextToHTML(in)
	for _, must := range []string{
		`<div data-type="chart" data-spec="{&#34;labels&#34;:[&#34;Mon&#34;]`,
		"<p><strong>Weekly</strong></p>",
		"<ul><li>3 open</li></ul>",
		// The prose is still escaped: an entity the model wrote stays an entity.
		"<p>That is a &amp;lt;fact&amp;gt;.</p>",
	} {
		if !strings.Contains(got, must) {
			t.Errorf("missing %q in %s", must, got)
		}
	}
	if strings.Contains(got, "```") {
		t.Errorf("a fence leaked: %s", got)
	}
}
