package contentcodec

import (
	"strings"
	"testing"
)

// The agent's doc tools read a doc's top-level blocks: their ids, what each
// is, a heading's level, their words, and each as written (a revert puts
// exactly that back).
func TestBlocksReadsTheTopLevelBlocks(t *testing.T) {
	src := `<h2 data-block-id="h">Risks</h2><p data-block-id="a">Vendor <strong>delay</strong></p>` +
		`<ul data-block-id="l"><li data-block-id="i"><p data-block-id="ip">One</p></li><li><p>Two</p></li></ul>` +
		` loose words ` + `<pre data-block-id="c"><code>go   run</code></pre><script>alert(1)</script>`
	blocks, err := Blocks(src, BlockOptions{})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id, typ string
		level   int
		text    string
	}
	want := []row{
		{"h", "heading", 2, "Risks"},
		{"a", "paragraph", 0, "Vendor delay"},
		{"l", "bulletList", 0, "One Two"},
		{"", "paragraph", 0, "loose words"},
		{"c", "codeBlock", 0, "go run"},
		{"", "", 0, ""}, // read as nothing, still listed with its HTML
	}
	if len(blocks) != len(want) {
		t.Fatalf("got %d blocks: %+v", len(blocks), blocks)
	}
	for i, w := range want {
		b := blocks[i]
		if b.ID != w.id || b.Type != w.typ || b.Level != w.level || b.Text != w.text {
			t.Errorf("block %d: got %+v, want %+v", i, b, w)
		}
	}
	if !strings.Contains(blocks[2].HTML, `data-block-id="ip"`) || !strings.HasPrefix(blocks[2].HTML, `<ul data-block-id="l">`) {
		t.Errorf("the list as written: %s", blocks[2].HTML)
	}
	if blocks[3].HTML != "<p>loose words</p>" || blocks[5].HTML != "<script>alert(1)</script>" {
		t.Errorf("loose text and an unread element: %q %q", blocks[3].HTML, blocks[5].HTML)
	}
}

// "Find action items" reads the lines of a doc wherever they are, each with
// its own id or its container's, and leaves out a ticked to-do: the editor's,
// and one written as GitHub writes them.
func TestBlocksReadsTheTextBlocksWhereverTheyAre(t *testing.T) {
	src := `<h1 data-block-id="t">Launch sync</h1><p data-block-id="empty"> </p><p><br></p>` +
		`<ul data-block-id="l"><li data-block-id="li1"><p data-block-id="b1">Priya to send the deck</p></li><li data-block-id="li2"><p>Jonas will book the venue</p></li></ul>` +
		`<ul data-type="taskList" data-block-id="tl"><li data-type="taskItem" data-checked="true" data-block-id="done"><p>Draft the agenda</p></li>` +
		`<li data-type="taskItem" data-checked="false" data-block-id="open"><p>Order the badges</p></li></ul>` +
		`<ul><li><input type="checkbox" checked> <p>Book the room</p></li><li><input type="checkbox"> <p>Print the menus</p></li></ul>` +
		`<blockquote data-block-id="q"><p>Ask <span data-type="mention" data-id="u1" data-label="Mia">@Mia</span> about it<script>x()</script></p></blockquote>` +
		`<table><tbody><tr><td><p data-block-id="cell">Budget</p></td></tr></tbody></table>` +
		`stray text`
	blocks, err := Blocks(src, BlockOptions{Text: true, SkipDone: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range blocks {
		got = append(got, b.ID+"|"+b.Type+"|"+b.Text)
	}
	want := []string{
		"t|heading|Launch sync",
		"b1|paragraph|Priya to send the deck",
		"li2|paragraph|Jonas will book the venue",
		"open|paragraph|Order the badges",
		"|paragraph|Print the menus",
		"q|paragraph|Ask @Mia about it",
		"cell|paragraph|Budget",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Without SkipDone, a ticked to-do is a line like any other.
	all, _ := Blocks(src, BlockOptions{Text: true})
	if len(all) != len(blocks)+2 || all[3].ID != "done" || all[5].Text != "Book the room" {
		t.Errorf("without SkipDone: %+v", all)
	}
}

// A list item that only looks ticked is one: "data-checked" on an item outside
// a to-do list is not a to-do, and its line stays.
func TestBlocksSkipsOnlyToDos(t *testing.T) {
	blocks, err := Blocks(`<ul><li data-checked="true"><p>Not a to-do</p></li></ul>`, BlockOptions{Text: true, SkipDone: true})
	if err != nil || len(blocks) != 1 || blocks[0].Text != "Not a to-do" {
		t.Errorf("got %+v %v", blocks, err)
	}
}
