package email

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// steered is what a model could write when someone on the call steers it.
var steered = []string{`<a href="javascript:alert(1)">the deck</a>`, `<img src=x onerror=alert(2)>`, `<script>alert(3)</script>`, `javascript:alert(4)`}

// A recap's email holds every line, and every action item's words, owner and
// due date, as text: no link, image or script, no handler and no javascript:
// address of the model's in its HTML; its plain text says them as written.
func TestARecapMailHoldsWhatTheModelWroteAsText(t *testing.T) {
	d := RecapMail{TemplateData: TemplateData{RecipientName: "Gita", ActorName: "Sam", Title: "Recap of Launch sync", Why: "Sam sends the recap."}}
	for i, w := range steered {
		d.Lines = append(d.Lines, RecapLine{Text: w}, RecapLine{Text: w, Heading: true})
		d.Items = append(d.Items, RecapMailItem{Text: w, Owner: steered[(i+1)%len(steered)], Due: steered[(i+2)%len(steered)]})
	}
	got, text, err := RenderRecap(d)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(strings.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	var words strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "a", "img", "script", "iframe", "object", "embed", "form", "svg":
				t.Errorf("the recap's email holds a live <%s>", n.Data)
			}
			for _, a := range n.Attr {
				if strings.HasPrefix(strings.ToLower(a.Key), "on") || strings.Contains(strings.ToLower(a.Val), "javascript:") {
					t.Errorf("the recap's email holds %s=%q on <%s>", a.Key, a.Val, n.Data)
				}
			}
		}
		if n.Type == html.TextNode {
			words.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	for _, w := range steered {
		// A line, a heading, an item's words, an owner and a due date each.
		if n := strings.Count(words.String(), w); n != 5 {
			t.Errorf("the email shows %q as text %d times, want 5", w, n)
		}
		if n := strings.Count(text, w); n != 5 {
			t.Errorf("the plain text says %q %d times, want 5", w, n)
		}
	}
	if want := "• " + steered[0] + " (" + steered[1] + ", due " + steered[2] + ")"; !strings.Contains(words.String(), want) || !strings.Contains(text, want) {
		t.Errorf("an item reads other than %q", want)
	}
}

// The recap's body trusts none of its fields as HTML: its template calls
// neither of the shell's functions that mark a string safe, every field it
// writes is a plain string, and a Body given by mistake is never written.
func TestTheRecapMailTrustsNoneOfItsFields(t *testing.T) {
	body := recapTpl.Lookup("body")
	if body == nil || body.Tree == nil {
		t.Fatal("the recap's email has no body of its own")
	}
	if m := regexp.MustCompile(`(^|[^\w])(safehtml|html)\b`).FindString(body.Tree.Root.String()); m != "" {
		t.Errorf("the recap's body calls %q on what it writes", m)
	}
	plain := reflect.TypeOf("")
	for _, typ := range []reflect.Type{reflect.TypeOf(RecapLine{}), reflect.TypeOf(RecapMailItem{})} {
		for i := 0; i < typ.NumField(); i++ {
			if f := typ.Field(i); f.Type.Kind() == reflect.String && f.Type != plain {
				t.Errorf("%s.%s is a %s, not a plain string", typ.Name(), f.Name, f.Type)
			}
		}
	}
	got, text, err := RenderRecap(RecapMail{TemplateData: TemplateData{Title: "Recap", Body: "<b>raw</b>"}, Lines: []RecapLine{{Text: "We ship Friday."}}})
	if err != nil || strings.Contains(got+text, "raw") || !strings.Contains(got, "<div>We ship Friday.</div>") {
		t.Errorf("RenderRecap with a Body = %v, %q", err, got)
	}
}
