package botpost

import (
	"strings"
	"testing"
)

func TestStreamHTML(t *testing.T) {
	// Typing: trailing cursor on the last paragraph; HTML-escaped.
	got := streamHTML("Hello <b>world", true)
	if !strings.Contains(got, "Hello &lt;b&gt;world") {
		t.Fatalf("expected HTML-escaped text, got %q", got)
	}
	if !strings.HasSuffix(got, streamCursor+"</p>") {
		t.Fatalf("expected trailing cursor while typing, got %q", got)
	}

	// Final: no cursor, paragraphs split on blank lines, newline -> <br/>.
	final := streamHTML("para one\nline two\n\npara two", false)
	if strings.Contains(final, streamCursor) {
		t.Fatalf("final text must not carry the typing cursor, got %q", final)
	}
	if strings.Count(final, "<p>") != 2 {
		t.Fatalf("expected two paragraphs, got %q", final)
	}
	if !strings.Contains(final, "para one<br/>line two") {
		t.Fatalf("expected single newline rendered as <br/>, got %q", final)
	}

	// Empty input yields empty output (caller substitutes a placeholder).
	if streamHTML("   ", true) != "" {
		t.Fatalf("blank input should render empty")
	}
}
