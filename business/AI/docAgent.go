package business

// Doc agent: read and draft OneCamp documents through the AI assistant.
//
// WHY THIS SHAPE
// --------------
// OneCamp docs are collaborative (Yjs/Tiptap/ProseMirror). The live document
// is authoritative while anyone has it open, so blindly writing HTML into an
// open doc would race the collab service and could clobber in-flight edits.
// Two operations are safe and high value:
//
//   - read_doc (read-only): fetch a doc the user can already see and hand its
//     text to the model so it can summarize, answer questions, or draft from
//     it. Permission is enforced here exactly as the app enforces it.
//   - create_doc with a body: a brand-new doc is open in no editor, so the
//     body we persist is what the collaboration service loads (from doc_body)
//     the first time someone opens it. "Turn this thread into a PRD" therefore
//     produces a real, populated doc rather than an empty titled shell.
//
// Editing a live collaborative document out-of-band is unsafe, so there is no
// tool that writes an existing doc's stored body. Adding to one goes through the
// collaboration service instead (append_to_doc, below).
//
// CONFIRMED, having built one and thrown it away. Documents are Yjs CRDTs behind
// Hocuspocus (other-services/collaboration-service). doc_body is not the
// document; it is where Hocuspocus persists a snapshot of it. Writing there is
// safe for a NEW doc precisely because no Y.Doc exists yet, which is why
// create_doc works. For an existing one, if anybody has it open the in-memory
// Y.Doc is authoritative and the next store overwrites whatever was written
// underneath it: the agent's edit vanishes with no error. If nobody has it open,
// the write lands and is then clobbered by the first person to open a stale tab.
//
// Checking "is it open right now" does not fix it either, only narrows the race.
// The real fix is an endpoint on the collaboration service that applies the
// change AS a Yjs update, so it merges like any other edit. That endpoint now
// exists (other-services/collaboration-service/appendToDoc.js, Sep 2026), and
// append_to_doc (docAppend.go) uses it: additions only, as a live edit. Writing
// doc_body for an existing doc is still the data-loss bug described above, and
// replacing or deleting existing content is still not offered.

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	domainUser "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// readDocMaxChars bounds how much doc text we feed back into the model so a
// huge doc can't blow the context budget (and the model's cost ceiling).
const readDocMaxChars = 12000

// executeReadDoc returns a document's text content so the assistant can
// summarize it, answer questions about it, or use it as drafting source.
// Read-only. Access is enforced the same way the app does: public docs are
// readable by any workspace member; private docs require the user to be the
// creator or to hold read/edit/comment access.
func executeReadDoc(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	docUUID := strings.TrimSpace(action.Params["doc_uuid"])
	if docUUID == "" {
		return "", nil, fmt.Errorf("doc_uuid is required")
	}
	if _, err := uuid.Parse(docUUID); err != nil {
		return "", nil, fmt.Errorf("invalid document UUID")
	}

	dgraphUser, err := getDgraphUserForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, err
	}

	doc, err := docBusiness.GetDocByDocUUID(ctx, docUUID, dgraphUser.Uid)
	if err != nil || doc == nil || doc.Uuid == "" {
		return "", nil, fmt.Errorf("document not found or you don't have access")
	}

	// Permission gate. The dgraph query returns the node regardless of access
	// (it carries access counts), so we must decide here.
	isPrivate := doc.IsPrivate != nil && *doc.IsPrivate
	isCreator := doc.CreatedBy != nil && doc.CreatedBy.Uuid == dgraphUser.Uuid
	hasAccess := !isPrivate || isCreator ||
		doc.HasReadAccess > 0 || doc.HasEditAccess > 0 || doc.HasCommentAccess > 0
	if !hasAccess {
		return "", nil, fmt.Errorf("document not found or you don't have access")
	}

	title := strings.TrimSpace(doc.Title)
	if title == "" {
		title = "(untitled)"
	}
	plain := strings.TrimSpace(helpers.RemoveHTMLTags(doc.Body))

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Document: %s\n\n", title))
	if plain == "" {
		b.WriteString("(This document has no body content yet.)")
	} else {
		runes := []rune(plain)
		if len(runes) > readDocMaxChars {
			plain = string(runes[:readDocMaxChars])
			b.WriteString(plain)
			b.WriteString("\n\n[Note: this document is long; only the first part is shown.]")
		} else {
			b.WriteString(plain)
		}
	}

	return b.String(), nil, nil
}

// --- Markdown -> Tiptap-safe HTML ------------------------------------------
//
// The model drafts doc bodies in Markdown. The collaboration service round-
// trips doc_body HTML through the Tiptap schema, so we emit the small, well-
// supported HTML subset that schema understands: headings (h1-h3), paragraphs,
// bullet/ordered lists, blockquotes, fenced code, and inline bold/italic/code/
// links. Everything is HTML-escaped FIRST so nothing the model emits can inject
// markup; we then introduce only our own safe tags.

var (
	docInlineCodeRe = regexp.MustCompile("`([^`]+)`")
	docBoldRe       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	docItalicRe     = regexp.MustCompile(`\*([^*]+)\*`)
	docLinkRe       = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
)

func docEscapeHTML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	)
	return r.Replace(s)
}

// docInlineMD applies inline formatting to a single (already block-classified)
// line. Underscore italics are intentionally unsupported because snake_case
// identifiers are common in technical docs and would be mangled.
func docInlineMD(s string) string {
	s = docEscapeHTML(s)
	s = docInlineCodeRe.ReplaceAllString(s, "<code>$1</code>")
	s = docBoldRe.ReplaceAllString(s, "<strong>$1</strong>")
	s = docItalicRe.ReplaceAllString(s, "<em>$1</em>")
	s = docLinkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := docLinkRe.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		text, url := sub[1], sub[2]
		low := strings.ToLower(url)
		// Only allow safe schemes; otherwise drop the link but keep the text.
		if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") && !strings.HasPrefix(low, "mailto:") {
			return text
		}
		return fmt.Sprintf(`<a href="%s">%s</a>`, url, text)
	})
	return s
}

func docHeadingLevel(line string) (int, string) {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 || i > 6 {
		return 0, ""
	}
	if i >= len(line) || line[i] != ' ' {
		return 0, "" // "#text" is not a heading; needs "# text"
	}
	level := i
	if level > 3 {
		level = 3
	}
	return level, strings.TrimSpace(line[i:])
}

func docIsUL(line string) (string, bool) {
	if len(line) >= 2 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && line[1] == ' ' {
		return strings.TrimSpace(line[2:]), true
	}
	return "", false
}

func docParseOL(line string) (string, bool) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) {
		return "", false
	}
	if line[i] == '.' || line[i] == ')' {
		rest := line[i+1:]
		if strings.HasPrefix(rest, " ") {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// docMarkdownToHTML converts the model's Markdown draft into Tiptap-safe HTML
// for storage in doc_body. Returns "" for empty input.
func docMarkdownToHTML(md string) string {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	md = strings.TrimSpace(md)
	if md == "" {
		return ""
	}

	lines := strings.Split(md, "\n")

	var b strings.Builder
	var para []string
	listType := "" // "ul" | "ol" | ""
	inCode := false
	var codeBuf []string

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		b.WriteString("<p>")
		b.WriteString(strings.Join(para, "<br/>"))
		b.WriteString("</p>")
		para = nil
	}
	closeList := func() {
		if listType != "" {
			b.WriteString("</" + listType + ">")
			listType = ""
		}
	}
	flushCode := func() {
		b.WriteString("<pre><code>")
		b.WriteString(docEscapeHTML(strings.Join(codeBuf, "\n")))
		b.WriteString("</code></pre>")
		codeBuf = nil
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)

		// Fenced code blocks.
		if strings.HasPrefix(trimmed, "```") {
			if inCode {
				flushCode()
				inCode = false
			} else {
				flushPara()
				closeList()
				inCode = true
			}
			continue
		}
		if inCode {
			codeBuf = append(codeBuf, raw)
			continue
		}

		if trimmed == "" {
			flushPara()
			closeList()
			continue
		}

		if level, content := docHeadingLevel(trimmed); level > 0 {
			flushPara()
			closeList()
			tag := fmt.Sprintf("h%d", level)
			b.WriteString("<" + tag + ">")
			b.WriteString(docInlineMD(content))
			b.WriteString("</" + tag + ">")
			continue
		}

		if strings.HasPrefix(trimmed, ">") {
			flushPara()
			closeList()
			content := strings.TrimPrefix(trimmed, ">")
			content = strings.TrimPrefix(content, " ")
			b.WriteString("<blockquote><p>")
			b.WriteString(docInlineMD(content))
			b.WriteString("</p></blockquote>")
			continue
		}

		if content, ok := docIsUL(trimmed); ok {
			flushPara()
			if listType != "ul" {
				closeList()
				b.WriteString("<ul>")
				listType = "ul"
			}
			b.WriteString("<li>")
			b.WriteString(docInlineMD(content))
			b.WriteString("</li>")
			continue
		}

		if content, ok := docParseOL(trimmed); ok {
			flushPara()
			if listType != "ol" {
				closeList()
				b.WriteString("<ol>")
				listType = "ol"
			}
			b.WriteString("<li>")
			b.WriteString(docInlineMD(content))
			b.WriteString("</li>")
			continue
		}

		// Plain paragraph text.
		closeList()
		para = append(para, docInlineMD(trimmed))
	}

	flushPara()
	closeList()
	if inCode && len(codeBuf) > 0 {
		flushCode()
	}

	return b.String()
}

// executeFindPeople answers "who is X", "how do I reach X", "who works on X".
//
// WHY IT EXISTS. The agent could list teams and assign a task to a name, and had
// no way to answer a question about a person. Every routing question ("who should
// I ask about billing", "what is Priya's address") fell back to the model's
// imagination, which for a name it has never seen produces a confident and wrong
// answer.
//
// Read-only, bounded, and bots are excluded in the query: every agent has a real
// users row so it can author as itself, and without that filter a question about
// who works on something answers with the other agents.
//
// It searches job title and department as well as names. Both are unset on every
// user in the workspaces I can see, so those clauses match nothing today. They
// cost one filter each and start working the day somebody fills the fields in,
// which is the difference between a feature that grows into its data and one that
// has to be rewritten when the data arrives.
func executeFindPeople(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	query := strings.TrimSpace(action.Params["query"])
	if query == "" {
		return "", nil, fmt.Errorf("query is required (a name, job title or team to look for)")
	}

	// Proves the caller is a real member before reading the directory, and gives
	// the same error shape as every other executor when they are not.
	if _, err := getUserInfoForExecutor(ctx, userUUID); err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}

	people, err := domainUser.SearchPeople(ctx, query, 0)
	if err != nil {
		return "", nil, fmt.Errorf("could not search people: %w", err)
	}
	if len(people) == 0 {
		return fmt.Sprintf("No workspace member matches %q.", query), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "People matching %q:\n", query)
	for _, u := range people {
		if u == nil {
			continue
		}
		name := strings.TrimSpace(u.UserName)
		if name == "" {
			name = strings.TrimSpace(u.UserFullName)
		}
		if name == "" {
			continue
		}
		b.WriteString("- " + name)
		if full := strings.TrimSpace(u.UserFullName); full != "" && !strings.EqualFold(full, name) {
			b.WriteString(" (" + full + ")")
		}
		// Only what is set. Printing "title: " with nothing after it invites the
		// model to fill the gap.
		if t := strings.TrimSpace(u.Title); t != "" {
			b.WriteString(", " + t)
		}
		if d := strings.TrimSpace(u.Department); d != "" {
			b.WriteString(", " + d)
		}
		if e := strings.TrimSpace(u.EmailID); e != "" {
			b.WriteString(" — " + e)
		}
		if id := strings.TrimSpace(u.Uuid); id != "" {
			b.WriteString(" [" + id + "]")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil, nil
}
