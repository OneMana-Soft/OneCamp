package contentcodec

import (
	"bytes"
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// keptHTML writes a doc the way the collaboration service writes a body whose
// schema lacks every node and mark in it (preserved.js renderDocHTML, step 2):
// each node a marker element with its attributes as JSON and its content
// inside, each mark a marker span around what it marks. A block the web
// shipped before this server (V-528 (1)) is written exactly so.
func keptHTML(doc *Node) string {
	var b strings.Builder
	for _, c := range doc.Content {
		writeKept(&b, c)
	}
	return b.String()
}

func writeKept(b *strings.Builder, n *Node) {
	for _, m := range n.Marks {
		b.WriteString(`<span data-oc-mark="` + m.Type + `" data-oc-attrs="` + html.EscapeString(keptBlob(m.Attrs)) + `">`)
	}
	if n.Type == "text" {
		b.WriteString(html.EscapeString(n.Text))
	} else {
		tag := "div"
		if s := specOf(n.Type); s != nil && s.inline {
			tag = "span"
		}
		holds := "none"
		for _, c := range n.Content {
			holds = "block"
			if c.Type == "text" || specOf(c.Type) != nil && specOf(c.Type).inline {
				holds = "inline"
				break
			}
		}
		b.WriteString("<" + tag + ` data-oc-preserved="` + n.Type + `" data-oc-holds="` + holds + `" data-oc-attrs="` + html.EscapeString(keptBlob(n.Attrs)) + `">`)
		for _, c := range n.Content {
			writeKept(b, c)
		}
		b.WriteString("</" + tag + ">")
	}
	for range n.Marks {
		b.WriteString("</span>")
	}
}

// keptBlob is preserved.js blobOf: the attributes that are set, as JSON.
func keptBlob(attrs map[string]any) string {
	out := map[string]any{}
	for k, v := range attrs {
		if v != nil {
			out[k] = v
		}
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

// Every node and mark of every round-trip fixture, kept as a marker, reads
// back as itself: exports, AI reads, the API's GET and Blocks all read a body
// through this parser.
func TestPreservedMarkersReadBack(t *testing.T) {
	for _, f := range readIndex(t).RoundTrip {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			schema := schemaNamed(f.Schema)
			raw, err := os.ReadFile(filepath.Join(fixtureDir, f.Name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			doc, err := parseJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			kept := keptHTML(Normalize(doc, schema))
			back, err := FromHTML(kept, HTMLOptions{Schema: schema})
			if err != nil {
				t.Fatal(err)
			}
			if got := canon(t, back); !bytes.Equal(got, raw) {
				t.Errorf("kept body read back as a different doc:\n%s\nfrom:\n%s", got, kept)
			}
		})
	}
}

// canonOf is a doc's JSON in canonical form.
func canonOf(t *testing.T, src string) []byte {
	t.Helper()
	doc, err := parseJSON([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return canon(t, Normalize(doc, nil))
}

const keptBoard = "0b0e5a8e-3c1d-4f6a-9b2e-7d4c5a6b8c9d"

// keptWhiteboard is a doc body written while the web was a release ahead of
// the collaboration service: its whiteboard block is a marker.
const keptWhiteboard = `<p>Before</p><div data-oc-preserved="whiteboardEmbed" data-oc-holds="none" data-oc-attrs="{&quot;boardId&quot;:&quot;` + keptBoard + `&quot;}"></div><p>After</p>`

func TestPreservedWhiteboardReadsAsTheBlock(t *testing.T) {
	doc, err := FromHTML(keptWhiteboard, HTMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := FromHTML(`<p>Before</p><div data-type="whiteboard-embed" data-board-id="`+keptBoard+`"></div><p>After</p>`, HTMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, w := canon(t, doc), canon(t, want); !bytes.Equal(got, w) {
		t.Fatalf("marker read as\n%s\nwant\n%s", got, w)
	}
	if md := ToMarkdown(doc, MarkdownOptions{AppURL: "https://app.example.test"}); !strings.Contains(md, "/app/board/"+keptBoard) {
		t.Errorf("Markdown leaves the whiteboard out:\n%s", md)
	}
	blocks, err := Blocks(keptWhiteboard, BlockOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 3 || blocks[1].Type != "whiteboardEmbed" {
		t.Errorf("Blocks = %+v, want the whiteboard between two lines", blocks)
	}
}

// A marker's attributes are checked as a doc's JSON is: a board id that is no
// id is dropped, and so is an attribute the block has none of.
func TestPreservedAttributesAreChecked(t *testing.T) {
	doc, err := FromHTML(`<div data-oc-preserved="whiteboardEmbed" data-oc-holds="none" data-oc-attrs="{&quot;boardId&quot;:&quot;javascript:alert(1)&quot;,&quot;onclick&quot;:&quot;x&quot;}"></div>`, HTMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canon(t, doc), canonOf(t, `{"type":"doc","content":[{"type":"whiteboardEmbed"}]}`); !bytes.Equal(got, want) {
		t.Errorf("got %s", got)
	}
	// Broken JSON is no attributes.
	doc, err = FromHTML(`<div data-oc-preserved="whiteboardEmbed" data-oc-attrs="{nope"></div>`, HTMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canon(t, doc), canonOf(t, `{"type":"doc","content":[{"type":"whiteboardEmbed"}]}`); !bytes.Equal(got, want) {
		t.Errorf("got %s", got)
	}
}

// A marker for a name this codec does not know reads as before (its content),
// and markers in foreign HTML (an import, a paste) are only elements.
func TestPreservedUnknownAndForeign(t *testing.T) {
	doc, err := FromHTML(`<div data-oc-preserved="notABlockYet" data-oc-holds="inline" data-oc-attrs="{}">words</div>`, HTMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canon(t, doc), canonOf(t, `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"words"}]}]}`); !bytes.Equal(got, want) {
		t.Errorf("unknown: got %s", got)
	}
	doc, err = FromHTML(keptWhiteboard, HTMLOptions{Foreign: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canon(t, doc)), "whiteboardEmbed") {
		t.Errorf("foreign HTML read a marker: %s", canon(t, doc))
	}
}

// A marker's addresses pass the same checks as its tag twin's (V-528 (1),
// CR-220 F1): each node or mark below, written natively (ToHTML) and kept as
// a marker (keptHTML), reads back as the same doc, so an address the tag
// reader refuses never reaches an export, an AI read or the API's GET by way
// of a marker.
func TestPreservedAddressesReadAsTheirTagTwins(t *testing.T) {
	para := func(inline string) string {
		return `{"type":"doc","content":[{"type":"paragraph","content":[` + inline + `]}]}`
	}
	link := func(attrs string) string {
		return para(`{"type":"text","text":"words","marks":[{"type":"link","attrs":` + attrs + `}]}`)
	}
	block := func(typ, attrs string) string {
		return `{"type":"doc","content":[{"type":"` + typ + `","attrs":` + attrs + `}]}`
	}
	const png = `https://example.test/a.png`
	cases := []struct{ name, doc string }{
		{"link javascript", link(`{"href":"javascript:alert(1)"}`)},
		{"link scheme by reference", link(`{"href":"java&#x73;cript:alert(1)"}`)},
		{"link scheme split by a tab", link(`{"href":"java\tscript:alert(1)"}`)},
		{"link data", link(`{"href":"data:text/html,<script>alert(1)</script>"}`)},
		{"link class", link(`{"href":"https://example.test","class":"evil link"}`)},
		{"link kept", link(`{"href":"https://example.test/x","target":"_self","rel":"noopener"}`)},
		{"image javascript", block("image", `{"src":"javascript:alert(1)","alt":"A cat"}`)},
		{"image svg data", block("image", `{"src":"data:image/svg+xml;base64,PHN2Zz4=","alt":"A cat"}`)},
		{"image blob", block("image", `{"src":"blob:https://example.test/1"}`)},
		{"image link, align, crop", block("image", `{"src":"`+png+`","href":"javascript:alert(1)","align":"evil","crop":"9,9,9,9"}`)},
		{"image kept", block("image", `{"src":"`+png+`","href":"https://example.test","align":"center","crop":"0,0,0.5,0.5","alt":"A cat"}`)},
		{"image src trimmed", block("image", `{"src":"  `+png+`  "}`)},
		{"bookmark javascript", block("bookmark", `{"href":"javascript:alert(1)","title":"T"}`)},
		{"bookmark icon, image", block("bookmark", `{"href":"https://example.test","icon":"javascript:alert(1)","image":"data:image/png;base64,AAAA"}`)},
		{"bookmark kept", block("bookmark", `{"href":"https://example.test","icon":"https://example.test/i.png","image":"/api/files/1"}`)},
		{"webEmbed javascript", block("webEmbed", `{"src":"javascript:alert(1)"}`)},
		{"webEmbed space", block("webEmbed", `{"src":" https://example.test"}`)},
		{"webEmbed kept", block("webEmbed", `{"src":"https://example.test/e","height":300}`)},
		{"fileBlock javascript", block("fileBlock", `{"kind":"pdf","src":"javascript:alert(1)","name":"a.pdf"}`)},
		{"fileBlock import path", block("fileBlock", `{"kind":"file","src":"files/a.pdf","name":"a.pdf"}`)},
		{"fileBlock kept", block("fileBlock", `{"kind":"video","src":"/api/files/1","name":"a.mp4","size":10}`)},
		{"linkMention javascript", para(`{"type":"linkMention","attrs":{"href":"javascript:alert(1)","title":"T"}}`)},
		{"linkMention icon", para(`{"type":"linkMention","attrs":{"href":"https://example.test","title":"T","icon":"javascript:alert(1)"}}`)},
		{"linkMention kept", para(`{"type":"linkMention","attrs":{"href":"https://example.test","title":"T","icon":"https://example.test/i.png","siteName":"S"}}`)},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			src, err := parseJSON([]byte(c.doc))
			if err != nil {
				t.Fatal(err)
			}
			doc := Normalize(src, nil)
			native, err := FromHTML(ToHTML(doc), HTMLOptions{})
			if err != nil {
				t.Fatal(err)
			}
			kept, err := FromHTML(keptHTML(doc), HTMLOptions{})
			if err != nil {
				t.Fatal(err)
			}
			got, want := canon(t, kept), canon(t, native)
			if !bytes.Equal(got, want) {
				t.Errorf("the marker read as\n%s\nits tag twin as\n%s\nfrom %s", got, want, keptHTML(doc))
			}
			for _, bad := range []string{"javascript", "evil", "blob:", "svg", "data:text", "9,9"} {
				if strings.Contains(string(got), bad) {
					t.Errorf("%q survived: %s", bad, got)
				}
			}
		})
	}
}
