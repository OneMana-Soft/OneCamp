package trello

import (
	"testing"
	"time"
)

func TestParseTrelloIDTime(t *testing.T) {
	// First 8 hex chars of a Trello id are unix seconds. This id
	// encodes 2024-01-15T08:00:00Z (1705305600 → 0x65A4D680).
	id := "65a4d680abcdef0123456789"
	got := parseTrelloIDTime(id)
	want := time.Unix(0x65a4d680, 0)
	if !got.Equal(want) {
		t.Fatalf("parseTrelloIDTime(%q): got %v, want %v", id, got, want)
	}
}

func TestParseTrelloIDTime_TooShort(t *testing.T) {
	if got := parseTrelloIDTime("abc"); !got.IsZero() {
		t.Fatalf("expected zero time for short id, got %v", got)
	}
}

func TestParseTrelloIDTime_NotHex(t *testing.T) {
	// "ZZZZZZZZ" is non-hex; we should get zero time, not panic.
	if got := parseTrelloIDTime("ZZZZZZZZdeadbeef"); !got.IsZero() {
		t.Fatalf("expected zero time for non-hex id, got %v", got)
	}
}

func TestSanitizeChannelName_NoOp(t *testing.T) {
	// Sanity check the htmlEscape used in trelloDescToHTML doesn't
	// mangle URLs and keeps newlines as <br>. Belt-and-braces because
	// the renderer is the only place we transform user-controlled text.
	in := "see <https://x.com> &\nworks"
	got := trelloDescToHTML(in)
	want := "see &lt;https://x.com&gt; &amp;<br>works"
	if got != want {
		t.Fatalf("trelloDescToHTML: got %q, want %q", got, want)
	}
}

func TestCountCheckItems(t *testing.T) {
	cls := []trelloChecklist{
		{CheckItems: []trelloCheckItem{{}, {}, {}}},
		{CheckItems: []trelloCheckItem{{}}},
	}
	if got := countCheckItems(cls); got != 4 {
		t.Fatalf("countCheckItems: got %d, want 4", got)
	}
	if got := countCheckItems(nil); got != 0 {
		t.Fatalf("countCheckItems(nil): got %d, want 0", got)
	}
}

func TestDerefTime(t *testing.T) {
	if got := derefTime(nil); !got.IsZero() {
		t.Fatalf("derefTime(nil): got %v, want zero", got)
	}
	now := time.Now()
	if got := derefTime(&now); !got.Equal(now) {
		t.Fatalf("derefTime(&now): got %v, want %v", got, now)
	}
}
