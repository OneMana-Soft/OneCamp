package miro

// A Miro board as a OneCamp board, drawn. Pure: the items, connectors, tags
// and groups Miro's API answered go in; the board's Excalidraw elements, the
// pictures they show and the board's report come out.
//
// Positions: Miro gives an item's centre, relative to the canvas's centre or
// to its parent frame's top left; a board's elements are placed by their top
// left on one plane, so each is made absolute (a parent's own place first),
// then moved by half its size. Rotation is degrees there, radians here.
//
// Order: the API promises none, so the items go in the order it answered,
// frames last after their children (as Excalidraw keeps a frame above what
// is in it), connectors on top.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/business/Board/scene"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// source is what the API answered for one board.
type source struct {
	items      []item
	connectors []connector
	// tags are each item's tags' titles, by the item's id.
	tags map[string][]string
	// groups are the board's groups, each its items' ids, by the group's id.
	groups map[string][]string
	// styleless counts the items whose style couldn't be read, so came over in
	// their default colours.
	styleless int
}

// drawn is a board drawn.
type drawn struct {
	elements []scene.Element
	pictures []importProvider.BoardPicture
	notes    []importProvider.BoardNote
}

// box is an item's place on the board: its top left, size and angle.
type box struct{ x, y, w, h, angle float64 }

func (b box) centre() scene.Point { return scene.Point{X: b.x + b.w/2, Y: b.y + b.h/2} }

// elementID is the element an item becomes: the same on every import of the
// board, so a board brought again is the same drawing.
func elementID(itemID id) string { return "miro-" + string(itemID) }

// Default sizes, for an item that comes without one.
var defaultSize = map[string][2]float64{
	"sticky_note": {200, 200}, "shape": {200, 120}, "text": {300, 40}, "frame": {800, 600},
	"image": {320, 240}, "card": {320, 120}, "app_card": {320, 140},
	"document": {320, 120}, "embed": {320, 180}, "preview": {320, 120},
}

// The words of a placeholder for what couldn't come over, by Miro's type.
var notBrought = map[string]string{
	"doc_format": "a Miro doc", "data_table_format": "a Miro table", "mindmap_node": "a mind map node",
	"kanban": "a Miro kanban", "table": "a table", "usm": "a story map", "mockup": "a mockup",
	"stroke": "a drawing", "emoji": "a sticker", "code_widget": "a code block",
}

// converter keeps a board's drawing as it is made.
type converter struct {
	src      source
	now      int64
	byID     map[string]*item
	boxes    map[string]box
	frames   map[string]bool
	main     map[string]scene.Element   // an item's main element, by item id
	own      map[string][]scene.Element // all of an item's elements, by item id
	elByID   map[string]scene.Element
	pictures []importProvider.BoardPicture
	counts   map[string]int // the report's counts, by code
	missing  map[string]int // what couldn't come, by what it was
}

func convert(src source, now int64) drawn {
	c := &converter{
		src: src, now: now, byID: map[string]*item{}, boxes: map[string]box{}, frames: map[string]bool{},
		main: map[string]scene.Element{}, own: map[string][]scene.Element{}, elByID: map[string]scene.Element{},
		counts: map[string]int{}, missing: map[string]int{},
	}
	for i := range src.items {
		it := &src.items[i]
		if it.ID == "" {
			continue
		}
		c.byID[string(it.ID)] = it
		if it.Type == "frame" {
			c.frames[string(it.ID)] = true
		}
	}
	for _, it := range src.items {
		if it.ID != "" {
			c.place(string(it.ID), 0)
		}
	}

	// Each item drawn, in the API's order.
	for _, it := range src.items {
		if it.ID == "" || c.own[string(it.ID)] != nil {
			continue
		}
		els := c.draw(it)
		if len(els) == 0 {
			continue
		}
		c.own[string(it.ID)] = els
		for _, el := range els {
			c.elByID[el["id"].(string)] = el
		}
		// Its main element carries the item's own id; the rest are its words and fills.
		if m := c.elByID[elementID(it.ID)]; m != nil {
			c.main[string(it.ID)] = m
		}
	}
	c.groups()

	// Top-level items first, then each frame after what is in it.
	var out []scene.Element
	children := map[string][]string{}
	for _, it := range src.items {
		pid := c.frameOf(it)
		if pid != "" {
			children[pid] = append(children[pid], string(it.ID))
		}
	}
	for _, it := range src.items {
		if c.own[string(it.ID)] == nil || it.Type == "frame" || c.frameOf(it) != "" {
			continue
		}
		out = append(out, c.own[string(it.ID)]...)
	}
	for _, it := range src.items {
		if it.Type != "frame" || c.own[string(it.ID)] == nil {
			continue
		}
		// Its fill behind its children, and the frame itself after them.
		var frame scene.Element
		for _, el := range c.own[string(it.ID)] {
			if el["type"] == "frame" {
				frame = el
			} else {
				out = append(out, el)
			}
		}
		for _, child := range children[string(it.ID)] {
			out = append(out, c.own[child]...)
		}
		out = append(out, frame)
	}
	out = append(out, c.connectors()...)
	scene.Index(out)
	return drawn{elements: out, pictures: c.pictures, notes: c.report()}
}

// place works out an item's box on the board, its parent's first.
func (c *converter) place(itemID string, depth int) box {
	if b, ok := c.boxes[itemID]; ok {
		return b
	}
	it := c.byID[itemID]
	if it == nil {
		return box{}
	}
	size := defaultSize[it.Type]
	if size == [2]float64{} {
		size = [2]float64{200, 120}
	}
	w, h, rot := size[0], size[1], 0.0
	if g := it.Geometry; g != nil {
		if g.Width > 0 {
			w = float64(g.Width)
		}
		if g.Height > 0 {
			h = float64(g.Height)
		} else if g.Width > 0 && it.Type == "sticky_note" {
			h = w // a square note given only its width
		}
		rot = float64(g.Rotation)
	}
	var cx, cy float64
	if p := it.Position; p != nil {
		cx, cy = float64(p.X), float64(p.Y)
		if p.RelativeTo == "parent_top_left" && it.Parent != nil && depth < 16 {
			if parent := c.byID[string(it.Parent.ID)]; parent != nil {
				pb := c.place(string(it.Parent.ID), depth+1)
				cx, cy = pb.x+cx, pb.y+cy
			}
		}
	}
	b := box{x: cx - w/2, y: cy - h/2, w: w, h: h, angle: rot * math.Pi / 180}
	c.boxes[itemID] = b
	return b
}

// frameOf is the frame an item is in, or "": Excalidraw's frames don't hold
// frames, so a frame in a frame stands on its own.
func (c *converter) frameOf(it item) string {
	if it.Parent == nil || it.Type == "frame" {
		return ""
	}
	if pid := string(it.Parent.ID); c.frames[pid] {
		return pid
	}
	return ""
}

func (c *converter) updated(it item) int64 {
	if t, err := time.Parse(time.RFC3339, it.ModifiedAt); err == nil {
		return t.UnixMilli()
	}
	return c.now
}

func (c *converter) note(code string) { c.counts[code]++ }

// draw is one item's elements, in the order they're drawn; its main one
// carries the item's own element id.
func (c *converter) draw(it item) []scene.Element {
	b := c.boxes[string(it.ID)]
	eid := elementID(it.ID)
	upd := c.updated(it)
	var els []scene.Element
	switch it.Type {
	case "sticky_note":
		els = c.sticky(it, b, eid, upd)
	case "shape":
		els = c.shape(it, b, eid, upd)
	case "text":
		els = c.text(it, b, eid, upd)
	case "frame":
		els = c.frame(it, b, eid, upd)
	case "image":
		els = c.image(it, b, eid, upd)
	case "card":
		els = c.card(it, b, eid, upd)
	case "app_card":
		els = c.appCard(it, b, eid, upd)
	case "document":
		var d documentData
		_ = json.Unmarshal(it.Data, &d)
		title := strings.TrimSpace(d.Title)
		if title == "" {
			title = "A file"
		}
		c.note("DOCUMENT_AS_CARD")
		els = card(eid, b, upd, title+"\nThe file stays in Miro.", "#9a9da3", "#F5F6F7", "", map[string]any{"kind": "file", "source": "miro"})
	case "embed":
		var d embedData
		_ = json.Unmarshal(it.Data, &d)
		els = c.linkCard(eid, b, upd, d.URL, firstOf(d.Title, d.ProviderName))
	case "preview":
		var d previewData
		_ = json.Unmarshal(it.Data, &d)
		els = c.linkCard(eid, b, upd, d.URL, d.Title)
	default:
		what := notBrought[it.Type]
		if what == "" {
			what = "a " + strings.ReplaceAll(it.Type, "_", " ")
			if it.Type == "" {
				what = "an item"
			}
		}
		c.missing[what]++
		els = scene.Placeholder(eid, b.x, b.y, math.Max(b.w, 160), math.Max(b.h, 80), "Not brought over from Miro: "+what, upd)
	}
	if fid := c.frameOf(it); fid != "" {
		for _, el := range els {
			el["frameId"] = elementID(id(fid))
		}
	}
	if tags := c.src.tags[string(it.ID)]; len(tags) > 0 && len(els) > 0 {
		if data, ok := els[0]["customData"].(map[string]any); ok && data["tags"] == nil {
			if t := scene.NormaliseTags(tags); len(t) > 0 {
				list := make([]any, len(t))
				for i, s := range t {
					list[i] = s
				}
				data["tags"] = list
			}
		}
	}
	return els
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// words reads an item's HTML, noting formatting it loses.
func (c *converter) words(htmlText string) (string, string) {
	text, link, formatted := plainText(htmlText)
	if formatted {
		c.note("RICH_TEXT_PLAIN")
	}
	return text, link
}

func (c *converter) font(name string) int {
	if name == "" {
		return scene.FontSans
	}
	f, ok := fonts[name]
	if !ok {
		c.note("FONT_NEAREST")
		return scene.FontSans
	}
	if !sansExact[name] {
		c.note("FONT_NEAREST")
	}
	return f
}

func (c *converter) sticky(it item, b box, eid string, upd int64) []scene.Element {
	var d stickyData
	var s stickyStyle
	_ = json.Unmarshal(it.Data, &d)
	_ = json.Unmarshal(it.Style, &s)
	colour, known := scene.NoteColourFromMiro(s.FillColor)
	if !known && s.FillColor != "" {
		c.note("NOTE_COLOUR_NEAREST")
	}
	text, link := c.words(d.Content)
	return scene.Note{
		ID: eid, X: b.x, Y: b.y, W: b.w, H: b.h, Angle: b.angle, Colour: colour, Text: text,
		Align: align(s.TextAlign, "center"), VAlign: valign(s.TextAlignVertical, "middle"), Link: link,
		Data: map[string]any{"source": "miro"},
	}.Elements(upd)
}

func align(a, def string) string {
	switch a {
	case "left", "center", "right":
		return a
	}
	return def
}

func valign(a, def string) string {
	switch a {
	case "top", "middle", "bottom":
		return a
	}
	return def
}

// colour is a Miro colour as the canvas takes it: "#rrggbb", or with an alpha
// ("#rrggbbaa"), whose 00 is no colour at all.
func colour(c, def string) string {
	c = strings.TrimSpace(c)
	switch {
	case c == "":
		return def
	case c == "transparent":
		return "transparent"
	case len(c) == 9 && c[0] == '#' && strings.EqualFold(c[7:], "00"):
		return "transparent"
	}
	return c
}

// blank is a fill that draws nothing worth a shape of its own.
func blank(fill string) bool {
	f := strings.ToLower(fill)
	return f == "transparent" || f == "#ffffff" || f == "#ffffffff" || f == "#fff"
}

func strokeStyle(s string) string {
	switch s {
	case "dotted", "dashed":
		return s
	}
	return "solid"
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func (c *converter) shape(it item, b box, eid string, upd int64) []scene.Element {
	var d shapeData
	var s shapeStyle
	_ = json.Unmarshal(it.Data, &d)
	_ = json.Unmarshal(it.Style, &s)
	kind, ok := shapes[d.Shape]
	if !ok {
		kind = shapeKind{kind: "rectangle", near: true}
	}
	if kind.near {
		c.note("SHAPE_NEAREST")
	}
	fill := colour(s.FillColor, "#ffffff")
	if s.FillOpacity != nil && *s.FillOpacity <= 0 {
		fill = "transparent"
	}
	stroke := colour(s.BorderColor, "#1a1a1a")
	if s.BorderOpacity != nil && *s.BorderOpacity <= 0 {
		stroke = "transparent"
	}
	width := 2.0
	if s.BorderWidth != nil {
		width = clamp(float64(*s.BorderWidth), 1, 16)
		if *s.BorderWidth <= 0 {
			stroke = "transparent"
		}
	}
	text, link := c.words(d.Content)
	size := 14.0
	if s.FontSize > 0 {
		size = clamp(float64(s.FontSize), 8, 288)
	}
	ts := scene.TextStyle{Size: size, Font: c.font(s.FontFamily), Colour: colour(s.Color, "#1a1a1a"),
		Align: align(s.TextAlign, "center"), VAlign: valign(s.TextAlignVertical, "middle")}

	if kind.kind != "line" {
		el := scene.Base(eid, kind.kind, b.x, b.y, b.w, b.h, upd)
		el["angle"] = b.angle
		el["backgroundColor"] = fill
		el["strokeColor"] = stroke
		el["strokeWidth"] = width
		el["strokeStyle"] = strokeStyle(s.BorderStyle)
		if kind.round {
			el["roundness"] = map[string]any{"type": 3}
		}
		if link != "" {
			el["link"] = link
		}
		el["customData"] = map[string]any{"source": "miro", "miroShape": d.Shape}
		if text == "" {
			return []scene.Element{el}
		}
		ts.Size = scene.FitSize(text, b.w, b.h, 8, ts.Size)
		return []scene.Element{el, scene.Label(eid+"-label", el, text, ts, 8, upd)}
	}

	// An outline: a line through its points, and its words (a line holds
	// none of its own) on it, grouped so they move together.
	points := make([]scene.Point, len(kind.outline))
	for i, p := range kind.outline {
		points[i] = scene.Point{X: b.x + p.X*b.w, Y: b.y + p.Y*b.h}
	}
	line := scene.Arrow{ID: eid, Points: points, Line: true, Colour: stroke, Width: width, Style: strokeStyle(s.BorderStyle)}
	if !kind.open {
		line.Fill = fill
	}
	el := line.Element(upd)
	el["angle"] = b.angle
	if link != "" {
		el["link"] = link
	}
	el["customData"] = map[string]any{"source": "miro", "miroShape": d.Shape}
	if text == "" {
		return []scene.Element{el}
	}
	group := eid + "-group"
	el["groupIds"] = []any{group}
	ts.Size = scene.FitSize(text, b.w, b.h, 12, ts.Size)
	lines := scene.Wrap(text, math.Max(b.w-24, 1), ts.Size)
	tw, th := scene.Measure(lines, ts.Size)
	tw = math.Min(tw, math.Max(b.w-24, 1))
	words := scene.Text(eid+"-text", b.x+(b.w-tw)/2, b.y+(b.h-th)/2, math.Max(b.w-24, 1), text, ts, upd)
	words["textAlign"] = "center"
	words["angle"] = b.angle
	words["groupIds"] = []any{group}
	return []scene.Element{el, words}
}

func (c *converter) text(it item, b box, eid string, upd int64) []scene.Element {
	var d textData
	var s textStyle
	_ = json.Unmarshal(it.Data, &d)
	_ = json.Unmarshal(it.Style, &s)
	text, link := c.words(d.Content)
	if text == "" {
		return nil
	}
	size := 14.0
	if s.FontSize > 0 {
		size = clamp(float64(s.FontSize), 8, 288)
	}
	a := align(s.TextAlign, "left")
	el := scene.Text(eid, b.x, b.y, b.w, text, scene.TextStyle{Size: size, Font: c.font(s.FontFamily), Colour: colour(s.Color, "#1a1a1a"), Align: a}, upd)
	// Its words sit in Miro's box as Miro aligned them.
	tw := el["width"].(float64)
	switch a {
	case "center":
		el["x"] = math.Round((b.x+(b.w-tw)/2)*100) / 100
	case "right":
		el["x"] = math.Round((b.x+b.w-tw)*100) / 100
	}
	el["angle"] = b.angle
	if link != "" {
		el["link"] = link
	}
	el["customData"] = map[string]any{"source": "miro"}
	if f := colour(s.FillColor, "transparent"); !blank(f) && (s.FillOpacity == nil || *s.FillOpacity > 0) {
		// A text box with a fill: the fill as a shape behind its words.
		bg := scene.Base(eid+"-fill", "rectangle", b.x, b.y, b.w, math.Max(b.h, el["height"].(float64)), upd)
		bg["backgroundColor"] = f
		bg["strokeColor"] = "transparent"
		bg["angle"] = b.angle
		group := eid + "-group"
		bg["groupIds"] = []any{group}
		el["groupIds"] = []any{group}
		return []scene.Element{bg, el}
	}
	return []scene.Element{el}
}

// frame is a frame and, when Miro filled it, the fill as a locked shape
// behind what is in it (an Excalidraw frame has no fill of its own). The
// converter draws the fill before the frame's children and the frame after.
func (c *converter) frame(it item, b box, eid string, upd int64) []scene.Element {
	var d frameData
	var s frameStyle
	_ = json.Unmarshal(it.Data, &d)
	_ = json.Unmarshal(it.Style, &s)
	name := strings.TrimSpace(d.Title)
	if name == "" {
		name = "Frame"
	}
	f := scene.Frame(eid, name, b.x, b.y, b.w, b.h, upd)
	f["customData"] = map[string]any{"source": "miro", "format": d.Format, "layout": d.Type}
	out := []scene.Element{f}
	if fill := colour(s.FillColor, "#ffffff"); !blank(fill) {
		bg := scene.Base(eid+"-fill", "rectangle", b.x, b.y, b.w, b.h, upd)
		bg["backgroundColor"] = fill
		bg["strokeColor"] = "transparent"
		bg["locked"] = true
		bg["frameId"] = eid
		bg["customData"] = map[string]any{"source": "miro", "kind": "frameFill"}
		out = append(out, bg)
	}
	return out
}

func (c *converter) image(it item, b box, eid string, upd int64) []scene.Element {
	var d imageData
	_ = json.Unmarshal(it.Data, &d)
	if strings.TrimSpace(d.ImageURL) == "" {
		c.missing["a picture Miro gave no address for"]++
		return scene.Placeholder(eid, b.x, b.y, b.w, b.h, "A picture that didn't come over", upd)
	}
	fileID := "miro-file-" + string(it.ID)
	el := scene.Image(eid, fileID, b.x, b.y, b.w, b.h, upd)
	el["angle"] = b.angle
	el["customData"] = map[string]any{"source": "miro"}
	name := strings.TrimSpace(d.Title)
	if name == "" {
		name = "picture-" + string(it.ID)
	}
	c.pictures = append(c.pictures, importProvider.BoardPicture{FileID: fileID, Attachment: importProvider.SourceAttachment{
		SourceID: string(it.ID), URL: d.ImageURL, Name: name,
		Parent: importProvider.SourceRef{Kind: "board", SourceID: string(it.ID)},
	}})
	return []scene.Element{el}
}

// card is a card on the board: a rounded box in its theme's colour with its
// words in it, top left.
func card(eid string, b box, upd int64, text, edge, fill, link string, data map[string]any) []scene.Element {
	el := scene.Base(eid, "rectangle", b.x, b.y, b.w, b.h, upd)
	el["angle"] = b.angle
	el["backgroundColor"] = fill
	el["strokeColor"] = edge
	el["strokeWidth"] = 2
	el["roundness"] = map[string]any{"type": 3}
	if link != "" {
		el["link"] = link
	}
	el["customData"] = data
	size := scene.FitSize(text, b.w, b.h, 12, 16)
	return []scene.Element{el, scene.Label(eid+"-label", el, text, scene.TextStyle{Size: size, Align: "left", VAlign: "top", Colour: "#1e1e1e"}, 12, upd)}
}

func (c *converter) card(it item, b box, eid string, upd int64) []scene.Element {
	var d cardData
	var s cardStyle
	_ = json.Unmarshal(it.Data, &d)
	_ = json.Unmarshal(it.Style, &s)
	title, link := c.words(d.Title)
	lines := []string{firstOf(title, "A card")}
	if desc, _ := c.words(d.Description); desc != "" {
		lines = append(lines, short(desc, 140))
	}
	data := map[string]any{"kind": "card", "source": "miro", "title": lines[0]}
	if due, err := time.Parse(time.RFC3339, d.DueDate); err == nil {
		lines = append(lines, "Due "+due.UTC().Format("2 Jan 2006"))
		data["dueDate"] = due.UTC().Format("2006-01-02")
	}
	return card(eid, b, upd, strings.Join(lines, "\n"), colour(s.CardTheme, "#2d9bf0"), "#ffffff", link, data)
}

func (c *converter) appCard(it item, b box, eid string, upd int64) []scene.Element {
	var d appCardData
	_ = json.Unmarshal(it.Data, &d)
	title, link := c.words(d.Title)
	lines := []string{firstOf(title, "An app card")}
	if desc, _ := c.words(d.Description); desc != "" {
		lines = append(lines, short(desc, 140))
	}
	for _, f := range d.Fields {
		if v := strings.TrimSpace(f.Value); v != "" {
			lines = append(lines, "• "+short(v, 60))
		}
	}
	return card(eid, b, upd, strings.Join(lines, "\n"), "#7B61D9", "#ffffff", link, map[string]any{"kind": "card", "source": "miro", "title": lines[0]})
}

// linkCard is an embed or a link's preview: a card naming it that opens it.
func (c *converter) linkCard(eid string, b box, upd int64, address, title string) []scene.Element {
	host := ""
	if u, err := url.Parse(strings.TrimSpace(address)); err == nil {
		host = strings.TrimPrefix(u.Hostname(), "www.")
	}
	if !safeLink(address) {
		address = ""
	}
	text := firstOf(title, host, "A link")
	if host != "" && text != host {
		text += "\n" + host
	}
	return card(eid, b, upd, text, "#3B7DDD", "#ffffff", address, map[string]any{"kind": "link", "source": "miro"})
}

func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}

// groups puts each group's elements in an Excalidraw group, under any group
// an element is already in (groupIds go from the deepest out).
func (c *converter) groups() {
	ids := make([]string, 0, len(c.src.groups))
	for gid := range c.src.groups {
		ids = append(ids, gid)
	}
	sort.Strings(ids)
	for _, gid := range ids {
		name := "miro-group-" + gid
		for _, itemID := range c.src.groups[gid] {
			for _, el := range c.own[itemID] {
				if el["type"] == "frame" {
					continue
				}
				list, _ := el["groupIds"].([]any)
				el["groupIds"] = append(append([]any{}, list...), name)
			}
		}
	}
}

// connectors are the board's connectors as arrows bound to what they join,
// each routed as Miro routed it: straight, elbowed or curved.
func (c *converter) connectors() []scene.Element {
	var out []scene.Element
	for _, cn := range c.src.connectors {
		if cn.ID == "" {
			continue
		}
		if cn.IsSupported != nil && !*cn.IsSupported {
			c.note("CONNECTOR_NOT_READ")
			continue
		}
		if cn.StartItem == nil || cn.EndItem == nil {
			c.note("CONNECTOR_LEFT_OUT")
			continue
		}
		start, end := c.main[string(cn.StartItem.ID)], c.main[string(cn.EndItem.ID)]
		if start == nil || end == nil {
			c.note("CONNECTOR_LEFT_OUT")
			continue
		}
		sb, eb := c.boxes[string(cn.StartItem.ID)], c.boxes[string(cn.EndItem.ID)]
		s := attach(sb, cn.StartItem, eb.centre())
		e := attach(eb, cn.EndItem, sb.centre())
		var points []scene.Point
		curved := false
		switch cn.Shape {
		case "elbowed":
			if math.Abs(e.X-s.X) >= math.Abs(e.Y-s.Y) {
				mx := (s.X + e.X) / 2
				points = []scene.Point{s, {X: mx, Y: s.Y}, {X: mx, Y: e.Y}, e}
			} else {
				my := (s.Y + e.Y) / 2
				points = []scene.Point{s, {X: s.X, Y: my}, {X: e.X, Y: my}, e}
			}
		case "curved":
			dx, dy := e.X-s.X, e.Y-s.Y
			points = []scene.Point{s, {X: (s.X+e.X)/2 - dy*0.15, Y: (s.Y+e.Y)/2 + dx*0.15}, e}
			curved = true
		default:
			points = []scene.Point{s, e}
		}
		startCap, endCap, colourOf, width, style, textColour, fontSize := "none", "stealth", "#1a1a1a", 2.0, "solid", "#1a1a1a", 14.0
		if st := cn.Style; st != nil {
			if st.StartStrokeCap != "" {
				startCap = st.StartStrokeCap
			}
			if st.EndStrokeCap != "" {
				endCap = st.EndStrokeCap
			}
			colourOf = colour(st.StrokeColor, colourOf)
			if st.StrokeWidth > 0 {
				width = clamp(float64(st.StrokeWidth), 1, 16)
			}
			style = strokeStyle(st.StrokeStyle)
			textColour = colour(st.Color, textColour)
			if st.FontSize > 0 {
				fontSize = clamp(float64(st.FontSize), 8, 144)
			}
		}
		aid := elementID(cn.ID)
		arrow := scene.Arrow{
			ID: aid, Points: points, Colour: colourOf, Width: width, Style: style, Curved: curved,
			StartHead: c.cap(startCap), EndHead: c.cap(endCap),
			Start: binding(start), End: binding(end),
		}.Element(c.now)
		arrow["customData"] = map[string]any{"source": "miro"}
		scene.Bind(arrow, c.elByID)
		els := []scene.Element{arrow}
		mid := midpoint(points)
		for i, cap := range cn.Captions {
			text, _ := c.words(cap.Content)
			if text == "" {
				continue
			}
			ts := scene.TextStyle{Size: fontSize, Colour: textColour}
			if i == 0 {
				// Its words on the arrow, at its middle, as Excalidraw places an arrow's label.
				els = append(els, scene.ArrowLabel(aid+"-label", arrow, text, ts, mid, c.now))
				continue
			}
			els = append(els, scene.Text(fmt.Sprintf("%s-caption-%d", aid, i), mid.X, mid.Y+float64(i)*fontSize*1.5, 0, text, ts, c.now))
		}
		out = append(out, els...)
	}
	return out
}

// bindable are the kinds of element an arrow can end on (Excalidraw's
// bindable elements); a shape drawn as a line can't hold an arrow's end, so
// one ending there starts or stops at its edge, unbound.
var bindable = map[any]bool{"rectangle": true, "ellipse": true, "diamond": true, "text": true, "image": true, "frame": true}

func binding(el scene.Element) *scene.Binding {
	if !bindable[el["type"]] {
		return nil
	}
	return &scene.Binding{ElementID: el["id"].(string), Gap: 4}
}

// midpoint is the middle of a path: its middle point, or the middle of its
// middle segment, as Excalidraw places an arrow's label.
func midpoint(points []scene.Point) scene.Point {
	n := len(points)
	if n%2 == 1 {
		return points[n/2]
	}
	a, b := points[n/2-1], points[n/2]
	return scene.Point{X: (a.X + b.X) / 2, Y: (a.Y + b.Y) / 2}
}

func (c *converter) cap(name string) string {
	m, ok := caps[name]
	if !ok {
		c.note("CAP_NEAREST")
		return "arrow"
	}
	if m.near {
		c.note("CAP_NEAREST")
	}
	return m.head
}

// attach is where a connector meets an item: the place Miro gives (a share of
// the item's width and height), or where the line from the other end's
// centre crosses the item's edge.
func attach(b box, e *endpoint, toward scene.Point) scene.Point {
	if e.Position != nil {
		return scene.Point{X: b.x + b.w*float64(e.Position.X)/100, Y: b.y + b.h*float64(e.Position.Y)/100}
	}
	c := b.centre()
	dx, dy := toward.X-c.X, toward.Y-c.Y
	if dx == 0 && dy == 0 {
		return c
	}
	tx, ty := math.Inf(1), math.Inf(1)
	if dx != 0 {
		tx = (b.w / 2) / math.Abs(dx)
	}
	if dy != 0 {
		ty = (b.h / 2) / math.Abs(dy)
	}
	t := math.Min(tx, ty)
	return scene.Point{X: c.X + dx*t, Y: c.Y + dy*t}
}

// report is what the board's report says: each kind of approximation once,
// with how many it was about, and what couldn't come over, by what it was.
func (c *converter) report() []importProvider.BoardNote {
	says := map[string]func(n int) string{
		"RICH_TEXT_PLAIN": func(n int) string {
			return count(n, "item", "items") + " had bold, italic, underlined or coloured words, which came over as plain words."
		},
		"FONT_NEAREST": func(n int) string {
			return count(n, "item", "items") + " used a font OneCamp boards don't have, and came over in the nearest one."
		},
		"SHAPE_NEAREST": func(n int) string {
			return count(n, "shape", "shapes") + " that Miro draws differently (clouds, cans and some flowchart shapes) came over as the nearest shape."
		},
		"NOTE_COLOUR_NEAREST": func(n int) string {
			return count(n, "sticky note", "sticky notes") + " came in a colour Miro's API didn't name, and came over yellow."
		},
		"CAP_NEAREST": func(n int) string {
			return count(n, "connector end", "connector ends") + " (some ERD ends) came over as the nearest arrowhead."
		},
		"DOCUMENT_AS_CARD": func(n int) string {
			return pick(n, "1 file on the board came over as a card naming it; the file stays in Miro.",
				fmt.Sprintf("%d files on the board came over as cards naming them; the files stay in Miro.", n))
		},
		"CONNECTOR_LEFT_OUT": func(n int) string {
			return pick(n, "1 connector joined something that didn't come over, so it was left out.",
				fmt.Sprintf("%d connectors joined something that didn't come over, so they were left out.", n))
		},
		"CONNECTOR_NOT_READ": func(n int) string {
			return pick(n, "1 connector Miro's API can't read was left out.", fmt.Sprintf("%d connectors Miro's API can't read were left out.", n))
		},
	}
	codes := make([]string, 0, len(c.counts))
	for code := range c.counts {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	var out []importProvider.BoardNote
	for _, code := range codes {
		out = append(out, importProvider.BoardNote{Code: code, Msg: says[code](c.counts[code]), Count: c.counts[code]})
	}
	if c.src.styleless > 0 {
		out = append(out, importProvider.BoardNote{Code: "STYLE_DEFAULT", Count: c.src.styleless,
			Msg: count(c.src.styleless, "item's colours", "items' colours") + " couldn't be read from Miro, so they came over in the default colours."})
	}
	whats := make([]string, 0, len(c.missing))
	for w := range c.missing {
		whats = append(whats, w)
	}
	sort.Strings(whats)
	for _, w := range whats {
		n := c.missing[w]
		out = append(out, importProvider.BoardNote{Code: "ITEM_NOT_BROUGHT", Count: n,
			Msg: fmt.Sprintf("Miro's API can't read %s, so a placeholder stands where each was (%d on this board).", w, n)})
	}
	return out
}

// count is n and its noun: "1 connector", "3 connectors". Pure.
func count(n int, one, many string) string {
	return pick(n, "1 "+one, fmt.Sprintf("%d %s", n, many))
}

// pick is one for 1, many for any other count. Pure.
func pick(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
