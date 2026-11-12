package contentcodec

import (
	"bytes"
	"testing"
)

// What admit.go keeps, by value (CR-221 M1): the pair table in preserved_test
// holds a marker to its tag twin; these hold each path to the doc it must read
// as, so a rule both paths lose together still fails here.
func TestAdmissionReadsAsTheseDocs(t *testing.T) {
	const png = "https://example.test/a.png"
	img := `{"type":"doc","content":[{"type":"image","attrs":{"src":"` + png + `"}}]}`
	words := func(s string) string {
		return `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"` + s + `"}]}]}`
	}
	cases := []struct {
		name, html string
		opts       HTMLOptions
		want       string
	}{
		{"an image's address is trimmed", `<img src="  ` + png + `  ">`, HTMLOptions{}, img},
		{"a kept image's address is trimmed", `<div data-oc-preserved="image" data-oc-holds="none" data-oc-attrs="{&quot;src&quot;:&quot;  ` + png + `  &quot;}"></div>`, HTMLOptions{}, img},
		{"a foreign image keeps its address only", `<img src="` + png + `" id="i1" caption="c" filename="f.png" data-align="center" data-crop="0,0,0.5,0.5" data-href="https://example.test">`, HTMLOptions{Foreign: true}, img},
		{"a foreign link keeps its address only", `<p><a href="https://example.test" target="_self" rel="opener" class="link">w</a></p>`, HTMLOptions{Foreign: true},
			`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"w","marks":[{"type":"link","attrs":{"href":"https://example.test"}}]}]}]}`},
		{"a foreign file with no address is its words", `<div data-type="file" data-name="a.pdf">a.pdf</div>`, HTMLOptions{Foreign: true}, words("a.pdf")},
		{"an import keeps a file's own path", `<div data-type="file" data-kind="pdf" data-src="files/a.pdf" data-name="a.pdf"></div>`, HTMLOptions{Foreign: true, Assets: true},
			`{"type":"doc","content":[{"type":"fileBlock","attrs":{"kind":"pdf","src":"files/a.pdf","name":"a.pdf"}}]}`},
		{"foreign HTML without its files loses the path, and the file", `<div data-type="file" data-kind="pdf" data-src="files/a.pdf" data-name="a.pdf">a.pdf</div>`, HTMLOptions{Foreign: true}, words("a.pdf")},
		{"the editor's own HTML loses the path, not the file", `<div data-type="file" data-kind="pdf" data-src="files/a.pdf" data-name="a.pdf"></div>`, HTMLOptions{},
			`{"type":"doc","content":[{"type":"fileBlock","attrs":{"kind":"pdf","name":"a.pdf"}}]}`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			doc, err := FromHTML(c.html, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := canon(t, doc), canonOf(t, c.want); !bytes.Equal(got, want) {
				t.Errorf("read as\n%s\nwant\n%s", got, want)
			}
		})
	}
	// Markdown's pictures pass the same admission: the address trimmed.
	doc, err := FromMarkdown("![](<  "+png+"  >)", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canon(t, doc), canonOf(t, img); !bytes.Equal(got, want) {
		t.Errorf("Markdown read the picture as\n%s\nwant\n%s", got, want)
	}
}
