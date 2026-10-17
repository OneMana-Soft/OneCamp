package contentcodec

import (
	"strings"

	"golang.org/x/net/html"
)

// A doc's HTML, block by block: what the agent's doc tools write beside and
// replace, and the lines "Find action items" numbers (docs programme B9).
// Each block's type and text are read by the reader FromHTML uses, so a
// block, its kind and its words are the same here as in an export, a search
// preview or the editor's own import. Its id and its HTML are read as
// written: the codec's doc carries no ids (coverage_test.go's identityAttrs).

// Block is one block of a doc's HTML.
type Block struct {
	// ID is the block's id (data-block-id), "" when it has none. A text block
	// (BlockOptions.Text) without one of its own has its nearest container's.
	ID string
	// Type is the node the codec reads it as ("heading", "paragraph",
	// "bulletList"…), "" for an element it reads as nothing.
	Type string
	// Level is a heading's level, 1 to 6; 0 for any other block.
	Level int
	// Text is its text as search reads it (PlainText), on one line.
	Text string
	// HTML is the block as written, with the ids of the blocks inside it.
	HTML string
}

// BlockOptions says which of a doc's blocks Blocks gives.
type BlockOptions struct {
	// Text gives the blocks that hold text (paragraphs, headings and code
	// with words in them), wherever they are, in place of the top-level
	// blocks: a list item's lines, a quote's, a table cell's.
	Text bool
	// SkipDone leaves out a ticked to-do and everything in it.
	SkipDone bool
}

// textBlockTags are the elements the codec reads as a block of text: a
// paragraph, a heading or a code block (addNode).
var textBlockTags = map[string]bool{"p": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "pre": true}

// Blocks reads a doc's HTML as its blocks, in order: the top-level ones, or
// with opts.Text the ones that hold text. Loose text at the top is a
// paragraph of its own; an error is HTML that couldn't be parsed.
func Blocks(src string, opts BlockOptions) ([]Block, error) {
	nodes, err := parseBody(src)
	if err != nil {
		return nil, err
	}
	var out []Block
	if !opts.Text {
		for _, n := range nodes {
			switch n.Type {
			case html.ElementNode:
				out = append(out, blockOf(n, attrVal(n, "data-block-id")))
			case html.TextNode:
				if t := oneLine(n.Data); t != "" {
					out = append(out, Block{Type: "paragraph", Text: t, HTML: "<p>" + html.EscapeString(t) + "</p>"})
				}
			}
		}
		return out, nil
	}
	var walk func(n *html.Node, id string, inTasks bool)
	walk = func(n *html.Node, id string, inTasks bool) {
		if n.Type != html.ElementNode || n.Namespace != "" {
			return
		}
		tag := strings.ToLower(n.Data)
		if ignoredTags[tag] {
			return
		}
		if own := attrVal(n, "data-block-id"); own != "" {
			id = own
		}
		if opts.SkipDone && tag == "li" && inTasks && itemTicked(n) {
			return
		}
		if textBlockTags[tag] {
			if b := blockOf(n, id); b.Text != "" {
				out = append(out, b)
			}
			return
		}
		tasks := readsAsTaskList(n)
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, id, tasks)
		}
	}
	for _, n := range nodes {
		walk(n, "", false)
	}
	return out, nil
}

// blockOf is one element as a block, under the id given.
func blockOf(n *html.Node, id string) Block {
	var sb strings.Builder
	_ = html.Render(&sb, n)
	b := Block{ID: id, HTML: sb.String()}
	read := readNodes([]*html.Node{n}, false)
	if len(read.Content) == 0 {
		return b
	}
	doc := Normalize(read, nil)
	first := doc.Content[0]
	b.Type = first.Type
	if first.Type == "heading" {
		b.Level = first.attrInt("level", 1)
	}
	b.Text = oneLine(PlainText(doc))
	return b
}

// oneLine is text with every run of white space one space, trimmed.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
