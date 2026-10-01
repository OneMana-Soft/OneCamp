package notion

import "testing"

func TestRenderRichText_PlainText(t *testing.T) {
	rts := []notionRichText{
		{Type: "text", PlainText: "hello world"},
	}
	if got := renderRichText(rts); got != "hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderRichText_Bold(t *testing.T) {
	rts := []notionRichText{
		{Type: "text", PlainText: "bold"},
	}
	rts[0].Annotations.Bold = true
	if got := renderRichText(rts); got != "<strong>bold</strong>" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderRichText_NestedAnnotations(t *testing.T) {
	rts := []notionRichText{{Type: "text", PlainText: "x"}}
	rts[0].Annotations.Bold = true
	rts[0].Annotations.Italic = true
	rts[0].Annotations.Code = true
	// Order: code innermost, then bold, then italic. <em><strong><code>x
	got := renderRichText(rts)
	want := "<em><strong><code>x</code></strong></em>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderRichText_Link(t *testing.T) {
	rts := []notionRichText{{Type: "text", PlainText: "click"}}
	rts[0].Text = &struct {
		Content string `json:"content"`
		Link    *struct {
			URL string `json:"url"`
		} `json:"link"`
	}{
		Content: "click",
	}
	rts[0].Text.Link = &struct {
		URL string `json:"url"`
	}{URL: "https://example.com"}
	got := renderRichText(rts)
	want := `<a href="https://example.com" target="_blank" rel="noopener noreferrer">click</a>`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderRichText_HTMLEscapesUnsafeChars(t *testing.T) {
	rts := []notionRichText{
		{Type: "text", PlainText: "<script>alert(1)</script>"},
	}
	got := renderRichText(rts)
	// Body must be HTML-escaped; the escaper does NOT also touch quotes.
	want := "&lt;script&gt;alert(1)&lt;/script&gt;"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderBlocks_Paragraph(t *testing.T) {
	blocks := []notionBlock{
		{
			Type: "paragraph",
			Paragraph: &notionRichTextBox{
				RichText: []notionRichText{{Type: "text", PlainText: "hi"}},
			},
		},
	}
	if got := renderBlocks(blocks); got != "<p>hi</p>" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderBlocks_BulletedListGrouping(t *testing.T) {
	blocks := []notionBlock{
		{Type: "bulleted_list_item", BulletedListItem: &notionRichTextBox{
			RichText: []notionRichText{{Type: "text", PlainText: "a"}},
		}},
		{Type: "bulleted_list_item", BulletedListItem: &notionRichTextBox{
			RichText: []notionRichText{{Type: "text", PlainText: "b"}},
		}},
		// Break the list with a paragraph.
		{Type: "paragraph", Paragraph: &notionRichTextBox{
			RichText: []notionRichText{{Type: "text", PlainText: "para"}},
		}},
		// New list.
		{Type: "bulleted_list_item", BulletedListItem: &notionRichTextBox{
			RichText: []notionRichText{{Type: "text", PlainText: "c"}},
		}},
	}
	got := renderBlocks(blocks)
	want := "<ul><li>a</li><li>b</li></ul><p>para</p><ul><li>c</li></ul>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderBlocks_ToDoChecked(t *testing.T) {
	blocks := []notionBlock{
		{Type: "to_do", ToDo: &notionToDoBox{
			RichText: []notionRichText{{Type: "text", PlainText: "task"}},
			Checked:  true,
		}},
	}
	got := renderBlocks(blocks)
	want := `<p><input type="checkbox" disabled checked /> task</p>`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderBlocks_UnsupportedSurfacedAsChip(t *testing.T) {
	blocks := []notionBlock{{Type: "synced_block"}}
	got := renderBlocks(blocks)
	if got == "" {
		t.Fatalf("expected unsupported chip, got empty")
	}
	// Should mention the type so the operator can spot what was dropped.
	if !contains(got, "synced_block") {
		t.Fatalf("expected type name in chip, got %q", got)
	}
}

// contains is a tiny helper because the standard strings.Contains
// import would otherwise be unused in this test file's other helpers.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestEscapeHTMLAttr_QuotesEscaped(t *testing.T) {
	got := escapeHTMLAttr(`a"b'c<d>e&f`)
	want := "a&quot;b&#39;c&lt;d&gt;e&amp;f"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLooksLikeTaskDatabase(t *testing.T) {
	// Title + Status property → yes.
	props := map[string]notionDBProp{
		"Name":   {Type: "title"},
		"Status": {Type: "status"},
	}
	if !looksLikeTaskDatabase(props) {
		t.Fatal("Title+Status should look task-shaped")
	}
	// Title + Select named "Status" → yes.
	props2 := map[string]notionDBProp{
		"Name":   {Type: "title"},
		"Status": {Type: "select"},
	}
	if !looksLikeTaskDatabase(props2) {
		t.Fatal("Title+Select(Status) should look task-shaped")
	}
	// Title alone → no.
	props3 := map[string]notionDBProp{
		"Name": {Type: "title"},
	}
	if looksLikeTaskDatabase(props3) {
		t.Fatal("Title-only should not be task-shaped")
	}
	// Status alone → no.
	props4 := map[string]notionDBProp{
		"Status": {Type: "status"},
	}
	if looksLikeTaskDatabase(props4) {
		t.Fatal("Status-only should not be task-shaped")
	}
}
