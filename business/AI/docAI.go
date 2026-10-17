package business

// Doc AI (docs programme B9): the actions the doc's AI bar runs, what each one
// reads, and how its answer is cleaned before it reaches the doc.
//
// THREE KINDS OF ACTION.
//   - A transform rewrites the selection (improve, fix, shorter, longer, tone,
//     translate, simplify). The web app shows its answer as a proposed change
//     against the selection (A9's proposals): a word diff, Accept or Discard.
//   - A writing action adds text at the caret (continue, write, draft, a
//     summary, a draft from a project's open tasks). The web app shows it as
//     ghost text until it is accepted.
//   - An answer is read, not written (explain, ask about this doc). The web app
//     shows it in the bar, and it goes into the doc only if asked.
//
// EVERY ANSWER IS MARKDOWN with nothing around it. Small local models (the demo
// runs qwen3:4b-instruct) add a line saying what they are about to do, and a
// line offering more help after it, however the prompt asks. Both are removed
// here, by rules that do the same thing every time (CleanDocAnswer), never by
// asking the model again. A line that was in the person's own text is never
// taken for one: "Revised timeline:" heading a selection stays.
//
// A DOC IS READ AS ITS LIVE COPY. The whole-doc actions read the doc the editor
// sends, typing included, or else the collaboration service's live copy, never
// the stored body, which can be a save behind. The stored body is the fallback
// when the service can't answer.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/services/collab"
)

// DocActionKind is what an action's answer is for.
type DocActionKind string

const (
	DocActionTransform DocActionKind = "transform"
	DocActionWrite     DocActionKind = "write"
	DocActionAnswer    DocActionKind = "answer"
)

// DocAction is one action the AI bar runs.
type DocAction struct {
	Name string
	Kind DocActionKind
	// NeedsText: the action works on the selection.
	NeedsText bool
	// NeedsPrompt: the action needs the person's instruction or question.
	NeedsPrompt bool
	// ReadsDoc: the action reads the whole doc (ask about it), so a doc the
	// person can read is required.
	ReadsDoc bool
}

var docActions = map[string]DocAction{
	// A typed instruction for the selection ("make it a table").
	"edit":             {Name: "edit", Kind: DocActionTransform, NeedsText: true, NeedsPrompt: true},
	"improve":          {Name: "improve", Kind: DocActionTransform, NeedsText: true},
	"fix":              {Name: "fix", Kind: DocActionTransform, NeedsText: true},
	"shorter":          {Name: "shorter", Kind: DocActionTransform, NeedsText: true},
	"longer":           {Name: "longer", Kind: DocActionTransform, NeedsText: true},
	"tone":             {Name: "tone", Kind: DocActionTransform, NeedsText: true},
	"translate":        {Name: "translate", Kind: DocActionTransform},
	"simplify":         {Name: "simplify", Kind: DocActionTransform, NeedsText: true},
	"explain":          {Name: "explain", Kind: DocActionAnswer, NeedsText: true},
	"summarize":        {Name: "summarize", Kind: DocActionWrite},
	"continue":         {Name: "continue", Kind: DocActionWrite},
	"write":            {Name: "write", Kind: DocActionWrite, NeedsPrompt: true},
	"draft":            {Name: "draft", Kind: DocActionWrite, NeedsPrompt: true},
	"draft_from_tasks": {Name: "draft_from_tasks", Kind: DocActionWrite},
	"ask":              {Name: "ask", Kind: DocActionAnswer, NeedsPrompt: true, ReadsDoc: true},
}

// legacyDocActions are the names the old AI panel sent, kept so a tab opened
// before an upgrade still works.
var legacyDocActions = map[string]string{
	"rewrite":     "improve",
	"fix_grammar": "fix",
	"shorten":     "shorter",
	"expand":      "longer",
}

// LookupDocAction is the action called name (or an older name for it).
func LookupDocAction(name string) (DocAction, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if to, ok := legacyDocActions[name]; ok {
		name = to
	}
	a, ok := docActions[name]
	return a, ok
}

// DocTones are the tones "Change tone" offers, with what each means to the model.
var DocTones = map[string]string{
	"professional": "professional tone: precise, polished and courteous, with no slang",
	"casual":       "casual tone: relaxed and conversational, as one colleague writes to another",
	"direct":       "direct tone: plain and to the point, with no hedging, filler or pleasantries",
	"confident":    "confident tone: assured and decisive, stating things plainly without hedging",
	"friendly":     "friendly tone: warm and approachable, while keeping every fact",
}

// languageName is a language as a person names it ("French", "Brazilian
// Portuguese", "zh-Hant"). Anything else is refused, so the field can't carry
// an instruction into the prompt.
var languageName = regexp.MustCompile(`^[\p{L}][\p{L} ()'-]{0,38}[\p{L})]$`)

// ValidDocLanguage is whether s names a language the way languageName allows.
func ValidDocLanguage(s string) bool { return languageName.MatchString(strings.TrimSpace(s)) }

// DocAIInput is what an action is run on.
type DocAIInput struct {
	Action   DocAction
	Text     string // the selection, as Markdown
	Prompt   string // the person's instruction, refinement or question
	Context  string // the text around the selection or before the caret
	Tone     string
	Language string
	// DocTitle and DocMarkdown are the doc's title and its body as Markdown,
	// for the actions that read the doc.
	DocTitle    string
	DocMarkdown string
	// Previous is the answer being refined ("make it warmer").
	Previous string
	// Tasks are a project's open tasks, for draft_from_tasks.
	Tasks string
	// Parts says DocMarkdown holds the summaries of a long doc's parts (the
	// map step's answer), for the summary written from them.
	Parts bool
}

const docAnswerRules = "Write Markdown only. Begin with the result itself: no line introducing it, no title saying what it is, and no note, question or offer after it."

// DocSystemPrompt is the system prompt for an action.
func DocSystemPrompt(in DocAIInput) string {
	var task string
	switch in.Action.Name {
	case "edit":
		task = "Change the text as the instruction asks. Keep everything the instruction doesn't ask to change: its facts, names, numbers, links, language and Markdown structure."
	case "improve":
		task = "Rewrite the text so it reads better: clearer, tighter, with better flow. Keep its meaning, facts, names, numbers, links, language and Markdown structure (headings and lists stay headings and lists)."
	case "fix":
		task = "Correct the spelling, grammar and punctuation of the text. Change nothing else: keep its words where they are right, its tone, its language and its Markdown structure."
	case "shorter":
		task = "Make the text shorter, about half its length, keeping its key points, facts and names, its language and its Markdown structure."
	case "longer":
		task = "Make the text longer, about twice its length, by adding useful detail, examples or explanation in the same voice and language. Keep its Markdown structure."
	case "tone":
		task = "Rewrite the text in a " + DocTones[in.Tone] + ". Keep every fact, name and number, its language and its Markdown structure."
	case "translate":
		task = "Translate the text into " + strings.TrimSpace(in.Language) + ". Keep its Markdown structure, and leave names, numbers, links and code as they are."
	case "simplify":
		task = "Rewrite the text in simpler language: short sentences and everyday words, with no jargon. Keep every fact, its language and its Markdown structure."
	case "explain":
		task = "Explain the text in plain words to someone new to the subject: what it means and why it matters, in a few short paragraphs or a short list."
	case "summarize":
		task = "Summarise the text: one short paragraph that says what it is about, then its key points, decisions and numbers as a bulleted list. Keep at least one key fact from every section."
		if in.Parts {
			task = "Below are summaries of a long document's parts, in order. Write one summary of the whole document from them: one short paragraph that says what it is about, then its key points, decisions and numbers as a bulleted list. Keep at least one key fact from every part."
		}
	case "continue":
		task = "Continue the document from exactly where it stops. Match its voice, tense, language and format (if it stops inside a list, continue the list). Write the next one to three paragraphs or items. Do not repeat anything already written."
	case "write", "draft":
		task = "Write what the instruction asks for, as part of a document. Use headings, lists and tables where they help the reader, and write in the language of the instruction."
	case "draft_from_tasks":
		task = "Write a plan document from the project's open tasks: a short overview, then the work grouped by theme or by status as headings with bulleted lists, naming each task's owner and due date where one is given. Use only the tasks listed."
	case "ask":
		task = "Answer the question using only the document. If the document doesn't say, answer exactly: The doc doesn't say. Quote short phrases from it where that helps."
	}
	if in.Action.Kind == DocActionAnswer {
		return "You are the writing assistant inside a document editor. " + task + " Answer in Markdown, starting with the answer itself, with no note or offer after it."
	}
	return "You are the writing assistant inside a document editor. " + task + " " + docAnswerRules
}

// DocUserContent is the user message for an action.
func DocUserContent(in DocAIInput) string {
	var b strings.Builder
	section := func(title, body string) {
		body = strings.TrimSpace(body)
		if body == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(title)
		b.WriteString(":\n")
		b.WriteString(body)
	}
	switch in.Action.Name {
	case "continue":
		section("The document so far, up to where the writer stopped", helpers.FirstNonBlank(in.Text, in.Context, in.DocMarkdown))
	case "write", "draft":
		section("Instruction", in.Prompt)
		section("The document's title", in.DocTitle)
		section("The document so far, for context (do not repeat it)", helpers.FirstNonBlank(in.Context, in.DocMarkdown))
	case "summarize":
		if in.Parts {
			section("The document's title", in.DocTitle)
			section("Summaries of its parts", in.DocMarkdown)
		} else {
			section("Text", helpers.FirstNonBlank(in.Text, in.DocMarkdown))
		}
		if in.Previous == "" {
			section("Also", in.Prompt)
		}
	case "draft_from_tasks":
		section("Instruction", helpers.FirstNonBlank(in.Prompt, "Draft a plan from these tasks."))
		section("Open tasks", in.Tasks)
	case "ask":
		section("Question", in.Prompt)
		section("The part of the document the question is about", in.Text)
		section("Document", in.DocMarkdown)
	default:
		if in.Prompt != "" && in.Previous == "" {
			section("Instruction", in.Prompt)
		}
		section("Text", helpers.FirstNonBlank(in.Text, in.DocMarkdown))
		section("The text around it, for tone only (do not rewrite or repeat it)", in.Context)
	}
	if in.Previous != "" {
		section("Your previous answer", in.Previous)
		section("Change it as asked, and answer with the whole new version", in.Prompt)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Cleaning an answer
// ---------------------------------------------------------------------------

// preambleLead is the start of a first line that announces an answer instead of
// being part of it, when the line ends in a colon: "Sure! Here's the improved
// version:", "Rewritten text:".
var preambleLead = regexp.MustCompile(`(?i)^(sure|certainly|of course|okay|ok|absolutely|alright|great|here\b|here's|here’s|below\b|the following|this is|i've|i’ve|i have|rewritten|revised|improved|corrected|fixed|edited|polished|shortened|shorter|condensed|longer|expanded|simplified|translated|translation|summary|summarised|summarized|continued|continuation|draft|result|output|answer)\b`)

// chattyLead is a first line that is only talk even without a colon ("Sure,
// here's a shorter version."), when a blank line and the answer follow it.
var chattyLead = regexp.MustCompile(`(?i)^(sure|certainly|of course|absolutely|okay|ok|alright|here is|here's|here’s|here are|below is|below are)\b`)

// chatterTail is a closing line that offers more help instead of being part of
// the answer: "Let me know if you'd like any changes!".
var chatterTail = regexp.MustCompile(`(?i)^(let me know|i hope this|hope this helps|feel free to|if you('d| would) like (me|any|a|another)|would you like (me|a|another)|i can (also )?(help|make|adjust|provide|tweak|expand|shorten)|is there anything else|happy to help)`)

// wholeFence is an answer that is one fenced block, and its language tag.
var wholeFence = regexp.MustCompile("(?s)^```([a-zA-Z0-9_+-]*)[ \t]*\n(.*?)\n?```$")

// preambleHoldRunes is how much of a first line is held back waiting for its
// end: a preamble is short, so a longer first line is content.
const preambleHoldRunes = 160

// CleanDocAnswer removes what a model wraps around an answer: a code fence
// around the whole of it (unless it is code the person asked for), quotes
// around the whole of it, a first line that introduces it, and a last line
// that offers more help. input is the text the action ran on: a line that is
// in it is the person's own and stays, wherever it stands. Pure: the same
// answer is always cleaned the same way.
func CleanDocAnswer(answer, input string) string {
	s := strings.TrimSpace(StripReasoning(answer))
	s = unwrapFence(s, input)
	lines := strings.Split(s, "\n")
	if len(lines) > 1 && isPreambleAt(lines, input) {
		lines = lines[1:]
		for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
	}
	for len(lines) > 1 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" || last == "---" || (isChatterLine(last) && !inInput(last, input)) {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	out = unwrapFence(out, input)
	return unwrapQuotes(out, input)
}

// unwrapFence takes away a fence around the whole answer when it holds prose:
// tagged as Markdown or text, or untagged when the person's text had no code.
func unwrapFence(s, input string) string {
	m := wholeFence.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	switch strings.ToLower(m[1]) {
	case "markdown", "md", "text", "plaintext":
	case "":
		if strings.Contains(input, "```") {
			return s
		}
	default:
		return s
	}
	return strings.TrimSpace(m[2])
}

var quotePairs = [][2]string{{`"`, `"`}, {"“", "”"}, {"'", "'"}, {"‘", "’"}}

// unwrapQuotes takes away quotes around a one-paragraph answer that the
// person's text did not have.
func unwrapQuotes(s, input string) string {
	if strings.Contains(s, "\n\n") {
		return s
	}
	in := strings.TrimSpace(input)
	for _, q := range quotePairs {
		if len(s) > len(q[0])+len(q[1]) && strings.HasPrefix(s, q[0]) && strings.HasSuffix(s, q[1]) &&
			!(strings.HasPrefix(in, q[0]) && strings.HasSuffix(in, q[1])) {
			inner := s[len(q[0]) : len(s)-len(q[1])]
			if !strings.Contains(inner, q[0]) && !strings.Contains(inner, q[1]) {
				return strings.TrimSpace(inner)
			}
		}
	}
	return s
}

// isPreambleAt is whether lines[0] only introduces what follows it.
func isPreambleAt(lines []string, input string) bool {
	first := lines[0]
	if inInput(first, input) {
		return false
	}
	if isPreambleLine(first) {
		return true
	}
	// A chatty line with no colon counts only when a blank line and more
	// content follow it, as a model writes "Sure, here it is." then the answer.
	t := bareLine(first)
	if t == "" || utf8.RuneCountInString(t) > 120 || !chattyLead.MatchString(t) {
		return false
	}
	if len(lines) < 3 || strings.TrimSpace(lines[1]) != "" {
		return false
	}
	for _, l := range lines[2:] {
		if strings.TrimSpace(l) != "" {
			return true
		}
	}
	return false
}

// bareLine is a line without Markdown emphasis or heading marks around it.
func bareLine(line string) string {
	t := strings.TrimSpace(line)
	t = strings.TrimLeft(t, "#> ")
	return strings.Trim(t, "*_ ")
}

// isPreambleLine: a short line, ending in a colon, that starts like an
// announcement.
func isPreambleLine(line string) bool {
	t := bareLine(line)
	if t == "" || utf8.RuneCountInString(t) > 140 {
		return false
	}
	if !strings.HasSuffix(strings.TrimRight(t, "*_ "), ":") {
		return false
	}
	return preambleLead.MatchString(t)
}

func isChatterLine(line string) bool {
	return chatterTail.MatchString(bareLine(line))
}

// inInput is whether a line, as words, is in the person's own text.
func inInput(line, input string) bool {
	l := strings.ToLower(collapseSpaces(bareLine(line)))
	l = strings.TrimRight(l, ":")
	if l == "" || input == "" {
		return false
	}
	return strings.Contains(strings.ToLower(collapseSpaces(input)), l)
}

// DocPreambleFilter cleans a streamed answer as it arrives: it holds the start
// back until the first line is whole and the next one has begun, and drops the
// first line when it only introduces the answer. Everything after passes
// through at once. A closing offer can't be taken back once sent, so the caller
// compares what it sent with CleanDocAnswer of the whole answer at the end and
// sends the clean version when they differ.
type DocPreambleFilter struct {
	// Input is the text the action ran on (CleanDocAnswer's input).
	Input     string
	head      strings.Builder
	decided   bool
	afterDrop bool
}

// Feed returns what can be shown of the answer so far.
func (f *DocPreambleFilter) Feed(s string) string {
	if f.decided {
		return f.passAfterDrop(s)
	}
	f.head.WriteString(s)
	held := strings.TrimLeft(f.head.String(), " \t\r\n")
	nl := strings.Index(held, "\n")
	if nl < 0 {
		if utf8.RuneCountInString(held) < preambleHoldRunes {
			return ""
		}
		f.decided = true
		return held
	}
	// The first line is whole; wait for the next content (past a blank line)
	// so a chatty line without a colon can be told apart.
	rest := held[nl+1:]
	if strings.TrimSpace(rest) == "" && utf8.RuneCountInString(held) < preambleHoldRunes*2 {
		return ""
	}
	f.decided = true
	lines := strings.Split(held, "\n")
	if isPreambleAt(lines, f.Input) {
		f.afterDrop = true
		return f.passAfterDrop(rest)
	}
	return held
}

func (f *DocPreambleFilter) passAfterDrop(s string) string {
	if !f.afterDrop {
		return s
	}
	t := strings.TrimLeft(s, " \t\r\n")
	if t == "" {
		return ""
	}
	f.afterDrop = false
	return t
}

// Flush returns what is still held when the answer ends.
func (f *DocPreambleFilter) Flush() string {
	if f.decided {
		return ""
	}
	f.decided = true
	return strings.TrimLeft(f.head.String(), " \t\r\n")
}

// ---------------------------------------------------------------------------
// Reading a doc
// ---------------------------------------------------------------------------

// A doc's blocks are read by the content codec (contentcodec.Blocks): the
// top-level ones where an agent writes, the lines "Find action items" numbers.

func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

// DocMarkdown is a doc's HTML as Markdown, the form a model reads best. HTML
// the parser can't read at all has no text to give.
func DocMarkdown(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	doc, err := contentcodec.FromHTML(src, contentcodec.HTMLOptions{})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(contentcodec.ToMarkdown(doc, contentcodec.MarkdownOptions{}))
}

// DocHTMLFromMarkdown is Markdown as the editor's HTML, through the content
// codec the editor's own import and paste use (A7), so what an agent writes
// reads exactly as the same Markdown pasted by a person would.
func DocHTMLFromMarkdown(md string) string {
	doc, err := contentcodec.FromMarkdown(md, nil)
	if err != nil || doc == nil {
		return docMarkdownToHTML(md)
	}
	return contentcodec.ToHTML(doc)
}

// errDocUnreadable is a doc whose text could be read from nowhere.
var errDocUnreadable = errors.New("the doc's text couldn't be read just now; try again in a moment")

// liveDocHTML is a doc's body as its live copy holds it now, else as it was
// last stored. A seam, so the rules are tested without the services.
var liveDocHTML = func(ctx context.Context, docUUID string) (string, error) {
	cur, err := collab.Read(ctx, docUUID)
	if err == nil {
		return cur.HTML, nil
	}
	if errors.Is(err, collab.ErrNoSuchDoc) || errors.Is(err, collab.ErrDocInTrash) {
		return "", err
	}
	doc, derr := docBusiness.GetSystemDocByUUID(ctx, docUUID)
	if derr != nil || doc == nil {
		return "", fmt.Errorf("%w (%v)", errDocUnreadable, err)
	}
	return doc.Body, nil
}

// ReadLiveDocHTML is liveDocHTML for the controllers.
func ReadLiveDocHTML(ctx context.Context, docUUID string) (string, error) {
	return liveDocHTML(ctx, docUUID)
}
