package contentcodec

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// B2's rules (doc_format.go): old toggles and colours move over losslessly,
// and colours are names.

func TestAnOldToggleTitleBecomesItsFirstLine(t *testing.T) {
	line := func(s string) *Node {
		return &Node{Type: "paragraph", Content: []*Node{{Type: "text", Text: s}}}
	}
	doc := &Node{Type: "doc", Content: []*Node{
		{Type: "collapsible", Attrs: map[string]any{"title": "Plan & <dates>", "open": false}, Content: []*Node{line("Body.")}},
		// An old toggle with no title (only its open): its blocks stay its blocks.
		{Type: "collapsible", Attrs: map[string]any{"open": true}, Content: []*Node{line("Still the body.")}},
		{Type: "collapsible", Attrs: map[string]any{"blockColor": "sun_background"}, Content: []*Node{line("A summary."), line("Folded.")}},
	}}
	once := Normalize(doc, nil)
	want := `<div data-type="collapsible"><p>Plan &amp; &lt;dates&gt;</p><p>Body.</p></div>` +
		`<div data-type="collapsible"><p></p><p>Still the body.</p></div>` +
		`<div data-type="collapsible" data-block-color="sun_background"><p>A summary.</p><p>Folded.</p></div>`
	if got := ToHTML(once); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Moved over once: normalising again changes nothing.
	if a, b := canon(t, once), canon(t, Normalize(once, nil)); string(a) != string(b) {
		t.Fatalf("a second pass changed the doc:\n%s", b)
	}
	// Without the toggle in the schema, an old title is its first line, in bold.
	plain := Normalize(doc, &Schema{nodes: map[string]bool{"doc": true, "paragraph": true, "text": true}, marks: map[string]bool{"bold": true}})
	if got := ToHTML(plain); !strings.HasPrefix(got, `<p><strong>Plan &amp; &lt;dates&gt;</strong></p><p>Body.</p>`) {
		t.Fatalf("degraded: %s", got)
	}
}

func TestColoursAreNames(t *testing.T) {
	for in, want := range map[string]string{
		"sky": "sky", "var(--camp-sky-ink)": "sky", " VAR(--camp-berry-ink) ": "berry", "GRAY": "gray",
		"#ff0000": "", "red": "", "var(--camp-red-ink)": "", "skyblue": "", "var(--camp-sky-tint)": "", "": "",
	} {
		if got := colourName(in); got != want {
			t.Errorf("colourName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"sky": "sky", "sky_background": "sky_background", "gray_background": "gray_background",
		"sky_": "", "_background": "", "sky_background_x": "", "red_background": "", "sky_text": "", "Sky": "",
	} {
		if got := blockColourValue(in); got != want {
			t.Errorf("blockColourValue(%q) = %q, want %q", in, got, want)
		}
	}
	if got := highlightColour("berry"); got != "berry" {
		t.Errorf("highlightColour(berry) = %q", got)
	}
	if got := highlightColour("#ff0"); got != "sun" {
		t.Errorf("a highlight that names none of ours is yellow, got %q", got)
	}
	for in, want := range map[string]string{"rocket": "rocket", "triangle-alert": "triangle-alert", "Rocket": "", "<svg>": "", "a-b-c-d-e": "", "": ""} {
		if got := calloutIcon(in); got != want {
			t.Errorf("calloutIcon(%q) = %q, want %q", in, got, want)
		}
	}
}

// The collaboration service names the same colours and the same blocks that
// take one (its schema/colours.js), so a doc reads the same on both.
func TestTheCollaborationServiceHasTheSameColours(t *testing.T) {
	src, err := os.ReadFile("../../other-services/collaboration-service/schema/colours.js")
	if err != nil {
		t.Fatal(err)
	}
	list := func(name string) string {
		m := regexp.MustCompile(name + ` = \[([^\]]*)\]`).FindSubmatch(src)
		if m == nil {
			t.Fatalf("colours.js has no %s", name)
		}
		return strings.NewReplacer("'", "", `"`, "", " ", "").Replace(string(m[1]))
	}
	if got, want := list("DOC_COLOURS"), strings.Join(DocColours, ","); got != want {
		t.Errorf("DOC_COLOURS: the service has %s, the codec %s", got, want)
	}
	if got, want := list("BLOCK_COLOUR_NODES"), strings.Join(docFormatAttrs[0].on, ","); got != want {
		t.Errorf("BLOCK_COLOUR_NODES: the service has %s, the codec %s", got, want)
	}
}
