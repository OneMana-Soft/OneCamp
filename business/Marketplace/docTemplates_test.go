package business

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/google/uuid"
)

func TestBuiltinDocTemplatesAreWholeAndStable(t *testing.T) {
	if len(BuiltinDocTemplates) != 10 {
		t.Fatalf("%d built-in doc templates, want 10", len(BuiltinDocTemplates))
	}
	seen := map[uuid.UUID]bool{}
	for i, b := range BuiltinDocTemplates {
		if seen[b.ID] {
			t.Errorf("%s: its id is used twice", b.Name)
		}
		seen[b.ID] = true
		if want := uuid.MustParse("b5d0c000-0000-4000-8000-" + strings.Repeat("0", 10) + []string{"01", "02", "03", "04", "05", "06", "07", "08", "09", "0a"}[i]); b.ID != want {
			t.Errorf("%s has id %s, want %s (the web asks for it by id)", b.Name, b.ID, want)
		}
		if BlankBody(b.Body) || !isCampHue(b.Hue) || b.Glyph == "" || b.Category == "" || b.Description == "" {
			t.Errorf("%s is missing something: %+v", b.Name, b)
		}
		if strings.Contains(b.Body, "data-block-id") {
			t.Errorf("%s carries block ids", b.Name)
		}
		for _, banned := range []string{"complian", "certified", "Untitled"} {
			if strings.Contains(strings.ToLower(b.Body+b.Description), strings.ToLower(banned)) {
				t.Errorf("%s says %q", b.Name, banned)
			}
		}
	}
}

func TestDocTemplateIsFoundByIDAndTheRestAreNot(t *testing.T) {
	p, err := DocTemplate(context.Background(), "b5d0c000-0000-4000-8000-000000000003")
	if err != nil || p.Title != "Decision record" || !strings.Contains(p.Body, "<h2>Decision</h2>") || !p.Builtin {
		t.Fatalf("got %+v, %v", p, err)
	}
	if _, err := DocTemplate(context.Background(), "meeting-notes"); err != docBusiness.ErrNoTemplate {
		t.Errorf("a name is not an id: %v", err)
	}
	body, err := docTemplates{}.Body(context.Background(), "b5d0c000-0000-4000-8000-000000000001")
	if err != nil || !strings.Contains(body, "Action items") {
		t.Errorf("the template source gives the body: %v", err)
	}
	if docBusiness.Templates == nil {
		t.Error("business/Doc's templates come from here")
	}
	if _, ok := docBusiness.Templates.(docTemplates); !ok {
		t.Error("business/Doc's templates are this package's")
	}
}

func TestBuiltinsAreListedWithAPreviewAndReadWithTheirBody(t *testing.T) {
	list := builtinsListed()
	var p DocPayload
	if err := json.Unmarshal([]byte(list[0].Payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.Body != "" || p.Preview == "" || !p.Builtin || list[0].AuthorName != "OneCamp" {
		t.Errorf("a listed template carries a preview, not its body: %+v", p)
	}
	full := builtinTemplate(BuiltinDocTemplates[0], true)
	var f DocPayload
	if err := json.Unmarshal([]byte(full.Payload), &f); err != nil || f.Body == "" || f.Preview != "" {
		t.Errorf("a template read on its own carries its body: %+v %v", f, err)
	}
}

func TestStripBlockIDs(t *testing.T) {
	got := StripBlockIDs(`<p data-block-id="0b5c">a</p><h2 class="x" data-block-id="9">b</h2>`)
	if got != `<p>a</p><h2 class="x">b</h2>` {
		t.Errorf("got %s", got)
	}
}

func TestDocPreviewEndsWhereABlockEnds(t *testing.T) {
	short := "<p>short</p>"
	if DocPreview(short) != short {
		t.Error("a short body is its own preview")
	}
	long := strings.Repeat("<p>Twelve words or so of a paragraph that goes on a while.</p>", 60)
	got := DocPreview(long)
	if len(got) > docPreviewBytes || !strings.HasSuffix(got, "</p>") || strings.Count(got, "<p>") != strings.Count(got, "</p>") {
		t.Errorf("a preview ends at a block: %d bytes, %q", len(got), got[len(got)-20:])
	}
}

func TestADocTemplatesPayloadIsChecked(t *testing.T) {
	if _, err := normalDocPayload(DocPayload{Title: "x", Body: "<p></p>"}, "id"); err != ErrEmptyTemplate {
		t.Errorf("an empty body is refused: %v", err)
	}
	big := "<p>" + strings.Repeat("a", maxPayloadBytes) + "</p>"
	if _, err := normalDocPayload(DocPayload{Title: "x", Body: big}, "id"); err != ErrTemplateTooLarge {
		t.Errorf("a body over the cap is refused: %v", err)
	}
	p, err := normalDocPayload(DocPayload{Title: " T ", Body: `<p data-block-id="1">hi</p>`, Builtin: true, Hue: "neon"}, "doc-1")
	if err != nil || p.Body != "<p>hi</p>" || p.Builtin || !isCampHue(p.Hue) || p.Glyph != defaultGlyph || p.Category != teamCategory || p.Title != "T" {
		t.Errorf("got %+v, %v", p, err)
	}
	if HueFor("doc-1") != HueFor("DOC-1") {
		t.Error("a hue is stable for an id")
	}
	if !validKind("doc") || !validKind("table") || validKind("page") {
		t.Error("doc is a kind beside the model's")
	}
}
