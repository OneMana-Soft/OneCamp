package scene

// Sticky notes, as the board's sticky tool makes them (lib/board/notes.ts in
// the web app): a filled rectangle marked customData.kind "note", with its
// words bound inside it. The sixteen colours are the web app's, each keeping
// one of Miro's sixteen, so a Miro note comes over as itself; a test holds the
// two tables to each other when ONECAMP_FE_DIR names the web app.

import "strings"

// NoteColour is one of the board's note colours: its id (customData.colour),
// paper, edge and ink, and the Miro sticky colour it keeps.
type NoteColour struct {
	ID, Fill, Edge, Ink, Miro string
}

// NoteColours are the sixteen, the camp six first (lib/board/notes.ts
// NOTE_COLOURS, in its order).
var NoteColours = []NoteColour{
	{"yellow", "#FFF6D6", "#B98200", "#7A5200", "yellow"},
	{"green", "#E6F6EC", "#2F9E5B", "#1D6A3C", "green"},
	{"red", "#FCE8EF", "#D9467C", "#962556", "pink"},
	{"blue", "#E7EFFC", "#3B7DDD", "#1E4E9A", "blue"},
	{"purple", "#EFEBFC", "#7B61D9", "#4B3699", "violet"},
	{"grey", "#F5F6F7", "#D3D5DA", "#14161A", "gray"},
	{"teal", "#E1F5F6", "#0E97A6", "#0B5C65", "cyan"},
	{"cream", "#FFFBEA", "#D9B54A", "#6B5310", "light_yellow"},
	{"orange", "#FFEBD9", "#E07B24", "#8A4310", "orange"},
	{"mint", "#EEFBE5", "#6CBF43", "#36651C", "light_green"},
	{"forest", "#2F6B4A", "#2F6B4A", "#FFFFFF", "dark_green"},
	{"blush", "#FDF0F4", "#E89BB4", "#8A3553", "light_pink"},
	{"tomato", "#FDE4E1", "#E5483B", "#A3271D", "red"},
	{"ice", "#EAF6FF", "#6BB3E8", "#1F5A85", "light_blue"},
	{"navy", "#233F7A", "#233F7A", "#FFFFFF", "dark_blue"},
	{"charcoal", "#1F2328", "#1F2328", "#FFFFFF", "black"},
}

// NoteColourByID is the note colour with id, and whether there is one.
func NoteColourByID(id string) (NoteColour, bool) {
	for _, c := range NoteColours {
		if c.ID == id {
			return c, true
		}
	}
	return NoteColour{}, false
}

// NoteColourFromMiro is the note colour that keeps a Miro sticky's colour
// (StickyNoteStyle.fillColor), and whether Miro's name was one of its sixteen;
// yellow for one it doesn't name, as the web app's noteColourFromMiro.
func NoteColourFromMiro(name string) (NoteColour, bool) {
	for _, c := range NoteColours {
		if c.Miro == name {
			return c, true
		}
	}
	return NoteColours[0], false
}

// noteSizes are a note's three sides, and the size of its words on each
// (lib/board/notes.ts NOTE_SIZES, NOTE_FONT_SIZES).
var noteSizes = []struct{ side, font float64 }{{140, 16}, {180, 18}, {240, 24}}

// NoteFontSize is the size of a note's words for a note w wide: the size the
// web app gives the nearest of its three notes. Pure.
func NoteFontSize(w float64) float64 {
	best := noteSizes[1]
	for _, s := range noteSizes {
		if abs(s.side-w) < abs(best.side-w) {
			best = s
		}
	}
	return best.font
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// Note is one sticky note to draw.
type Note struct {
	ID         string
	X, Y, W, H float64
	Angle      float64 // radians
	Colour     NoteColour
	Text       string
	Tags       []string
	FrameID    string
	Align      string // left, center or right; "" centres
	VAlign     string // top, middle or bottom; "" middle
	Font       int    // 0: the clean sans
	Link       string
	// Data is more to keep in the note's customData (where it came from).
	Data map[string]any
}

// notePad is the room between a note's edge and its words.
const notePad = 10

// Elements are the note and its words, in that order: a clean hairline in
// its colour's edge round its paper (a note is paper, not a sketch of one),
// marked a note for the notes bar, voting and Make tasks, its words in its
// ink at the size the web app gives a note this wide, smaller when they
// wouldn't fit.
func (n Note) Elements(updated int64) []Element {
	rect := Base(n.ID, "rectangle", n.X, n.Y, n.W, n.H, updated)
	rect["backgroundColor"] = n.Colour.Fill
	rect["strokeColor"] = n.Colour.Edge
	rect["roundness"] = map[string]any{"type": 3}
	rect["angle"] = round2(n.Angle)
	data := map[string]any{}
	for k, v := range n.Data {
		data[k] = v
	}
	data["kind"] = "note"
	data["colour"] = n.Colour.ID
	if tags := NormaliseTags(n.Tags); len(tags) > 0 {
		list := make([]any, len(tags))
		for i, t := range tags {
			list[i] = t
		}
		data["tags"] = list
	}
	rect["customData"] = data
	if n.FrameID != "" {
		rect["frameId"] = n.FrameID
	}
	if n.Link != "" {
		rect["link"] = n.Link
	}
	size := FitSize(n.Text, n.W, n.H, notePad, NoteFontSize(n.W))
	label := Label(n.ID+"-label", rect, n.Text, TextStyle{Size: size, Font: n.Font, Colour: n.Colour.Ink, Align: n.Align, VAlign: n.VAlign}, notePad, updated)
	return []Element{rect, label}
}

// A note's tags, as the sticky tool keeps them (lib/board/sticky.ts
// normaliseTags): at most maxTags, each at most maxTagLength characters.
const (
	maxTags      = 8
	maxTagLength = 32
)

// NormaliseTags is tags as a note keeps them: trimmed, without a leading #,
// spaces run together, case kept, no repeats (by case). Pure.
func NormaliseTags(tags []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.Join(strings.Fields(strings.TrimLeft(strings.TrimSpace(t), "#")), " ")
		if r := []rune(t); len(r) > maxTagLength {
			t = strings.TrimSpace(string(r[:maxTagLength]))
		}
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
		if len(out) == maxTags {
			break
		}
	}
	return out
}
