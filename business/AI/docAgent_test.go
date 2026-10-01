package business

import (
	"strings"
	"testing"
)

func TestDocMarkdownToHTML_Blocks(t *testing.T) {
	md := "# Title\n\nSome **bold** and *italic* and `code`.\n\n- one\n- two\n\n1. first\n2. second\n\n> a quote"
	got := docMarkdownToHTML(md)

	wants := []string{
		"<h1>Title</h1>",
		"<strong>bold</strong>",
		"<em>italic</em>",
		"<code>code</code>",
		"<ul><li>one</li><li>two</li></ul>",
		"<ol><li>first</li><li>second</li></ol>",
		"<blockquote><p>a quote</p></blockquote>",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Fatalf("expected output to contain %q, got %q", w, got)
		}
	}
}

func TestDocMarkdownToHTML_EscapesInjection(t *testing.T) {
	md := `here is <script>alert("x")</script> and <img src=x onerror=y>`
	got := docMarkdownToHTML(md)
	if strings.Contains(got, "<script>") || strings.Contains(got, "<img") {
		t.Fatalf("raw HTML must be escaped, got %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Fatalf("expected escaped script tag, got %q", got)
	}
}

func TestDocMarkdownToHTML_LinkSchemeFiltering(t *testing.T) {
	safe := docMarkdownToHTML("see [docs](https://example.com/x)")
	if !strings.Contains(safe, `<a href="https://example.com/x">docs</a>`) {
		t.Fatalf("expected safe https link, got %q", safe)
	}
	unsafe := docMarkdownToHTML("click [here](javascript:alert(1))")
	if strings.Contains(unsafe, "href") || strings.Contains(unsafe, "javascript:") {
		t.Fatalf("javascript: scheme must be dropped, got %q", unsafe)
	}
	if !strings.Contains(unsafe, "here") {
		t.Fatalf("link text should be preserved when scheme is dropped, got %q", unsafe)
	}
}

func TestDocMarkdownToHTML_FencedCode(t *testing.T) {
	md := "```\nfoo := **notbold**\n```"
	got := docMarkdownToHTML(md)
	if !strings.Contains(got, "<pre><code>") {
		t.Fatalf("expected a code block, got %q", got)
	}
	// Inside a code fence, markdown must not be interpreted.
	if strings.Contains(got, "<strong>") {
		t.Fatalf("markdown inside code fence must not be parsed, got %q", got)
	}
}

func TestDocMarkdownToHTML_SnakeCaseNotItalic(t *testing.T) {
	got := docMarkdownToHTML("the user_id_field is set")
	if strings.Contains(got, "<em>") {
		t.Fatalf("snake_case must not become italic, got %q", got)
	}
}

func TestDocMarkdownToHTML_Empty(t *testing.T) {
	if got := docMarkdownToHTML("   \n  "); got != "" {
		t.Fatalf("expected empty string for blank input, got %q", got)
	}
}
