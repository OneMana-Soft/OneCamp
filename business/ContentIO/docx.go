package business

import (
	"context"
	"encoding/xml"
	"io"
	"path"
	"strconv"
	"strings"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
)

// The Word importer: a pure-Go reader of the parts of a .docx a doc can
// hold, built on archive/zip and encoding/xml, so no converter has to run on
// the server. Headings (by style), paragraphs, bold, italic, underline,
// strikethrough, code-styled runs, links, line breaks, bulleted and numbered
// lists (nested), tables, and images (uploaded as the doc's attachments).

func init() { Pipeline.RegisterImporter(docxImporter{}) }

type docxImporter struct{}

func (docxImporter) Format() string { return "docx" }

func (docxImporter) Extensions() []string { return []string{".docx"} }

func (docxImporter) Accepts(filename, mediaType string) bool {
	return hasExt(filename, ".docx") ||
		baseMediaType(mediaType) == "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
}

func (docxImporter) Import(_ context.Context, in Input, out Output) error {
	pg, err := docxPage(in.Filename, in.File, in.Size, nil)
	if err != nil {
		return err
	}
	return out.Page(pg)
}

// maxDocxPart bounds one XML part of a Word file once unpacked.
const maxDocxPart = 50 << 20

// docxPage reads a Word file as a page, unpacking it within budget (nil: one
// of its own; a Word file in an import's zip takes the zip's): its parts and
// its images alike, by the rule every zip an import reads keeps
// (openPackage, unpackBudget).
func docxPage(key string, r io.ReaderAt, size int64, budget *unpackBudget) (Page, error) {
	z, err := openPackage(r, size, budget)
	if err != nil {
		return Page{}, readError{"This is not a Word file (.docx) that can be read."}
	}
	read := func(name string) []byte {
		data, err := z.Read(name, maxDocxPart)
		if err != nil {
			return nil
		}
		return data
	}
	main := read("word/document.xml")
	if main == nil {
		return Page{}, readError{"This is not a Word file (.docx) that can be read."}
	}
	d := &docxReader{
		rels:     docxRels(read("word/_rels/document.xml.rels")),
		styles:   docxStyles(read("word/styles.xml")),
		numbers:  docxNumbering(read("word/numbering.xml")),
		mediaDir: "word",
	}
	body := d.read(main)
	title := d.title
	if title == "" {
		title, body = titleFromBody(body, fileTitle(key))
	}
	return Page{Key: key, Title: title, Body: body, Assets: docxAssets{assets: z.Assets(), key: key}}, nil
}

// docxAssets opens a Word file's images, which the body names by their path
// in the package, relative to the page key's folder, through its package's
// own opener: within the budget, and never past the size each declares.
type docxAssets struct {
	assets AssetOpener
	key    string
}

func (a docxAssets) Open(p string) (io.ReadCloser, int64, error) {
	name := strings.TrimPrefix(p, path.Dir(a.key)+"/")
	name = strings.TrimPrefix(name, "./")
	return a.assets.Open(name)
}

type docxStyle struct {
	heading int  // 1–6, 0 when it is no heading
	title   bool // the document's title
	code    bool // a code or verbatim style
	quote   bool
	list    bool
}

func docxStyles(data []byte) map[string]docxStyle {
	out := map[string]docxStyle{}
	if data == nil {
		return out
	}
	var doc struct {
		Styles []struct {
			ID   string `xml:"styleId,attr"`
			Name struct {
				Val string `xml:"val,attr"`
			} `xml:"name"`
		} `xml:"style"`
	}
	_ = xml.Unmarshal(data, &doc)
	for _, s := range doc.Styles {
		out[s.ID] = styleFromName(s.Name.Val, s.ID)
	}
	return out
}

// styleFromName reads what a style means from its name ("heading 2",
// "Title", "Quote", "Code"), as Word, LibreOffice and Google Docs name them.
func styleFromName(name, id string) docxStyle {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		n = strings.ToLower(id)
	}
	var s docxStyle
	compact := strings.ReplaceAll(n, " ", "")
	switch {
	case compact == "title":
		s.title = true
	case strings.HasPrefix(compact, "heading"):
		if l, err := strconv.Atoi(strings.TrimPrefix(compact, "heading")); err == nil && l >= 1 {
			s.heading = min(l, 6)
		}
	case strings.Contains(compact, "quote"):
		s.quote = true
	case strings.Contains(compact, "code") || strings.Contains(compact, "preformatted") || strings.Contains(compact, "sourcetext"):
		s.code = true
	case strings.Contains(compact, "listparagraph") || strings.Contains(compact, "listbullet") || strings.Contains(compact, "listnumber"):
		s.list = true
	}
	return s
}

// docxNumbering maps a list's numId to whether each level is numbered.
func docxNumbering(data []byte) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	if data == nil {
		return out
	}
	var doc struct {
		Abstract []struct {
			ID     string `xml:"abstractNumId,attr"`
			Levels []struct {
				Level  int `xml:"ilvl,attr"`
				Format struct {
					Val string `xml:"val,attr"`
				} `xml:"numFmt"`
			} `xml:"lvl"`
		} `xml:"abstractNum"`
		Nums []struct {
			ID       string `xml:"numId,attr"`
			Abstract struct {
				Val string `xml:"val,attr"`
			} `xml:"abstractNumId"`
		} `xml:"num"`
	}
	_ = xml.Unmarshal(data, &doc)
	abstract := map[string]map[int]bool{}
	for _, a := range doc.Abstract {
		levels := map[int]bool{}
		for _, l := range a.Levels {
			f := l.Format.Val
			levels[l.Level] = f != "bullet" && f != "none" && f != ""
		}
		abstract[a.ID] = levels
	}
	for _, n := range doc.Nums {
		out[n.ID] = abstract[n.Abstract.Val]
	}
	return out
}

type docxRel struct {
	target   string
	external bool
}

func docxRels(data []byte) map[string]docxRel {
	out := map[string]docxRel{}
	if data == nil {
		return out
	}
	var doc struct {
		Rels []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
			Mode   string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	_ = xml.Unmarshal(data, &doc)
	for _, r := range doc.Rels {
		out[r.ID] = docxRel{target: r.Target, external: strings.EqualFold(r.Mode, "External")}
	}
	return out
}

type docxReader struct {
	rels     map[string]docxRel
	styles   map[string]docxStyle
	numbers  map[string]map[int]bool
	mediaDir string
	title    string
}

// docxPara is one paragraph as read, before lists are put together.
type docxPara struct {
	node   *contentcodec.Node // a paragraph, heading or code line
	image  []*contentcodec.Node
	list   bool
	numID  string
	level  int
	style  docxStyle
	table  *contentcodec.Node
	isCode bool
}

// read reads word/document.xml's body.
func (d *docxReader) read(data []byte) *contentcodec.Node {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	var paras []docxPara
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "p":
			paras = append(paras, d.paragraph(dec, se))
		case "tbl":
			paras = append(paras, docxPara{table: d.table(dec, se)})
		}
	}
	doc := &contentcodec.Node{Type: "doc", Content: d.assemble(paras)}
	return contentcodec.Normalize(doc, nil)
}

// assemble turns paragraphs into blocks: list paragraphs into nested lists,
// code lines into one code block.
func (d *docxReader) assemble(paras []docxPara) []*contentcodec.Node {
	var out []*contentcodec.Node
	type open struct {
		list *contentcodec.Node
		item *contentcodec.Node
	}
	var stack []open
	var code []string
	flushCode := func() {
		if code == nil {
			return
		}
		out = append(out, &contentcodec.Node{Type: "codeBlock", Content: []*contentcodec.Node{{Type: "text", Text: strings.Join(code, "\n")}}})
		code = nil
	}
	for _, p := range paras {
		if p.isCode {
			stack = nil
			code = append(code, contentcodec.PlainText(p.node))
			continue
		}
		flushCode()
		if p.table != nil {
			stack = nil
			out = append(out, p.table)
			continue
		}
		if !p.list {
			stack = nil
			if p.node != nil && p.node.Content != nil || p.node != nil && p.node.Type == "heading" {
				out = append(out, p.node)
			}
			out = append(out, p.image...)
			continue
		}
		numbered := d.numbers[p.numID][p.level]
		kind := "bulletList"
		if numbered {
			kind = "orderedList"
		}
		for len(stack) > p.level+1 {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == p.level+1 && stack[p.level].list.Type != kind {
			stack = stack[:p.level]
		}
		for len(stack) < p.level+1 {
			l := &contentcodec.Node{Type: kind}
			if len(stack) == 0 {
				out = append(out, l)
			} else {
				parent := stack[len(stack)-1].item
				if parent == nil {
					parent = &contentcodec.Node{Type: "listItem"}
					stack[len(stack)-1].list.Content = append(stack[len(stack)-1].list.Content, parent)
					stack[len(stack)-1].item = parent
				}
				parent.Content = append(parent.Content, l)
			}
			stack = append(stack, open{list: l})
		}
		top := &stack[len(stack)-1]
		item := &contentcodec.Node{Type: "listItem", Content: []*contentcodec.Node{p.node}}
		item.Content = append(item.Content, p.image...)
		top.list.Content = append(top.list.Content, item)
		top.item = item
	}
	flushCode()
	return out
}

func attr(se xml.StartElement, name string) string {
	for _, a := range se.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// paragraph reads one <w:p>.
func (d *docxReader) paragraph(dec *xml.Decoder, start xml.StartElement) docxPara {
	var p docxPara
	var inline []*contentcodec.Node
	var link string
	var run docxRun
	inRun := false
	pendingAlt := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "pStyle":
				p.style = d.styles[attr(t, "val")]
				if p.style == (docxStyle{}) {
					p.style = styleFromName("", attr(t, "val"))
				}
			case "numId":
				p.numID = attr(t, "val")
				if p.numID != "0" {
					p.list = true
				}
			case "ilvl":
				p.level, _ = strconv.Atoi(attr(t, "val"))
				if p.level > 8 {
					p.level = 8
				}
			case "hyperlink":
				if rel, ok := d.rels[attr(t, "id")]; ok && rel.external {
					link = rel.target
				} else if a := attr(t, "anchor"); a != "" {
					link = ""
				}
			case "r":
				inRun, run = true, docxRun{}
			case "rPr":
				run.props(dec)
			case "t":
				if inRun {
					var s string
					_ = dec.DecodeElement(&s, &t)
					inline = append(inline, run.text(s, link))
				}
			case "tab":
				if inRun {
					inline = append(inline, run.text("\t", link))
				}
			case "br", "cr":
				if inRun && attr(t, "type") != "page" {
					inline = append(inline, &contentcodec.Node{Type: "hardBreak"})
				}
			case "docPr":
				// A drawing's description, read before its picture.
				pendingAlt = attr(t, "descr")
			case "blip":
				if rel, ok := d.rels[attr(t, "embed")]; ok && !rel.external {
					img := &contentcodec.Node{Type: "image", Attrs: map[string]any{"src": path.Join(d.mediaDir, rel.target)}}
					if pendingAlt != "" {
						img.Attrs["alt"] = pendingAlt
					}
					p.image = append(p.image, img)
				}
				pendingAlt = ""
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "r":
				inRun = false
			case "hyperlink":
				link = ""
			case start.Name.Local:
				if t.Name.Space == start.Name.Space {
					return d.finishParagraph(p, inline)
				}
			}
		}
	}
	return d.finishParagraph(p, inline)
}

func (d *docxReader) finishParagraph(p docxPara, inline []*contentcodec.Node) docxPara {
	switch {
	case p.style.title:
		if t := strings.TrimSpace(contentcodec.PlainText(&contentcodec.Node{Type: "paragraph", Content: inline})); t != "" && d.title == "" {
			d.title = t
			return docxPara{image: p.image}
		}
		p.node = &contentcodec.Node{Type: "heading", Attrs: map[string]any{"level": float64(1)}, Content: inline}
	case p.style.heading > 0:
		p.node = &contentcodec.Node{Type: "heading", Attrs: map[string]any{"level": float64(p.style.heading)}, Content: inline}
	case p.style.code:
		p.isCode = true
		p.node = &contentcodec.Node{Type: "paragraph", Content: inline}
	case p.style.quote:
		p.node = &contentcodec.Node{Type: "blockquote", Content: []*contentcodec.Node{{Type: "paragraph", Content: inline}}}
	default:
		p.node = &contentcodec.Node{Type: "paragraph", Content: inline}
	}
	if p.node.Type == "blockquote" && len(inline) == 0 {
		p.node = &contentcodec.Node{Type: "paragraph"}
	}
	return p
}

// docxRun is a run's formatting.
type docxRun struct {
	bold, italic, underline, strike, code bool
}

// props reads a <w:rPr>.
func (r *docxRun) props(dec *xml.Decoder) {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			on := func() bool {
				v := strings.ToLower(attr(t, "val"))
				return v != "0" && v != "false" && v != "none"
			}
			switch t.Name.Local {
			case "b":
				r.bold = on()
			case "i":
				r.italic = on()
			case "u":
				r.underline = on()
			case "strike", "dstrike":
				r.strike = on()
			case "rFonts":
				f := strings.ToLower(attr(t, "ascii"))
				r.code = strings.Contains(f, "courier") || strings.Contains(f, "consolas") || strings.Contains(f, "mono")
			case "rStyle":
				s := strings.ToLower(attr(t, "val"))
				if strings.Contains(s, "code") || strings.Contains(s, "verbatim") {
					r.code = true
				}
			}
		case xml.EndElement:
			depth--
		}
	}
}

func (r docxRun) text(s, link string) *contentcodec.Node {
	n := &contentcodec.Node{Type: "text", Text: s}
	add := func(typ string) { n.Marks = append(n.Marks, &contentcodec.Mark{Type: typ}) }
	if r.bold {
		add("bold")
	}
	if r.italic {
		add("italic")
	}
	if r.underline && link == "" {
		add("underline")
	}
	if r.strike {
		add("strike")
	}
	if r.code {
		add("code")
	}
	if link != "" {
		n.Marks = append(n.Marks, &contentcodec.Mark{Type: "link", Attrs: map[string]any{"href": link}})
	}
	return n
}

// table reads a <w:tbl>: its first row is the header.
func (d *docxReader) table(dec *xml.Decoder, start xml.StartElement) *contentcodec.Node {
	tbl := &contentcodec.Node{Type: "table"}
	var row *contentcodec.Node
	var cell *contentcodec.Node
	var cellParas []docxPara
	for {
		tok, err := dec.Token()
		if err != nil {
			return tbl
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tr":
				row = &contentcodec.Node{Type: "tableRow"}
			case "tc":
				typ := "tableCell"
				if len(tbl.Content) == 0 {
					typ = "tableHeader"
				}
				cell = &contentcodec.Node{Type: typ}
				cellParas = nil
			case "p":
				if cell != nil {
					cellParas = append(cellParas, d.paragraph(dec, t))
				}
			case "tbl":
				// A table in a table: its text, as paragraphs.
				inner := d.table(dec, t)
				if cell != nil {
					cellParas = append(cellParas, docxPara{node: &contentcodec.Node{Type: "paragraph", Content: []*contentcodec.Node{{Type: "text", Text: contentcodec.PlainText(inner)}}}})
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "tc":
				if cell != nil && row != nil {
					cell.Content = d.assemble(cellParas)
					row.Content = append(row.Content, cell)
				}
				cell = nil
			case "tr":
				if row != nil {
					tbl.Content = append(tbl.Content, row)
				}
				row = nil
			case "tbl":
				return tbl
			}
		}
	}
}
