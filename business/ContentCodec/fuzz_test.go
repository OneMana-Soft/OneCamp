package contentcodec

import (
	"bytes"
	"strings"
	"testing"
)

// Whatever Markdown reads as, writing it out and reading it again gives the
// same doc: export is faithful to everything import can produce.
func FuzzMarkdownStable(f *testing.F) {
	for _, s := range []string{
		"# h\n\ntext *em* **strong** `code` [l](u)\n",
		"- a\n- b\n  - c\n\n1. x\n2. y\n",
		"> q\n> - l\n",
		"a\\\nb",
		"* [ ] t\n* [x] u\n",
		"| a | b |\n|---|---|\n| 1 | 2 |\n",
		"<details>\n<summary>T</summary>\n\nx\n\n</details>\n",
		"text_with_underscores and *stars* and ~~strike~~ and <u>u</u>",
		"```go\ncode\n```\n",
		"![i](https://x/y.png) after",
		"https://x.y/z and www.a.b.",
		// B2: a toggle heading, an empty summary, contents, highlights.
		"<details open>\n<summary><h2>T <em>i</em></h2></summary>\n\nx\n\n</details>\n",
		"<details>\n<summary></summary>\n\n- a\n\n</details>\n",
		"[TOC]\n\n- [TOC]\n\n> [[_TOC_]]\n\n\\[TOC\\]",
		"a <mark>m **b**</mark> c ==not== <mark>",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, md string) {
		j1, err := FromMarkdown(md, everything())
		if err != nil {
			return
		}
		m1 := ToMarkdown(j1, MarkdownOptions{AppURL: "https://app.example.test"})
		j2, err := FromMarkdown(m1, everything())
		if err != nil {
			t.Fatalf("own output does not parse: %v", err)
		}
		// Markdown can't put a mark's edge on whitespace ("** x**" is not
		// bold), so the writer moves it outside: compare with that undone.
		a, _ := CanonicalJSON(Normalize(unmarkEdgeSpace(j1), everything()))
		b, _ := CanonicalJSON(Normalize(unmarkEdgeSpace(j2), everything()))
		if !bytes.Equal(a, b) {
			t.Fatalf("not stable.\ninput: %q\nmd: %q\nfirst: %s\nsecond: %s", md, m1, a, b)
		}
	})
}

// unmarkEdgeSpace moves the whitespace at either end of every marked text
// node into text with no marks.
func unmarkEdgeSpace(n *Node) *Node {
	out := &Node{Type: n.Type, Attrs: n.Attrs, Marks: n.Marks, Text: n.Text}
	for _, c := range n.Content {
		if c.Type != "text" || len(c.Marks) == 0 || hasMark(c.Marks, "code") {
			out.Content = append(out.Content, unmarkEdgeSpace(c))
			continue
		}
		t := c.Text
		core := strings.TrimFunc(t, isSpaceRune)
		lead := t[:strings.Index(t, core)]
		trail := t[len(lead)+len(core):]
		if core == "" {
			out.Content = append(out.Content, &Node{Type: "text", Text: t})
			continue
		}
		if lead != "" {
			out.Content = append(out.Content, &Node{Type: "text", Text: lead})
		}
		out.Content = append(out.Content, &Node{Type: "text", Text: core, Marks: c.Marks})
		if trail != "" {
			out.Content = append(out.Content, &Node{Type: "text", Text: trail})
		}
	}
	return out
}

// Whatever HTML reads as, writing it and reading it again gives the same doc.
func FuzzHTMLStable(f *testing.F) {
	for _, s := range []string{
		"<p>a <b>b</b> <i>c</i></p>",
		"<ul><li>a<ul><li>b</li></ul></li></ul>",
		"<div>loose <span>text</span><p>block</p>tail</div>",
		"<table><tr><td>a</td></tr></table>",
		"<pre><code class=\"language-go\">x\n y</code></pre>",
		"<details><summary>t</summary>c</details>",
		"<p>a<br>b</p><img src=\"https://x/y.png\">",
		// B2: old toggles, summaries, colours by name, contents.
		"<div data-type=\"collapsible\" data-title=\"t\" data-open=\"false\"><p>c</p></div>",
		"<details><summary><h3>t</h3>x</summary><ul><li>c</li></ul></details><details>d</details>",
		"<p data-block-color=\"sky_background\"><mark data-color=\"berry\">m</mark> <span data-color=\"moss\">g</span></p>",
		"<blockquote data-quote-size=\"large\"><p>q</p></blockquote><nav data-type=\"toc\"></nav><ol><li data-block-color=\"dusk\">i</li></ol>",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, html string) {
		j1, err := FromHTML(html, HTMLOptions{Foreign: true, Schema: everything()})
		if err != nil {
			return
		}
		h1 := ToHTML(j1)
		j2, err := FromHTML(h1, HTMLOptions{Schema: everything()})
		if err != nil {
			t.Fatalf("own output does not parse: %v", err)
		}
		a, _ := CanonicalJSON(j1)
		b, _ := CanonicalJSON(j2)
		if !bytes.Equal(a, b) {
			t.Fatalf("not stable.\ninput: %q\nhtml: %q\nfirst: %s\nsecond: %s", html, h1, a, b)
		}
	})
}
