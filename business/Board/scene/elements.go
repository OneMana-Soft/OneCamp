package scene

// Excalidraw elements, as the board's canvas keeps them in its shared document.
// The canvas puts the document's elements on the scene as they are (lib/board/
// binding.ts in the web app: no restore pass fills in a missing field), so every
// element written here carries every field Excalidraw 0.18 reads for its kind.

import (
	"fmt"
	"hash/fnv"
	"math"
	"strings"
)

// Element is one Excalidraw element, as a map of its fields.
type Element = map[string]any

// Excalidraw's font families (0.18): Nunito is a board's clean sans (O4),
// Excalifont its hand-drawn one, Lilita One its display face and Comic Shanns
// its code face.
const (
	FontHand    = 5
	FontSans    = 6
	FontDisplay = 7
	FontCode    = 8
)

// Ink is the colour of words and lines that name no colour of their own.
const Ink = "#1e1e1e"

// lineHeight is Excalidraw's unitless line height for every font here.
const lineHeight = 1.25

// Seed is a stable number for an element, from its id. Pure.
func Seed(id string) int64 {
	h := fnv.New32a()
	h.Write([]byte(id))
	return int64(h.Sum32() & 0x7fffffff)
}

// FractionalIndex is the n-th of Excalidraw's element orders (rocicorp's
// fractional indexing, base 62), which sort as the elements are listed: "a0"
// … "az" for the first 62, then "b10" … "bzz", then "c100" … and "d1000" on,
// each head letter one digit longer. Pure.
func FractionalIndex(n int) string {
	const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	if n < 0 {
		n = 0
	}
	head := byte('a')
	width := 1
	limit := len(digits) // the values of this many digits
	for n >= limit && head < 'z' {
		head++
		width++
		limit *= len(digits)
	}
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		out[i] = digits[n%len(digits)]
		n /= len(digits)
	}
	return string(head) + string(out)
}

// Index gives the elements their draw order, as listed. Pure but for the maps.
func Index(elements []Element) {
	for i, el := range elements {
		el["index"] = FractionalIndex(i)
	}
}

// Base is an element of kind at x, y, w by h: clean (no hand-drawn wobble),
// in ink, unfilled, unrotated, first version, changed at updated (ms).
func Base(id, kind string, x, y, w, h float64, updated int64) Element {
	return Element{
		"id": id, "type": kind, "x": round2(x), "y": round2(y), "width": round2(math.Max(w, 0)), "height": round2(math.Max(h, 0)),
		"angle": 0, "strokeColor": Ink, "backgroundColor": "transparent",
		"fillStyle": "solid", "strokeWidth": 1, "strokeStyle": "solid",
		"roughness": 0, "opacity": 100, "groupIds": []any{}, "frameId": nil,
		"roundness": nil, "seed": Seed(id), "version": 1,
		"versionNonce": Seed(id + "#"), "isDeleted": false,
		"boundElements": nil, "updated": updated, "link": nil, "locked": false,
	}
}

// round2 keeps two decimals: a scene coordinate needs no more, and a whole
// number is written as one.
func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

// TextStyle is how words are set.
type TextStyle struct {
	Size   float64 // px; 0 means 20
	Font   int     // an Excalidraw font family; 0 means FontSans
	Colour string  // "" means Ink
	Align  string  // left, center or right; "" means center for a label, left for text
	VAlign string  // top, middle or bottom; "" means middle
}

func (s TextStyle) size() float64 {
	if s.Size <= 0 {
		return 20
	}
	return s.Size
}

func (s TextStyle) font() int {
	if s.Font == 0 {
		return FontSans
	}
	return s.Font
}

func (s TextStyle) colour() string {
	if s.Colour == "" {
		return Ink
	}
	return s.Colour
}

// charWidth is about how wide a character of the sans is at size px: 11 px at
// 18, as the demo's notes were measured.
func charWidth(size float64) float64 { return size * 11 / 18 }

// Wrap breaks text into lines that fit width at size px, each paragraph on
// its own, as Excalidraw wraps a label: it doesn't rewrap one it is handed. A
// word longer than a line is cut. Pure.
func Wrap(text string, width, size float64) []string {
	per := int(math.Floor(width / charWidth(size)))
	if per < 1 {
		per = 1
	}
	var lines []string
	for _, para := range strings.Split(text, "\n") {
		line := ""
		for _, w := range strings.Fields(para) {
			for len([]rune(w)) > per {
				r := []rune(w)
				if line != "" {
					lines = append(lines, line)
					line = ""
				}
				lines = append(lines, string(r[:per]))
				w = string(r[per:])
			}
			switch {
			case line == "":
				line = w
			case len([]rune(line))+1+len([]rune(w)) > per:
				lines = append(lines, line)
				line = w
			default:
				line += " " + w
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// Measure is the box lines of text take at size px. Pure.
func Measure(lines []string, size float64) (w, h float64) {
	widest := 0
	for _, l := range lines {
		widest = max(widest, len([]rune(l)))
	}
	return float64(widest) * charWidth(size), float64(max(len(lines), 1)) * size * lineHeight
}

// FitSize is the largest size, from want down to 10 px, at which text wraps
// inside a w by h box with pad on every side. Pure.
func FitSize(text string, w, h, pad, want float64) float64 {
	size := want
	for ; size > 10; size-- {
		lines := Wrap(text, w-2*pad, size)
		if _, th := Measure(lines, size); th <= h-2*pad {
			return size
		}
	}
	return 10
}

func textFields(el Element, text, original string, s TextStyle, align, valign string) {
	for k, v := range map[string]any{
		"text": text, "originalText": original, "fontSize": s.size(), "fontFamily": s.font(),
		"textAlign": align, "verticalAlign": valign, "containerId": nil,
		"autoResize": true, "lineHeight": lineHeight, "strokeColor": s.colour(),
	} {
		el[k] = v
	}
}

// Label is words bound inside a container (a note, a shape or an arrow),
// wrapped to it with pad on each side and placed as Excalidraw places a
// container's label.
func Label(id string, container Element, text string, s TextStyle, pad float64, updated int64) Element {
	cx, cy := num(container["x"]), num(container["y"])
	cw, ch := num(container["width"]), num(container["height"])
	align, valign := s.Align, s.VAlign
	if align == "" {
		align = "center"
	}
	if valign == "" {
		valign = "middle"
	}
	lines := Wrap(text, math.Max(cw-2*pad, 1), s.size())
	tw, th := Measure(lines, s.size())
	tw = math.Min(tw, math.Max(cw-2*pad, 1))
	x := cx + (cw-tw)/2
	switch align {
	case "left":
		x = cx + pad
	case "right":
		x = cx + cw - pad - tw
	}
	y := cy + (ch-th)/2
	switch valign {
	case "top":
		y = cy + pad
	case "bottom":
		y = cy + ch - pad - th
	}
	el := Base(id, "text", x, y, tw, th, updated)
	textFields(el, strings.Join(lines, "\n"), text, s, align, valign)
	el["containerId"] = container["id"]
	el["angle"] = container["angle"]
	el["frameId"] = container["frameId"]
	el["groupIds"] = container["groupIds"]
	container["boundElements"] = appendBound(container["boundElements"], "text", id)
	return el
}

// Text is free words at x, y, wrapped to width when it is over 0, else one
// line per paragraph.
func Text(id string, x, y, width float64, text string, s TextStyle, updated int64) Element {
	align := s.Align
	if align == "" {
		align = "left"
	}
	lines := strings.Split(text, "\n")
	if width > 0 {
		lines = Wrap(text, width, s.size())
	}
	tw, th := Measure(lines, s.size())
	el := Base(id, "text", x, y, tw, th, updated)
	textFields(el, strings.Join(lines, "\n"), text, s, align, "top")
	return el
}

// Frame is a named frame. Its children name it in their frameId, and come
// before it in the draw order, as Excalidraw keeps them.
func Frame(id, name string, x, y, w, h float64, updated int64) Element {
	el := Base(id, "frame", x, y, w, h, updated)
	el["name"] = name
	el["strokeColor"] = "#bbb"
	return el
}

// Point is a place on the scene.
type Point struct{ X, Y float64 }

// Binding ties an arrow's end to an element.
type Binding struct {
	ElementID string
	Focus     float64 // -1 to 1: where along the element the end aims
	Gap       float64 // px between the end and the element
}

// Arrow is an arrow (or, when Line is set, a line) through points on the
// scene, bound at either end when a Binding is given, with Excalidraw's
// arrowheads ("" for none).
type Arrow struct {
	ID                 string
	Points             []Point
	Start, End         *Binding
	StartHead, EndHead string
	Colour             string
	Width              float64
	Style              string // solid, dashed or dotted
	Curved             bool
	Line               bool
	Fill               string // a closed line's fill
}

// Element is the arrow on the scene: placed at its first point, its points
// relative to it.
func (a Arrow) Element(updated int64) Element {
	if len(a.Points) < 2 {
		a.Points = append(a.Points, Point{}, Point{})[:2]
	}
	x0, y0 := a.Points[0].X, a.Points[0].Y
	minX, minY, maxX, maxY := 0.0, 0.0, 0.0, 0.0
	pts := make([]any, 0, len(a.Points))
	for _, p := range a.Points {
		dx, dy := round2(p.X-x0), round2(p.Y-y0)
		minX, minY, maxX, maxY = math.Min(minX, dx), math.Min(minY, dy), math.Max(maxX, dx), math.Max(maxY, dy)
		pts = append(pts, []any{dx, dy})
	}
	kind := "arrow"
	if a.Line {
		kind = "line"
	}
	el := Base(a.ID, kind, x0, y0, maxX-minX, maxY-minY, updated)
	el["points"] = pts
	el["lastCommittedPoint"] = nil
	el["startBinding"] = binding(a.Start)
	el["endBinding"] = binding(a.End)
	el["startArrowhead"] = head(a.StartHead)
	el["endArrowhead"] = head(a.EndHead)
	if !a.Line {
		el["elbowed"] = false
	}
	if a.Colour != "" {
		el["strokeColor"] = a.Colour
	}
	if a.Width > 0 {
		el["strokeWidth"] = a.Width
	}
	if a.Style != "" {
		el["strokeStyle"] = a.Style
	}
	if a.Curved {
		el["roundness"] = map[string]any{"type": 2}
	}
	if a.Fill != "" {
		el["backgroundColor"] = a.Fill
	}
	return el
}

func binding(b *Binding) any {
	if b == nil || b.ElementID == "" {
		return nil
	}
	return map[string]any{"elementId": b.ElementID, "focus": round2(b.Focus), "gap": round2(b.Gap)}
}

func head(h string) any {
	if h == "" {
		return nil
	}
	return h
}

// Bind records on each bound element that the arrow ends at it, as Excalidraw
// keeps the two sides of a binding.
func Bind(arrow Element, byID map[string]Element) {
	for _, end := range []string{"startBinding", "endBinding"} {
		b, _ := arrow[end].(map[string]any)
		if b == nil {
			continue
		}
		if target := byID[fmt.Sprint(b["elementId"])]; target != nil {
			target["boundElements"] = appendBound(target["boundElements"], "arrow", fmt.Sprint(arrow["id"]))
		}
	}
}

func appendBound(list any, kind, id string) []any {
	out, _ := list.([]any)
	for _, b := range out {
		if m, ok := b.(map[string]any); ok && m["id"] == id {
			return out
		}
	}
	return append(out, map[string]any{"type": kind, "id": id})
}

// Image is a picture whose bytes are the board's file fileID.
func Image(id, fileID string, x, y, w, h float64, updated int64) Element {
	el := Base(id, "image", x, y, w, h, updated)
	el["strokeColor"] = "transparent"
	el["fileId"] = fileID
	el["status"] = "saved"
	el["scale"] = []any{1, 1}
	el["crop"] = nil
	return el
}

// LiveCount is how many of the elements a person sees. Pure.
func LiveCount(elements []Element) int {
	n := 0
	for _, el := range elements {
		if d, _ := el["isDeleted"].(bool); !d {
			n++
		}
	}
	return n
}

// num reads a number an element holds.
func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

// Placeholder stands where something couldn't come over: a dashed grey box of
// the same place and size, saying what it was, marked customData.kind
// "placeholder".
func Placeholder(id string, x, y, w, h float64, said string, updated int64) []Element {
	w, h = math.Max(w, 80), math.Max(h, 48)
	box := Base(id, "rectangle", x, y, w, h, updated)
	box["strokeColor"] = "#9a9da3"
	box["strokeStyle"] = "dashed"
	box["backgroundColor"] = "#f5f6f7"
	box["roundness"] = map[string]any{"type": 3}
	box["customData"] = map[string]any{"kind": "placeholder"}
	size := FitSize(said, w, h, 8, 16)
	return []Element{box, Label(id+"-label", box, said, TextStyle{Size: size, Colour: "#55585e"}, 8, updated)}
}

// ArrowLabel is words bound to an arrow, centred at mid (the middle of its
// path), wrapped as Excalidraw wraps an arrow's label: to 70% of the arrow's
// width, and never narrower than eleven characters' worth of its size.
func ArrowLabel(id string, arrow Element, text string, s TextStyle, mid Point, updated int64) Element {
	width := math.Max(0.7*num(arrow["width"]), 11*s.size())
	lines := Wrap(text, width, s.size())
	tw, th := Measure(lines, s.size())
	el := Base(id, "text", mid.X-tw/2, mid.Y-th/2, tw, th, updated)
	textFields(el, strings.Join(lines, "\n"), text, s, "center", "middle")
	el["containerId"] = arrow["id"]
	el["frameId"] = arrow["frameId"]
	el["groupIds"] = arrow["groupIds"]
	arrow["boundElements"] = appendBound(arrow["boundElements"], "text", id)
	return el
}
