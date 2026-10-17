package botpost

import (
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// What a model actually writes, and what a reader must see.
func TestRenderMarkdownFormatsWhatModelsWrite(t *testing.T) {
	got := renderMarkdown("The **load test** is left.\n\n1. Post the numbers\n2. Mark it done\n\nRun `make verify` first.", false)
	for _, want := range []string{
		"<strong>load test</strong>",
		"<ol><li>Post the numbers</li><li>Mark it done</li></ol>",
		"<code>make verify</code>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "**") || strings.Contains(got, "`") {
		t.Errorf("markdown syntax leaked through: %q", got)
	}
}

func TestRenderMarkdownKeepsChatShape(t *testing.T) {
	// A heading is emphasis in chat, not a page title.
	if got := renderMarkdown("# Status\nAll green", false); !strings.Contains(got, "<p><strong>Status</strong></p>") || strings.Contains(got, "<h1") {
		t.Errorf("heading = %q", got)
	}
	// A single newline is a line break, as people type in chat.
	if got := renderMarkdown("line one\nline two", false); got != "<p>line one<br/>line two</p>" {
		t.Errorf("hard wrap = %q", got)
	}
	// While streaming, the cursor trails the last words, even inside a list.
	if got := renderMarkdown("- a\n- b", true); !strings.HasSuffix(got, "b"+streamCursor+"</li></ul>") {
		t.Errorf("cursor = %q", got)
	}
}

func TestRenderMarkdownIsSafe(t *testing.T) {
	cases := map[string]string{
		"use the <div> tag":                       "use the &lt;div&gt; tag",
		"<script>alert(1)</script>":               "&lt;script&gt;",
		"[click](javascript:alert(1))":            "click",
		"![pixel](https://tracker.example/p.gif)": `href="https://tracker.example/p.gif"`,
	}
	for in, want := range cases {
		got := renderMarkdown(in, false)
		if !strings.Contains(got, want) {
			t.Errorf("renderMarkdown(%q) = %q, want it to contain %q", in, got, want)
		}
		if strings.Contains(got, "<script") || strings.Contains(got, "javascript:") || strings.Contains(got, "<img") {
			t.Errorf("renderMarkdown(%q) let something executable or remote through: %q", in, got)
		}
	}
}

// A reply nested deeper than the parser should be asked to read
// (helpers.MarkdownTooDeep) is shown as its words, escaped, and never parsed:
// 50,000 list items nested on one line (100 KB, a model's reply steered by
// what it read) took goldmark 12 s and 7.5 GB of heap. At the limit it is
// still Markdown.
func TestARepliesMarkdownNestedPastTheLimitIsShownAsItsWords(t *testing.T) {
	for _, deep := range []string{
		strings.Repeat("- ", helpers.MaxMarkdownNesting+1) + "deepest <b>item</b>",
		strings.Repeat(">", helpers.MaxMarkdownNesting+1) + " deepest <b>item</b>",
	} {
		got := renderMarkdown(deep, true)
		if strings.Contains(got, "<ul>") || strings.Contains(got, "<blockquote>") || strings.Contains(got, "<b>") {
			t.Fatalf("%.20q… was parsed: %.200s", deep, got)
		}
		if !strings.Contains(got, "deepest &lt;b&gt;item&lt;/b&gt;") || !strings.Contains(got, streamCursor) {
			t.Fatalf("%.20q… lost its words or its cursor: %.200s", deep, got)
		}
	}
	at := strings.Repeat("- ", helpers.MaxMarkdownNesting) + "deepest"
	if got := renderMarkdown(at, false); strings.Count(got, "<ul>") != helpers.MaxMarkdownNesting {
		t.Fatalf("%d levels should still be lists: %d", helpers.MaxMarkdownNesting, strings.Count(got, "<ul>"))
	}
}
