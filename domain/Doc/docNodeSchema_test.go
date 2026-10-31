package domain

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// docSchema is the graph schema the server alters at every start.
func docSchema(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("../../initializers/dgraphInit/connectDgraphDB.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

// The three predicates Docs & Files adds (F1, agreed with DGRAPH-LEAD-2 on 20
// Oct): doc_node_kind with an exact index, which the folder budget's count and
// the page-only reads filter on; doc_color and doc_link_url with none. None of
// them is @upsert (no write of them conflicts with another) or @count. All
// three are in type Doc too, so expand(_all_), a node's delete (S * *) and a
// purge take them with the rest of the doc.
func TestTheNodePredicatesAreDeclaredAsAgreed(t *testing.T) {
	schema := docSchema(t)
	want := map[string]string{
		"doc_node_kind": "string @index(exact)",
		"doc_color":     "string",
		"doc_link_url":  "string",
	}
	for pred, decl := range want {
		m := regexp.MustCompile(`(?m)^\s*` + pred + `\s*:\s*([^\n#]*?)\s*\.\s*$`).FindStringSubmatch(schema)
		if m == nil {
			t.Errorf("the schema doesn't declare %s", pred)
			continue
		}
		if m[1] != decl {
			t.Errorf("%s is declared %q, want %q", pred, m[1], decl)
		}
	}
	block := regexp.MustCompile(`(?s)type Doc \{(.*?)\}`).FindStringSubmatch(schema)
	if block == nil {
		t.Fatal("cannot find type Doc in the schema; it changed shape")
	}
	inType := map[string]bool{}
	for _, line := range strings.Split(block[1], "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), ":")
		inType[strings.TrimSpace(name)] = true
	}
	if !inType["doc_uuid"] || !inType["doc_host_kind"] {
		t.Fatal("read type Doc without doc_uuid or doc_host_kind; the parse no longer matches the schema")
	}
	for pred := range want {
		if !inType[pred] {
			t.Errorf("%s isn't in type Doc, so a doc's delete and purge would leave it behind", pred)
		}
	}
}

// A node's kind, colour and link are written under the agreed names, and a
// page (no kind) writes no doc_node_kind at all: nothing writes "".
func TestADocWritesItsKindColourAndLinkByTheirNames(t *testing.T) {
	raw, err := json.Marshal(&dgraphStruct.DgraphDoc{Uuid: "u", NodeKind: "link", Color: "sky", LinkURL: "https://example.com/a"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["doc_node_kind"] != "link" || got["doc_color"] != "sky" || got["doc_link_url"] != "https://example.com/a" {
		t.Errorf("a link wrote %s", raw)
	}
	page, _ := json.Marshal(&dgraphStruct.DgraphDoc{Uuid: "u"})
	for _, pred := range []string{"doc_node_kind", "doc_color", "doc_link_url"} {
		if strings.Contains(string(page), pred) {
			t.Errorf("a page wrote %s: %s", pred, page)
		}
	}
}
