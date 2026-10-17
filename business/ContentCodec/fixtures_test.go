package contentcodec

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "write the fixtures' HTML and Markdown from their JSON")

const fixtureDir = "../../testdata/codec"

type fixtureIndex struct {
	AppURL    string `json:"appURL"`
	RoundTrip []struct {
		Name   string `json:"name"`
		Schema string `json:"schema"`
	} `json:"roundTrip"`
	Import []struct {
		Name    string `json:"name"`
		From    string `json:"from"`
		Foreign bool   `json:"foreign"`
		Schema  string `json:"schema"`
	} `json:"import"`
}

func readIndex(t *testing.T) fixtureIndex {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx fixtureIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	return idx
}

func schemaNamed(name string) *Schema {
	if name == "everything" {
		return everything()
	}
	return shipped()
}

func canon(t *testing.T, n *Node) []byte {
	t.Helper()
	b, err := CanonicalJSON(n)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// golden compares got with a fixture file, or writes it under -update. An
// empty want with no file is a match (the file need not exist).
func golden(t *testing.T, path string, got []byte, optional bool) {
	t.Helper()
	if *update {
		if optional && got == nil {
			_ = os.Remove(path)
			return
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if optional && got == nil {
		if err == nil {
			t.Errorf("%s exists, but nothing is lost on the way through Markdown: delete it", filepath.Base(path))
		}
		return
	}
	if err != nil {
		t.Fatalf("%s: %v", filepath.Base(path), err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs.\n--- got\n%s\n--- want\n%s", filepath.Base(path), got, want)
	}
}

func TestFixturesRoundTrip(t *testing.T) {
	idx := readIndex(t)
	opts := MarkdownOptions{AppURL: idx.AppURL}
	for _, f := range idx.RoundTrip {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			schema := schemaNamed(f.Schema)
			base := filepath.Join(fixtureDir, f.Name)
			raw, err := os.ReadFile(base + ".json")
			if err != nil {
				t.Fatal(err)
			}
			doc, err := parseJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			// The fixture is canonical already.
			c := canon(t, Normalize(doc, schema))
			if !bytes.Equal(c, raw) {
				t.Fatalf("%s.json is not canonical; canonical:\n%s", f.Name, c)
			}
			doc = Normalize(doc, schema)

			// JSON → HTML → JSON.
			html := ToHTML(doc)
			golden(t, base+".html", []byte(html+"\n"), false)
			back, err := FromHTML(html, HTMLOptions{Schema: schema})
			if err != nil {
				t.Fatal(err)
			}
			if got := canon(t, back); !bytes.Equal(got, raw) {
				t.Errorf("HTML read back as a different doc:\n%s", got)
			}

			// JSON → Markdown → JSON → Markdown.
			md := ToMarkdown(doc, opts)
			golden(t, base+".md", []byte(md), false)
			fromMD, err := FromMarkdown(md, schema)
			if err != nil {
				t.Fatal(err)
			}
			got := canon(t, fromMD)
			var lossy []byte
			if !bytes.Equal(got, raw) {
				lossy = got
			}
			golden(t, base+".md.json", lossy, true)
			if again := ToMarkdown(fromMD, opts); again != md {
				t.Errorf("Markdown read and written again changed:\n--- got\n%s\n--- want\n%s", again, md)
			}
			// What Markdown reads back as is itself a doc HTML keeps.
			if h2, err := FromHTML(ToHTML(fromMD), HTMLOptions{Schema: schema}); err != nil || !bytes.Equal(canon(t, h2), got) {
				t.Errorf("the Markdown reading does not survive HTML")
			}
		})
	}
}

func TestFixturesImport(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Import []struct {
			Name    string `json:"name"`
			File    string `json:"file"`
			From    string `json:"from"`
			Foreign bool   `json:"foreign"`
			Schema  string `json:"schema"`
		} `json:"import"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	for _, f := range idx.Import {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			file := f.File
			if file == "" {
				file = f.Name
			}
			ext := ".md"
			if f.From == "html" {
				ext = ".html"
			}
			in, err := os.ReadFile(filepath.Join(fixtureDir, "import", file+ext))
			if err != nil {
				t.Fatal(err)
			}
			schema := schemaNamed(f.Schema)
			var doc *Node
			if f.From == "html" {
				doc, err = FromHTML(string(in), HTMLOptions{Foreign: f.Foreign, Schema: schema})
			} else {
				doc, err = FromMarkdown(string(in), schema)
			}
			if err != nil {
				t.Fatal(err)
			}
			golden(t, filepath.Join(fixtureDir, "import", f.Name+".json"), canon(t, doc), false)
		})
	}
}

// parseJSON reads a fixture's JSON.
func parseJSON(raw []byte) (*Node, error) {
	var n Node
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, err
	}
	return &n, nil
}
