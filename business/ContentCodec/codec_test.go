package contentcodec

import (
	"strings"
	"testing"
)

func mustMD(t *testing.T, md string) *Node {
	t.Helper()
	doc, err := FromMarkdown(md, nil)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func mustHTML(t *testing.T, src string, foreign bool) *Node {
	t.Helper()
	doc, err := FromHTML(src, HTMLOptions{Foreign: foreign})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestUnsafeLinksAreLeftOut(t *testing.T) {
	for _, href := range []string{"javascript:alert(1)", "java&#x09;script:alert(1)", " JAVASCRIPT:x", "vbscript:x", "data:text/html,x", "file:///etc/passwd"} {
		if safeHref(href) {
			t.Errorf("%q passed", href)
		}
	}
	for _, href := range []string{"https://x.example", "http://x", "mailto:a@b.c", "tel:+1", "/app/doc/1", "#notes", "page.md", "?q=1"} {
		if !safeHref(href) {
			t.Errorf("%q refused", href)
		}
	}
	doc := mustHTML(t, `<p><a href="javascript:alert(1)">click</a> <a href="https://ok.example">ok</a></p>`, true)
	md := ToMarkdown(doc, MarkdownOptions{})
	if md != "click [ok](https://ok.example)\n" {
		t.Fatalf("got %q", md)
	}
}

func TestForeignHTMLKeepsNoClassesScriptsOrStyleColours(t *testing.T) {
	doc := mustHTML(t, `<p class="fixed inset-0"><span style="color:#000000;font-weight:700">bold</span><script>alert(1)</script><iframe src="x"></iframe></p>`, true)
	html := ToHTML(doc)
	if html != `<p><strong>bold</strong></p>` {
		t.Fatalf("got %s", html)
	}
	// The editor's colours are names (B2, N15): one by name, or an old doc's
	// var(--camp-sky-ink), stays; any other goes, and its text stays.
	own := mustHTML(t, `<p><span data-color="berry">red</span>, <span style="color: var(--camp-sky-ink)">blue</span> and <span style="color: #ff0000">hex</span></p>`, false)
	if got := ToHTML(own); got != `<p><span data-color="berry">red</span>, <span data-color="sky">blue</span> and hex</p>` {
		t.Fatalf("the editor's own colours: %s", got)
	}
	// Foreign HTML keeps a colour only by one of our names, never by its style.
	pasted := mustHTML(t, `<p><span data-color="moss">green</span> <span style="color: var(--camp-sky-ink)">blue</span> <mark data-color="nope">marked</mark></p>`, true)
	if got := ToHTML(pasted); got != `<p><span data-color="moss">green</span> blue <mark data-color="sun">marked</mark></p>` {
		t.Fatalf("got %s", got)
	}
}

func TestHTMLWhitespaceIsCollapsedOutsidePre(t *testing.T) {
	doc := mustHTML(t, "<p>  a \n\t b  </p><pre>  keep\n   this </pre>", true)
	if got := ToHTML(doc); got != "<p>a b</p><pre><code>  keep\n   this </code></pre>" {
		t.Fatalf("got %q", got)
	}
}

func TestImagesNeverKeepABlobAddress(t *testing.T) {
	doc := mustHTML(t, `<p>before<img src="blob:https://app/1" alt="pasted">after</p><img src="javascript:x">`, true)
	html := ToHTML(doc)
	if strings.Contains(html, "blob:") || strings.Contains(html, "javascript") || strings.Contains(html, "<img") {
		t.Fatalf("got %s", html)
	}
	if !strings.Contains(html, "pasted") {
		t.Fatalf("the alt text should stay: %s", html)
	}
}

// shippedWithout is the shipped schema less some node types: an editor
// without them.
func shippedWithout(types ...string) *Schema {
	s := &Schema{nodes: map[string]bool{}, marks: map[string]bool{}}
	for k := range shipped().nodes {
		s.nodes[k] = true
	}
	for k := range shipped().marks {
		s.marks[k] = true
	}
	for _, typ := range types {
		delete(s.nodes, typ)
	}
	return s
}

func TestATableWithNoTableBlockIsKeptAsItsMarkdown(t *testing.T) {
	noTables := shippedWithout("table", "tableRow", "tableHeader", "tableCell")
	doc, err := FromMarkdown("| a | b |\n| --- | --- |\n| 1 | 2 |\n", noTables)
	if err != nil || len(doc.Content) != 1 || doc.Content[0].Type != "codeBlock" || doc.Content[0].attrString("language") != "markdown" {
		t.Fatalf("got %+v, %v", doc.Content[0], err)
	}
	if got := codeText(doc.Content[0]); got != "| a | b |\n| --- | --- |\n| 1 | 2 |" {
		t.Fatalf("got %q", got)
	}
	// With the simple table (the shipped editor has it, B3), it is a table.
	full := mustMD(t, "| a | b |\n| --- | --- |\n| 1 | 2 |\n")
	if full.Content[0].Type != "table" {
		t.Fatalf("got %+v", full.Content[0])
	}
}

// Equations (B3) are GitHub's: $`x`$ in a line, a math block on its own; an
// editor without them keeps a block one as a math code block, an inline one
// as its TeX between dollars.
func TestEquationsReadAsGitHubWritesThem(t *testing.T) {
	doc := mustMD(t, "Area $`\\pi r^2`$ and `code`, $5.\n\n```math\nx^2\n```\n")
	p := doc.Content[0]
	if p.Content[1].Type != "inlineMath" || p.Content[1].attrString("latex") != "\\pi r^2" {
		t.Fatalf("got %+v", p.Content[1])
	}
	if b := doc.Content[1]; b.Type != "blockMath" || b.attrString("latex") != "x^2" {
		t.Fatalf("got %+v", b)
	}
	// An escaped dollar is text, and so is a code span with one open side.
	for _, md := range []string{"\\$`x`$", "$`x` $", "$`x`"} {
		if d := mustMD(t, md+"\n"); d.Content[0].Content[0].Type == "inlineMath" {
			t.Fatalf("%q read as an equation", md)
		}
	}
	plain := shippedWithout("inlineMath", "blockMath")
	d, err := FromMarkdown("$`x`$\n\n```math\ny\n```\n", plain)
	if err != nil || PlainText(d.Content[0]) != "$x$" || d.Content[1].Type != "codeBlock" || d.Content[1].attrString("language") != "math" {
		t.Fatalf("got %+v, %v", d.Content, err)
	}
}

func TestAChartBlockIsAChart(t *testing.T) {
	doc := mustMD(t, "```chart\n{\"type\":\"bar\",\"data\":[1]}\n```\n")
	if doc.Content[0].Type != "chartEmbed" || doc.Content[0].attrString("spec") != `{"type":"bar","data":[1]}` {
		t.Fatalf("got %+v", doc.Content[0])
	}
	// Not JSON: a code block, as written.
	doc = mustMD(t, "```chart\nnot json\n```\n")
	if doc.Content[0].Type != "codeBlock" {
		t.Fatalf("got %+v", doc.Content[0])
	}
}

func TestAlertsAreCallouts(t *testing.T) {
	doc := mustMD(t, "> [!WARNING]\n> Careful.\n\n> [!UNKNOWN]\n> Just a quote.\n")
	if doc.Content[0].Type != "callout" || doc.Content[0].attrString("color") != "yellow" {
		t.Fatalf("got %+v", doc.Content[0])
	}
	if doc.Content[1].Type != "blockquote" {
		t.Fatalf("an unknown alert is a quote: %+v", doc.Content[1])
	}
}

func TestEmphasisMarkdownCannotWriteIsWrittenAsHTML(t *testing.T) {
	// **!** right after a letter is not bold in Markdown.
	doc := &Node{Type: "doc", Content: []*Node{{Type: "paragraph", Content: []*Node{
		{Type: "text", Text: "Note"},
		{Type: "text", Text: "!", Marks: []*Mark{{Type: "bold"}}},
		{Type: "text", Text: "ok"},
	}}}}
	md := ToMarkdown(doc, MarkdownOptions{})
	if md != "Note<strong>!</strong>ok\n" {
		t.Fatalf("got %q", md)
	}
	back := mustMD(t, md)
	if ToHTML(back) != "<p>Note<strong>!</strong>ok</p>" {
		t.Fatalf("read back as %s", ToHTML(back))
	}
}

func TestListsInARowStaySeparate(t *testing.T) {
	item := func(s string) *Node {
		return &Node{Type: "listItem", Content: []*Node{{Type: "paragraph", Content: []*Node{{Type: "text", Text: s}}}}}
	}
	list := func(s string) *Node { return &Node{Type: "bulletList", Content: []*Node{item(s)}} }
	doc := &Node{Type: "doc", Content: []*Node{list("a"), list("b"), list("c")}}
	md := ToMarkdown(doc, MarkdownOptions{})
	if md != "- a\n\n* b\n\n- c\n" {
		t.Fatalf("got %q", md)
	}
	if back := mustMD(t, md); len(back.Content) != 3 {
		t.Fatalf("read back as %d lists", len(back.Content))
	}
}

func TestPlainTextReadsAsParagraphs(t *testing.T) {
	doc := FromText("# not a heading\nsecond line\n\n\nnext paragraph, see https://onemana.dev.\n")
	want := `<p># not a heading<br>second line</p><p>next paragraph, see <a target="_blank" rel="noopener noreferrer nofollow" href="https://onemana.dev">https://onemana.dev</a>.</p>`
	if got := ToHTML(doc); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestInlineHTMLLinksAndTagsInMarkdown(t *testing.T) {
	doc := mustMD(t, `a <a href="https://x.example/?a=1&amp;b=2">link</a> and <u>under</u> and <b>bold</b><br>next`)
	want := `<p>a <a target="_blank" rel="noopener noreferrer nofollow" href="https://x.example/?a=1&amp;b=2">link</a> and <u>under</u> and <strong>bold</strong><br>next</p>`
	if got := ToHTML(doc); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestEveryReaderReturnsAValidDoc(t *testing.T) {
	for _, doc := range []*Node{mustMD(t, ""), mustHTML(t, "", true), FromText("")} {
		if got := ToHTML(doc); got != "<p></p>" {
			t.Fatalf("an empty input should read as one empty paragraph, got %s", got)
		}
	}
	// A list item that starts with a list gets its paragraph first.
	doc := mustHTML(t, "<ul><li><ul><li>x</li></ul></li></ul>", true)
	if got := ToHTML(doc); got != "<ul><li><p></p><ul><li><p>x</p></li></ul></li></ul>" {
		t.Fatalf("got %s", got)
	}
}
