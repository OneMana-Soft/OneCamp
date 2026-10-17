package contentcodec

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// The codec's copy of the schema manifest (embedded, so the server knows its
// editor's schema at run time) is the collaboration service's, byte for byte.
func TestManifestIsTheCollaborationServicesCopy(t *testing.T) {
	ours, err := os.ReadFile("schema-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := os.ReadFile("../../other-services/collaboration-service/schema-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ours, theirs) {
		t.Fatal("business/ContentCodec/schema-manifest.json differs from the collaboration service's: " +
			"copy other-services/collaboration-service/schema-manifest.json over it, and give any new " +
			"node or mark its rule in schema.go")
	}
}

// identityAttrs are the attributes the editor gives blocks for their identity,
// not their content: blockId, on every block (docs programme A1; the
// collaboration service's schema/blockId.js). The codec neither reads nor
// writes them: an export carries none, and an import's blocks get theirs
// where they land. The manifest lists them, so the check below leaves them out.
var identityAttrs = map[string]bool{"blockId": true}

// Every node and mark the editor writes has a rule here, with the same
// attributes: a block added to the editor without its codec rule fails.
func TestEveryTypeTheEditorWritesHasARule(t *testing.T) {
	m, err := shippedManifest()
	if err != nil {
		t.Fatal(err)
	}
	for name, typ := range m.Nodes {
		spec := nodeSpecs[name]
		if spec == nil {
			t.Errorf("node %q has no codec rule (business/ContentCodec/schema.go)", name)
			continue
		}
		var attrs []string
		for _, a := range spec.attrs {
			attrs = append(attrs, a.name)
		}
		// And the attributes another package puts on it (global_attrs.go).
		for _, g := range globalsOn(name) {
			attrs = append(attrs, g.name)
		}
		sort.Strings(attrs)
		var want []string
		for _, a := range typ.Attrs {
			if !identityAttrs[a] {
				want = append(want, a)
			}
		}
		sort.Strings(want)
		if len(attrs) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(attrs, want) {
			t.Errorf("node %q: the codec knows attributes %v, the editor writes %v", name, attrs, want)
		}
	}
	for name, typ := range m.Marks {
		spec := markSpecs[name]
		if spec == nil {
			t.Errorf("mark %q has no codec rule", name)
			continue
		}
		var attrs []string
		for _, a := range spec.attrs {
			attrs = append(attrs, a.name)
		}
		sort.Strings(attrs)
		want := append([]string{}, typ.Attrs...)
		sort.Strings(want)
		if len(attrs) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(attrs, want) {
			t.Errorf("mark %q: the codec knows attributes %v, the editor writes %v", name, attrs, want)
		}
	}
}

// The round-trip fixtures hold every node and mark the editor writes, so
// Markdown → JSON → Markdown and HTML → JSON → HTML are proven for each.
func TestFixturesCoverEveryType(t *testing.T) {
	m, err := shippedManifest()
	if err != nil {
		t.Fatal(err)
	}
	idx := readIndex(t)
	seenNodes, seenMarks := map[string]bool{}, map[string]bool{}
	var visit func(*Node)
	visit = func(n *Node) {
		seenNodes[n.Type] = true
		for _, mk := range n.Marks {
			seenMarks[mk.Type] = true
		}
		for _, c := range n.Content {
			visit(c)
		}
	}
	for _, f := range idx.RoundTrip {
		raw, err := os.ReadFile(filepath.Join(fixtureDir, f.Name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := parseJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		visit(doc)
	}
	for name := range m.Nodes {
		if !seenNodes[name] {
			t.Errorf("no round-trip fixture holds a %q node", name)
		}
	}
	for name := range m.Marks {
		if !seenMarks[name] {
			t.Errorf("no round-trip fixture holds a %q mark", name)
		}
	}
}
