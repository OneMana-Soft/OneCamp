package miro

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/business/Board/scene"
)

var update = flag.Bool("update", false, "write the golden board again")

// fixture reads testdata/board.json: what Miro's API answers for a board
// holding every kind of item the import meets, shaped as its spec says.
func fixture(t *testing.T) source {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "board.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Items      []item              `json:"items"`
		Connectors []connector         `json:"connectors"`
		Tags       map[string][]string `json:"tags"`
		Groups     map[string][]string `json:"groups"`
		Styleless  int                 `json:"styleless"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return source{items: f.Items, connectors: f.Connectors, tags: f.Tags, groups: f.Groups, styleless: f.Styleless}
}

const fixedNow = 1792000000000

func byID(els []scene.Element) map[string]scene.Element {
	out := map[string]scene.Element{}
	for _, el := range els {
		out[el["id"].(string)] = el
	}
	return out
}

// f reads a number an element holds.
func f(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return math.NaN()
}

// The board as drawn is held in testdata/board.golden.json; go test -run
// TestTheBoardIsDrawnAsBefore -update writes it again after a change that
// means to change it.
func TestTheBoardIsDrawnAsBefore(t *testing.T) {
	d := convert(fixture(t), fixedNow)
	got, err := json.MarshalIndent(map[string]any{"elements": d.elements, "pictures": d.pictures, "notes": d.notes}, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "board.golden.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (write it with -update)", err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Fatal("the board is drawn differently from testdata/board.golden.json; if that is meant, write it again with -update and read the difference")
	}
	// What it draws must be a state the collaboration service opens.
	if _, _, err := scene.State(1, d.elements, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPlacesAreTopLeftOnOnePlane(t *testing.T) {
	els := byID(convert(fixture(t), fixedNow).elements)
	// A note at the canvas's centre, 200 square.
	if n := els["miro-1001"]; f(n["x"]) != -100 || f(n["y"]) != -100 || f(n["width"]) != 200 {
		t.Fatalf("the centred note: %v, %v, %v", n["x"], n["y"], n["width"])
	}
	// A frame centred at 1000, 500, 800 by 600: its top left is 600, 200.
	fr := els["miro-1100"]
	if f(fr["x"]) != 600 || f(fr["y"]) != 200 || fr["type"] != "frame" || fr["name"] != "Retro" {
		t.Fatalf("the frame: %v", fr)
	}
	// Its note, centred at 100, 114 from the frame's top left: 700, 314; 200 by 228.
	child := els["miro-1101"]
	if f(child["x"]) != 600 || f(child["y"]) != 200 || f(child["height"]) != 228 || child["frameId"] != "miro-1100" {
		t.Fatalf("the frame's note: %v %v %v %v", child["x"], child["y"], child["height"], child["frameId"])
	}
	if els["miro-1101-label"]["frameId"] != "miro-1100" || els["miro-1102"]["frameId"] != "miro-1100" {
		t.Fatal("the frame's note's words and its picture are in the frame")
	}
	// Rotation in degrees is radians here.
	if a := f(els["miro-1002"]["angle"]); math.Abs(a-math.Pi/2) > 0.01 {
		t.Fatalf("the rotated note's angle is %v", a)
	}
	// A note given only its width is square.
	if n := els["miro-1003"]; f(n["height"]) != 200 {
		t.Fatalf("a note given its width alone is %v high", n["height"])
	}
}

func TestEachKindComesOverAsItsKind(t *testing.T) {
	d := convert(fixture(t), fixedNow)
	els := byID(d.elements)
	data := func(id string) map[string]any { m, _ := els[id]["customData"].(map[string]any); return m }

	note := data("miro-1001")
	if note["kind"] != "note" || note["colour"] != "cream" || note["source"] != "miro" {
		t.Fatalf("a light yellow note: %v", note)
	}
	if tags := note["tags"].([]any); len(tags) != 2 || tags[0] != "launch" || tags[1] != "q4" {
		t.Fatalf("its tags, kept as the sticky tool keeps them: %v", tags)
	}
	if els["miro-1001-label"]["text"] != "Ship the pricing\npage" && !strings.HasPrefix(els["miro-1001-label"]["originalText"].(string), "Ship the pricing page") {
		t.Fatalf("its words: %q", els["miro-1001-label"]["originalText"])
	}
	if els["miro-1001-label"]["verticalAlign"] != "top" || data("miro-1002")["colour"] != "navy" || data("miro-1003")["colour"] != "yellow" {
		t.Fatal("notes keep their alignment and colours; one Miro doesn't name is yellow")
	}
	if els["miro-1200"]["type"] != "diamond" || els["miro-1201"]["type"] != "rectangle" || els["miro-1201"]["roundness"] == nil ||
		els["miro-1201"]["strokeStyle"] != "dashed" || f(els["miro-1201"]["strokeWidth"]) != 3 || els["miro-1204"]["type"] != "ellipse" {
		t.Fatal("shapes come over as their own")
	}
	tri := els["miro-1202"]
	if tri["type"] != "line" || len(tri["points"].([]any)) != 4 || tri["backgroundColor"] != "#fce8ef" {
		t.Fatalf("a triangle is a filled closed line: %v", tri)
	}
	if words := els["miro-1202-text"]; words["text"] != "Up" || words["fontFamily"] != scene.FontCode {
		t.Fatalf("its words, in the code face: %v", words)
	}
	if els["miro-1203"]["backgroundColor"] != "transparent" {
		t.Fatal("a shape with no fill opacity is unfilled")
	}
	if groups := els["miro-1202"]["groupIds"].([]any); len(groups) != 2 || groups[1] != "miro-group-7001" {
		t.Fatalf("a grouped outline keeps its words' group and Miro's: %v", groups)
	}
	if els["miro-1300"]["text"] != "Launch plan\nQ4 & beyond" || els["miro-1300"]["textAlign"] != "center" || f(els["miro-1300"]["fontSize"]) != 36 {
		t.Fatalf("text: %v", els["miro-1300"])
	}
	if els["miro-1301"]["link"] != "https://onemana.dev/docs" || els["miro-1301-fill"]["backgroundColor"] != "#fff6d6" {
		t.Fatal("a text box keeps its link, and its fill behind it")
	}
	if img := els["miro-1102"]; img["type"] != "image" || img["fileId"] != "miro-file-1102" || len(d.pictures) != 1 ||
		d.pictures[0].FileID != "miro-file-1102" || d.pictures[0].Attachment.Name != "Mood board" {
		t.Fatalf("a picture: %v, %v", img, d.pictures)
	}
	if card := data("miro-1400"); card["kind"] != "card" || card["dueDate"] != "2026-11-03" || els["miro-1400"]["strokeColor"] != "#2d9bf0" {
		t.Fatalf("a card: %v", card)
	}
	if !strings.Contains(els["miro-1401-label"]["originalText"].(string), "• Sprint 5") {
		t.Fatal("an app card shows its fields")
	}
	if els["miro-1501"]["link"] != "https://www.youtube.com/watch?v=abc" || els["miro-1502"]["link"] != nil {
		t.Fatal("an embed opens its address; a preview of a script address opens nothing")
	}
	if data("miro-1600")["kind"] != "placeholder" || !strings.Contains(els["miro-1600-label"]["originalText"].(string), "a Miro doc") ||
		!strings.Contains(els["miro-1601-label"]["originalText"].(string), "a mind map node") {
		t.Fatal("what the API can't read stands as a placeholder saying what it was")
	}
	fill := els["miro-1100-fill"]
	if fill["backgroundColor"] != "#fff9b1" || fill["locked"] != true || fill["frameId"] != "miro-1100" {
		t.Fatalf("a frame's fill: %v", fill)
	}
}

func TestFramesComeAfterWhatIsInThem(t *testing.T) {
	els := convert(fixture(t), fixedNow).elements
	at := map[string]int{}
	for i, el := range els {
		at[el["id"].(string)] = i
		if el["index"] != scene.FractionalIndex(i) {
			t.Fatalf("element %d's index is %v", i, el["index"])
		}
	}
	if !(at["miro-1100-fill"] < at["miro-1101"] && at["miro-1101"] < at["miro-1102"] && at["miro-1102"] < at["miro-1100"]) {
		t.Fatalf("the frame's order: fill %d, note %d, picture %d, frame %d", at["miro-1100-fill"], at["miro-1101"], at["miro-1102"], at["miro-1100"])
	}
	if at["miro-2001"] < at["miro-1100"] {
		t.Fatal("connectors are drawn on top")
	}
}

func TestConnectorsAreBoundRoutedAndCapped(t *testing.T) {
	d := convert(fixture(t), fixedNow)
	els := byID(d.elements)
	a := els["miro-2001"]
	sb, eb := a["startBinding"].(map[string]any), a["endBinding"].(map[string]any)
	if sb["elementId"] != "miro-1001" || eb["elementId"] != "miro-1200" || a["endArrowhead"] != "arrow" || a["startArrowhead"] != nil {
		t.Fatalf("a straight connector: %v", a)
	}
	// From the note's bottom middle (0, 100) to the diamond's top middle (0, 340).
	if f(a["x"]) != 0 || f(a["y"]) != 100 || f(a["height"]) != 240 {
		t.Fatalf("its ends: %v, %v, %v", a["x"], a["y"], a["height"])
	}
	bound := func(id string) []any { l, _ := els[id]["boundElements"].([]any); return l }
	has := func(list []any, kind, id string) bool {
		for _, b := range list {
			if m := b.(map[string]any); m["type"] == kind && m["id"] == id {
				return true
			}
		}
		return false
	}
	if !has(bound("miro-1001"), "arrow", "miro-2001") || !has(bound("miro-1200"), "arrow", "miro-2001") || !has(bound("miro-2001"), "text", "miro-2001-label") {
		t.Fatal("both ends know the arrow, and the arrow its words")
	}
	if lbl := els["miro-2001-label"]; lbl["containerId"] != "miro-2001" || lbl["text"] != "yes" || els["miro-2001-caption-1"]["text"] != "maybe" {
		t.Fatal("its first caption is its label, the next free words by it")
	}
	elbow := els["miro-2002"]
	pts := elbow["points"].([]any)
	if len(pts) != 4 || elbow["strokeStyle"] != "dashed" || elbow["startArrowhead"] != "crowfoot_one" || elbow["endArrowhead"] != "crowfoot_many" {
		t.Fatalf("an elbowed connector: %v", elbow)
	}
	for i := 1; i < len(pts); i++ {
		p, q := pts[i-1].([]any), pts[i].([]any)
		if p[0] != q[0] && p[1] != q[1] {
			t.Fatalf("segment %d of the elbow isn't straight across or down: %v → %v", i, p, q)
		}
	}
	if curve := els["miro-2003"]; curve["roundness"] == nil || len(curve["points"].([]any)) != 3 || curve["endArrowhead"] != "arrow" {
		t.Fatalf("a curved connector, capped as Miro's default: %v", curve)
	}
	// A triangle is a line, which can't hold an arrow's end: it starts at its edge, unbound.
	if tri := els["miro-2004"]; tri["startBinding"] != nil || tri["endBinding"] == nil || tri["startArrowhead"] != "circle_outline" || tri["endArrowhead"] != "triangle" {
		t.Fatalf("a connector from an outline: %v", tri)
	}
	if els["miro-2005"] != nil || els["miro-2006"] != nil {
		t.Fatal("a connector to what didn't come over, and one the API can't read, are left out")
	}
}

func TestTheReportSaysWhatWasApproximatedAndWhatCouldNotCome(t *testing.T) {
	d := convert(fixture(t), fixedNow)
	got := map[string]int{}
	for _, n := range d.notes {
		got[n.Code] += n.Count
		if n.Msg == "" || strings.Contains(n.Msg, "%!") {
			t.Errorf("note %s says %q", n.Code, n.Msg)
		}
	}
	want := map[string]int{
		"RICH_TEXT_PLAIN": 2, "FONT_NEAREST": 3, "SHAPE_NEAREST": 1, "NOTE_COLOUR_NEAREST": 1, "CAP_NEAREST": 1,
		"DOCUMENT_AS_CARD": 1, "CONNECTOR_LEFT_OUT": 1, "CONNECTOR_NOT_READ": 1, "STYLE_DEFAULT": 1, "ITEM_NOT_BROUGHT": 2,
	}
	for code, n := range want {
		if got[code] != n {
			t.Errorf("%s: %d, want %d", code, got[code], n)
		}
	}
	for code := range got {
		if _, ok := want[code]; !ok {
			t.Errorf("an unexpected note %s", code)
		}
	}
}

// Every value Miro's spec names (REST v2, read 21 Oct 2026) has its own way
// over: only a value Miro adds later falls to a default.
func TestEveryValueMiroNamesIsMapped(t *testing.T) {
	for _, name := range []string{"gray", "light_yellow", "yellow", "orange", "light_green", "green", "dark_green", "cyan",
		"light_pink", "pink", "violet", "red", "light_blue", "blue", "dark_blue", "black"} {
		if _, ok := scene.NoteColourFromMiro(name); !ok {
			t.Errorf("sticky colour %s", name)
		}
	}
	for _, name := range []string{"rectangle", "round_rectangle", "circle", "triangle", "rhombus", "parallelogram", "trapezoid",
		"pentagon", "hexagon", "octagon", "wedge_round_rectangle_callout", "star", "flow_chart_predefined_process", "cloud",
		"cross", "can", "right_arrow", "left_arrow", "left_right_arrow", "left_brace", "right_brace"} {
		if _, ok := shapes[name]; !ok {
			t.Errorf("shape %s", name)
		}
	}
	for _, name := range []string{"none", "stealth", "rounded_stealth", "diamond", "filled_diamond", "oval", "filled_oval", "arrow",
		"triangle", "filled_triangle", "erd_one", "erd_many", "erd_only_one", "erd_zero_or_one", "erd_one_or_many", "erd_zero_or_many", "unknown"} {
		if _, ok := caps[name]; !ok {
			t.Errorf("cap %s", name)
		}
	}
	for _, name := range []string{"arial", "abril_fatface", "bangers", "eb_garamond", "georgia", "graduate", "gravitas_one",
		"fredoka_one", "nixie_one", "open_sans", "permanent_marker", "pt_sans", "pt_sans_narrow", "pt_serif", "rammetto_one",
		"roboto", "roboto_condensed", "roboto_slab", "caveat", "times_new_roman", "titan_one", "lemon_tuesday", "roboto_mono",
		"noto_sans", "plex_sans", "plex_serif", "plex_mono", "spoof", "tiempos_text", "formular"} {
		if _, ok := fonts[name]; !ok {
			t.Errorf("font %s", name)
		}
	}
	for name, s := range shapes {
		if s.kind == "line" && (len(s.outline) < 3 || (!s.open && s.outline[0] != s.outline[len(s.outline)-1])) {
			t.Errorf("shape %s's outline doesn't close", name)
		}
		for _, p := range s.outline {
			if p.X < 0 || p.X > 1 || p.Y < 0 || p.Y > 1 {
				t.Errorf("shape %s's outline leaves its box: %v", name, p)
			}
		}
	}
}

func TestHTMLIsReadAsLines(t *testing.T) {
	for _, c := range []struct {
		in, text, link string
		formatted      bool
	}{
		{"plain words", "plain words", "", false},
		{"<p>One</p><p>Two&nbsp;&amp; three</p>", "One\nTwo & three", "", false},
		{"<p>Line<br>break</p>", "Line\nbreak", "", false},
		{"<ul><li>a</li><li>b</li></ul>", "• a\n• b", "", false},
		{"<p><strong>Bold</strong> and <a href='https://x.dev/a?b=1&amp;c=2'>a link</a></p>", "Bold and a link", "https://x.dev/a?b=1&c=2", true},
		{`<p><a href="javascript:alert(1)">no</a></p>`, "no", "", false},
		{"<p></p><p>  spaced   out </p><p></p><p></p><p>end</p>", "spaced out\n\nend", "", false},
	} {
		text, link, formatted := plainText(c.in)
		if text != c.text || link != c.link || formatted != c.formatted {
			t.Errorf("%q: %q %q %v, want %q %q %v", c.in, text, link, formatted, c.text, c.link, c.formatted)
		}
	}
}
