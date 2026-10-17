package contentcodec

import (
	_ "embed"
	"encoding/json"
)

// What a node may hold.
type contentKind int

const (
	holdsNothing     contentKind = iota // an atom or a leaf
	holdsBlocks                         // block+
	holdsInline                         // inline*
	holdsText                           // text*, unmarked (a code block)
	holdsListItems                      // listItem+
	holdsTaskItems                      // taskItem+
	holdsItemBody                       // paragraph block*
	holdsRows                           // tableRow+
	holdsCells                          // (tableCell | tableHeader)*
	holdsSummaryBody                    // (paragraph | heading) block*: a toggle's summary, then what it folds
	holdsColumns                        // column+
)

type attrSpec struct {
	name string
	def  any // nil: null
}

type nodeSpec struct {
	name    string
	inline  bool
	content contentKind
	attrs   []attrSpec
}

type markSpec struct {
	name string
	// rank orders marks on a node and nests them in HTML, outermost first: the
	// order the editor's schema has them in.
	rank        int
	attrs       []attrSpec
	excludesAll bool
}

// The nodes and marks the codec knows, with the attributes the editor gives
// them and their defaults. schema_test.go holds this to the schema manifest
// (schema-manifest.json, generated from the web editor's Block Registry): a
// type the editor writes with no rule here fails that test.
var nodeSpecs = map[string]*nodeSpec{
	"doc":        {name: "doc", content: holdsBlocks},
	"paragraph":  {name: "paragraph", content: holdsInline},
	"text":       {name: "text", inline: true},
	"heading":    {name: "heading", content: holdsInline, attrs: []attrSpec{{"level", float64(1)}}},
	"blockquote": {name: "blockquote", content: holdsBlocks},
	"bulletList": {name: "bulletList", content: holdsListItems},
	"orderedList": {name: "orderedList", content: holdsListItems,
		attrs: []attrSpec{{"start", float64(1)}, {"type", nil}}},
	"listItem": {name: "listItem", content: holdsItemBody},
	"taskList": {name: "taskList", content: holdsTaskItems},
	"taskItem": {name: "taskItem", content: holdsItemBody, attrs: []attrSpec{{"checked", false}}},
	// wrap is true or absent; caption is plain text (B3).
	"codeBlock":      {name: "codeBlock", content: holdsText, attrs: []attrSpec{{"language", nil}, {"wrap", nil}, {"caption", nil}}},
	"horizontalRule": {name: "horizontalRule"},
	"hardBreak":      {name: "hardBreak", inline: true},
	"image": {name: "image", attrs: []attrSpec{
		{"src", nil}, {"alt", nil}, {"title", nil}, {"width", nil}, {"height", nil},
		{"id", nil}, {"caption", nil}, {"fileName", nil},
	}},
	"callout":     {name: "callout", content: holdsBlocks, attrs: []attrSpec{{"emoji", "💡"}, {"color", "blue"}, {"icon", nil}}},
	"collapsible": {name: "collapsible", content: holdsSummaryBody},
	"toc":         {name: "toc"},
	"tableEmbed":  {name: "tableEmbed", attrs: []attrSpec{{"tableId", ""}, {"viewId", ""}}},
	"poll":        {name: "poll", attrs: []attrSpec{{"id", ""}}},
	"chartEmbed":  {name: "chartEmbed", attrs: []attrSpec{{"spec", ""}}},
	"diffEmbed":   {name: "diffEmbed", attrs: []attrSpec{{"diff", ""}}},
	"recordingEmbed": {name: "recordingEmbed", attrs: []attrSpec{
		{"egress", ""}, {"kind", ""}, {"sid", ""}, {"u1", ""}, {"u2", ""},
	}},
	"mention": {name: "mention", inline: true,
		attrs: []attrSpec{{"id", nil}, {"label", nil}, {"mentionSuggestionChar", "@"}}},
	"channelMention": {name: "channelMention", inline: true,
		attrs: []attrSpec{{"id", nil}, {"label", nil}, {"mentionSuggestionChar", "@"}}},
	"entityMention": {name: "entityMention", inline: true,
		attrs: []attrSpec{{"id", nil}, {"label", nil}, {"mentionSuggestionChar", "@"}}},
	// Equations (B3): their TeX, with Tiptap's names.
	"inlineMath": {name: "inlineMath", inline: true, attrs: []attrSpec{{"latex", ""}}},
	"blockMath":  {name: "blockMath", attrs: []attrSpec{{"latex", ""}}},
	// The simple table's nodes (Tiptap's own names; B3). A schema without them
	// keeps a table read from Markdown or HTML as its Markdown in a code block
	// (see degrade.go).
	"table":       {name: "table", content: holdsRows},
	"tableRow":    {name: "tableRow", content: holdsCells},
	"tableHeader": {name: "tableHeader", content: holdsBlocks, attrs: cellAttrs},
	"tableCell":   {name: "tableCell", content: holdsBlocks, attrs: cellAttrs},
	// B1's columns: a row of columns, each holding blocks, a whole-percent
	// width or an equal share.
	"columnList": {name: "columnList", content: holdsColumns},
	"column":     {name: "column", content: holdsBlocks, attrs: []attrSpec{{"width", nil}}},
}

// cellAttrs are a table cell's attributes; background is a colour by name
// (B3, N15), one of DocColours (colours.go).
var cellAttrs = []attrSpec{{"colspan", float64(1)}, {"rowspan", float64(1)}, {"colwidth", nil}, {"background", nil}}

var markSpecs = map[string]*markSpec{
	"link": {name: "link", rank: 0, attrs: []attrSpec{
		{"href", nil}, {"target", "_blank"}, {"rel", "noopener noreferrer nofollow"}, {"class", nil},
	}},
	"textStyle": {name: "textStyle", rank: 1, attrs: []attrSpec{{"color", nil}}},
	"bold":      {name: "bold", rank: 2},
	"code":      {name: "code", rank: 3, excludesAll: true},
	"italic":    {name: "italic", rank: 4},
	"strike":    {name: "strike", rank: 5},
	"underline": {name: "underline", rank: 6},
	"highlight": {name: "highlight", rank: 7, attrs: []attrSpec{{"color", "sun"}}},
}

func specOf(typ string) *nodeSpec { return nodeSpecs[typ] }

// Schema is the set of node and mark types a doc may hold: what the editor
// this codec writes for knows.
type Schema struct {
	nodes map[string]bool
	marks map[string]bool
}

// Has reports whether the schema has a node type.
func (s *Schema) Has(node string) bool { return s == nil || s.nodes[node] }

// HasMark reports whether the schema has a mark type.
func (s *Schema) HasMark(mark string) bool { return s == nil || s.marks[mark] }

// Manifest is the schema manifest's shape: every node and mark the editor
// writes, with its attributes.
type Manifest struct {
	Nodes map[string]ManifestType `json:"nodes"`
	Marks map[string]ManifestType `json:"marks"`
}

// ManifestType is one entry of the manifest.
type ManifestType struct {
	Attrs   []string `json:"attrs"`
	Group   *string  `json:"group,omitempty"`
	Content *string  `json:"content,omitempty"`
	Inline  bool     `json:"inline,omitempty"`
	Atom    bool     `json:"atom,omitempty"`
}

//go:embed schema-manifest.json
var manifestJSON []byte

// shippedManifest is the manifest this build was made with.
func shippedManifest() (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

var shippedSchema = func() *Schema {
	m, err := shippedManifest()
	if err != nil {
		panic("contentcodec: the embedded schema manifest does not parse: " + err.Error())
	}
	return schemaOf(m)
}()

// shipped is the schema the editor of this release has.
func shipped() *Schema { return shippedSchema }

// schemaOf is the schema a manifest describes.
func schemaOf(m *Manifest) *Schema {
	s := &Schema{nodes: map[string]bool{}, marks: map[string]bool{}}
	for k := range m.Nodes {
		s.nodes[k] = true
	}
	for k := range m.Marks {
		s.marks[k] = true
	}
	return s
}

// everything is every type the codec has rules for, the simple table
// included. Tests use it to exercise rules the shipped editor has no block
// for yet.
func everything() *Schema {
	s := &Schema{nodes: map[string]bool{}, marks: map[string]bool{}}
	for k := range nodeSpecs {
		s.nodes[k] = true
	}
	for k := range markSpecs {
		s.marks[k] = true
	}
	return s
}
