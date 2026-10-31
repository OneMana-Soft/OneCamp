package scene

import (
	"encoding/base64"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestValuesRoundTrip(t *testing.T) {
	in := map[string]any{
		"s": "héllo — there", "t": true, "f": false, "n": nil, "small": 3, "neg": -70,
		"big": int64(1791614951123), "frac": 1.25, "arr": []any{1, "x", nil},
		"obj": map[string]any{"type": 3},
	}
	b, err := MapsUpdate(42, Root{Name: "elements", Entries: []Entry{{"a", in}, {"b", "two"}}})
	if err != nil {
		t.Fatal(err)
	}
	roots, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	got := roots["elements"]
	if len(roots) != 1 || got["b"] != "two" {
		t.Fatalf("roots %v, b %v", roots, got["b"])
	}
	m := got["a"].(map[string]any)
	if m["s"] != "héllo — there" || m["t"] != true || m["f"] != false || m["n"] != nil ||
		m["small"] != 3.0 || m["neg"] != -70.0 || m["big"] != 1791614951123.0 || m["frac"] != 1.25 ||
		len(m["arr"].([]any)) != 3 || m["obj"].(map[string]any)["type"] != 3.0 {
		t.Fatalf("round trip: %#v", m)
	}
	for _, bad := range []any{struct{}{}, math.NaN(), math.Inf(1), []string{"a typed slice"}} {
		if _, err := MapsUpdate(1, Root{Name: "elements", Entries: []Entry{{"bad", bad}}}); err == nil {
			t.Errorf("a value Yjs can't hold (%#v) was written", bad)
		}
	}
}

func TestDecodeRefusesWhatItDoesNotWrite(t *testing.T) {
	good, err := MapsUpdate(7, Root{Name: "elements", Entries: []Entry{{"a", map[string]any{"x": 1.5}}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(good); i++ {
		if _, err := Decode(good[:i]); err == nil {
			t.Fatalf("an update cut at byte %d of %d was read", i, len(good))
		}
	}
	if _, err := Decode(append(append([]byte{}, good...), 0)); err == nil {
		t.Fatal("bytes after the delete set were read")
	}
	two := append([]byte{2}, good[1:]...) // two clients
	if _, err := Decode(two); err == nil {
		t.Fatal("an update of two clients was read")
	}
}

// demoIndex is the demo's own order for its first 3,844 elements, before the
// order went past them; the shared order keeps it.
func demoIndex(n int) string {
	const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	if n < len(digits) {
		return "a" + string(digits[n])
	}
	return "b" + string(digits[(n/len(digits))%len(digits)]) + string(digits[n%len(digits)])
}

func TestFractionalIndexSortsPastTenThousand(t *testing.T) {
	prev := ""
	for i := 0; i < 300_000; i++ {
		ix := FractionalIndex(i)
		if ix <= prev {
			t.Fatalf("index %d (%s) is not after %s", i, ix, prev)
		}
		if i < 62*62 && ix != demoIndex(i) {
			t.Fatalf("index %d is %s, the demo's was %s", i, ix, demoIndex(i))
		}
		// Rocicorp's keys: the head letter says how many digits follow.
		if want := int(ix[0]-'a') + 1; len(ix)-1 != want {
			t.Fatalf("index %d (%s) has %d digits after %c, want %d", i, ix, len(ix)-1, ix[0], want)
		}
		prev = ix
	}
}

func TestWrapKeepsParagraphsAndCutsLongWords(t *testing.T) {
	// 160 px at 18 px: 14 characters a line, as the demo's notes wrap.
	got := Wrap("SSO worked first time for both pilots\n— Maya", 160, 18)
	want := []string{"SSO worked", "first time for", "both pilots", "— Maya"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wrapped %q, want %q", got, want)
	}
	if got := Wrap("Supercalifragilistic", 88, 18); strings.Join(got, "|") != "Supercal|ifragili|stic" {
		t.Fatalf("a long word: %q", got)
	}
	if got := Wrap("one\n\nthree", 300, 18); len(got) != 3 || got[1] != "" {
		t.Fatalf("a blank line is kept: %q", got)
	}
	if w, h := Measure([]string{"abc", "abcdef"}, 18); w != 66 || h != 45 {
		t.Fatalf("measured %v × %v, want 66 × 45", w, h)
	}
	if size := FitSize(strings.Repeat("word ", 60), 180, 180, 10, 18); size >= 18 || size < 10 {
		t.Fatalf("a long note's words stay %v px", size)
	}
	if size := FitSize("Short", 180, 180, 10, 18); size != 18 {
		t.Fatalf("a short note's words shrank to %v px", size)
	}
}

func TestANoteIsANoteWithItsWordsBoundInside(t *testing.T) {
	colour, ok := NoteColourFromMiro("light_blue")
	if !ok || colour.ID != "ice" {
		t.Fatalf("Miro's light_blue is %v", colour)
	}
	if c, ok := NoteColourFromMiro("magenta"); ok || c.ID != "yellow" {
		t.Fatalf("a colour Miro's notes don't have came as %v, %v", c, ok)
	}
	els := Note{ID: "n1", X: 10, Y: 20, W: 200, H: 228, Colour: colour, Text: "Book the load test a week earlier",
		Tags: []string{"launch", " "}, FrameID: "f1", Align: "left", VAlign: "top", Data: map[string]any{"source": "miro"}}.Elements(5)
	if len(els) != 2 {
		t.Fatalf("%d elements, want a note and its words", len(els))
	}
	rect, label := els[0], els[1]
	data := rect["customData"].(map[string]any)
	if rect["type"] != "rectangle" || data["kind"] != "note" || data["colour"] != "ice" || data["source"] != "miro" ||
		len(data["tags"].([]any)) != 1 || rect["backgroundColor"] != colour.Fill || rect["frameId"] != "f1" {
		t.Fatalf("the note: %v", rect)
	}
	bound := rect["boundElements"].([]any)
	if len(bound) != 1 || bound[0].(map[string]any)["id"] != "n1-label" {
		t.Fatalf("the note's bound words: %v", bound)
	}
	if label["containerId"] != "n1" || label["strokeColor"] != colour.Ink || label["frameId"] != "f1" ||
		label["originalText"] != "Book the load test a week earlier" || label["textAlign"] != "left" || label["verticalAlign"] != "top" {
		t.Fatalf("the words: %v", label)
	}
	if label["x"].(float64) != 20 || label["y"].(float64) != 30 {
		t.Fatalf("left and top words sit at the note's pad: %v, %v", label["x"], label["y"])
	}
	for _, line := range strings.Split(label["text"].(string), "\n") {
		if float64(len([]rune(line)))*charWidth(label["fontSize"].(float64)) > 180 {
			t.Fatalf("line %q runs off the note", line)
		}
	}
}

func TestAnArrowIsBoundAtBothEnds(t *testing.T) {
	a := Base("a", "rectangle", 0, 0, 100, 50, 1)
	b := Base("b", "rectangle", 300, 200, 100, 50, 1)
	arrow := Arrow{ID: "c", Points: []Point{{100, 25}, {200, 25}, {300, 225}}, Start: &Binding{ElementID: "a", Gap: 2},
		End: &Binding{ElementID: "b", Focus: 0.25, Gap: 2}, EndHead: "triangle", Style: "dashed", Width: 2}.Element(1)
	Bind(arrow, map[string]Element{"a": a, "b": b})
	pts := arrow["points"].([]any)
	if arrow["x"] != 100.0 || arrow["y"] != 25.0 || len(pts) != 3 || pts[2].([]any)[0] != 200.0 || pts[2].([]any)[1] != 200.0 {
		t.Fatalf("the arrow's place and points: %v %v %v", arrow["x"], arrow["y"], pts)
	}
	if arrow["width"] != 200.0 || arrow["height"] != 200.0 || arrow["elbowed"] != false || arrow["startArrowhead"] != nil || arrow["endArrowhead"] != "triangle" {
		t.Fatalf("the arrow: %v", arrow)
	}
	for _, el := range []Element{a, b} {
		bound, _ := el["boundElements"].([]any)
		if len(bound) != 1 || bound[0].(map[string]any)["type"] != "arrow" || bound[0].(map[string]any)["id"] != "c" {
			t.Fatalf("%s doesn't know the arrow ends at it: %v", el["id"], bound)
		}
	}
	Bind(arrow, map[string]Element{"a": a, "b": b})
	if bound := a["boundElements"].([]any); len(bound) != 1 {
		t.Fatalf("binding twice listed the arrow twice: %v", bound)
	}
	line := Arrow{ID: "l", Points: []Point{{0, 0}, {10, 0}, {0, 10}, {0, 0}}, Line: true, Fill: "#fff"}.Element(1)
	if line["type"] != "line" || line["backgroundColor"] != "#fff" {
		t.Fatalf("a closed line: %v", line)
	}
	if _, has := line["elbowed"]; has {
		t.Fatal("a line carries an arrow's elbowed")
	}
}

func TestAStateReadsBackWithItsFilesAndSettings(t *testing.T) {
	els := Note{ID: "n", W: 180, H: 180, Colour: NoteColours[0], Text: "Hi"}.Elements(1)
	img := Image("i", "file-1", 0, 300, 64, 64, 1)
	gone := Base("g", "ellipse", 0, 0, 1, 1, 1)
	gone["isDeleted"] = true
	els = append(els, img, gone)
	Index(els)
	state, live, err := State(9, els, []File{{ID: "file-1", ObjectUUID: "obj", MimeType: "image/png"}}, map[string]any{"style": "clean"})
	if err != nil {
		t.Fatal(err)
	}
	if live != 3 {
		t.Fatalf("%d seen, want 3 (the deleted one isn't)", live)
	}
	raw, err := base64.StdEncoding.DecodeString(state)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots["elements"]) != 4 || roots["boardMeta"]["style"] != "clean" {
		t.Fatalf("roots: %v", roots)
	}
	f := roots["files"]["file-1"].(map[string]any)
	if f["objectUuid"] != "obj" || f["mimeType"] != "image/png" || f["id"] != "file-1" {
		t.Fatalf("the file: %v", f)
	}
	if roots["elements"]["i"].(map[string]any)["fileId"] != "file-1" || roots["elements"]["n"].(map[string]any)["index"] != "a0" {
		t.Fatalf("the elements: %v", roots["elements"])
	}
	if _, _, err := State(9, append(els, Base("n", "rectangle", 0, 0, 1, 1, 1)), nil, nil); err == nil {
		t.Fatal("two elements with one id were saved")
	}
}

// The sixteen note colours are the web app's (lib/board/notes.ts
// NOTE_COLOURS), in its order, each keeping the same Miro colour: a note made
// on the board, from a template or brought from Miro is the same note.
func TestNoteColoursAreTheWebApps(t *testing.T) {
	fe := os.Getenv("ONECAMP_FE_DIR")
	if fe == "" {
		t.Skip("ONECAMP_FE_DIR names the web app to check against")
	}
	src, err := os.ReadFile(filepath.Join(fe, "lib/board/notes.ts"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s*(\w+): \{ label: "[^"]*", fill: "(#[0-9A-Fa-f]{6})", edge: "(#[0-9A-Fa-f]{6})", ink: "(#[0-9A-Fa-f]{6})", miro: "(\w+)" \},?$`)
	var web []NoteColour
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		web = append(web, NoteColour{ID: m[1], Fill: m[2], Edge: m[3], Ink: m[4], Miro: m[5]})
	}
	if len(web) != len(NoteColours) {
		t.Fatalf("the web app has %d note colours, this %d", len(web), len(NoteColours))
	}
	for i := range web {
		if web[i] != NoteColours[i] {
			t.Errorf("colour %d: web %v, here %v", i, web[i], NoteColours[i])
		}
	}
}
