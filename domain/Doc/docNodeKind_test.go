package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A node's kind is folder, file or link, and nothing else: a page has none,
// so "" and "page" are refused as kinds, as is anything spelled otherwise.
func TestOnlyFolderFileAndLinkAreKinds(t *testing.T) {
	for _, k := range []string{NodeFolder, NodeFile, NodeLink} {
		if err := CheckNodeKind(k); err != nil {
			t.Errorf("%q refused: %v", k, err)
		}
	}
	for _, k := range []string{"", "page", "Folder", "folders", "files", "url", " link"} {
		if err := CheckNodeKind(k); !errors.Is(err, ErrBadNodeKind) {
			t.Errorf("%q: %v, want ErrBadNodeKind", k, err)
		}
	}
	if !IsPage(&dgraphStruct.DgraphDoc{}) || IsPage(&dgraphStruct.DgraphDoc{NodeKind: NodeFolder}) || IsPage(nil) {
		t.Error("IsPage: a doc with no kind is a page, one with a kind isn't, and nil is neither")
	}
	if !HoldsChildren(&dgraphStruct.DgraphDoc{}) || !HoldsChildren(&dgraphStruct.DgraphDoc{NodeKind: NodeFolder}) ||
		HoldsChildren(&dgraphStruct.DgraphDoc{NodeKind: NodeFile}) || HoldsChildren(&dgraphStruct.DgraphDoc{NodeKind: NodeLink}) || HoldsChildren(nil) {
		t.Error("only a page or a folder holds children (D-30)")
	}
}

// A colour is one of the camp's six hues by name; ink is the default look,
// not a colour, and nothing else is one.
func TestAColourIsACampHue(t *testing.T) {
	for _, c := range helpers.CampHues {
		if err := CheckColor(c); err != nil {
			t.Errorf("%q refused: %v", c, err)
		}
	}
	for _, c := range []string{"", "ink", "red", "Sky", "#3B7DDD", "sky ", "gray"} {
		if err := CheckColor(c); !errors.Is(err, ErrBadColor) {
			t.Errorf("%q: %v, want ErrBadColor", c, err)
		}
	}
}

// A link is a public https address and nothing else: never http,
// javascript:, data:, a relative address, a password in it, an address
// inside the network, or one past the length a preview takes.
func TestALinkIsAPublicHTTPSAddress(t *testing.T) {
	ok := map[string]string{
		"https://docs.google.com/document/d/1AbC/edit":  "https://docs.google.com/document/d/1AbC/edit",
		"  https://onedrive.live.com/view.aspx?id=12  ": "https://onedrive.live.com/view.aspx?id=12",
		"HTTPS://Drive.Google.com/file/d/9#heading":     "https://drive.google.com/file/d/9#heading",
	}
	for in, want := range ok {
		got, err := CheckLinkURL(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "http://docs.google.com/a", "javascript:alert(1)", "data:text/html,<b>x</b>", "ftp://example.com/a",
		"//docs.google.com/a", "docs.google.com/a", "https://", "https://user:pw@example.com/a",
		"https://localhost/a", "https://127.0.0.1/a", "https://10.0.0.8/a", "https://db.internal/a",
		"https://example.com/" + strings.Repeat("a", 2048),
	} {
		if got, err := CheckLinkURL(in); !errors.Is(err, ErrBadLink) {
			t.Errorf("%q: %q %v, want ErrBadLink", in, got, err)
		}
	}
}

// A node is checked before anything is written: a bad kind, colour or link,
// or a link's address on a node that isn't a link, is refused by
// CreateDocNodes before it reaches the graph (no graph is connected here, so
// a write would fail otherwise).
func TestCreateDocNodesRefusesABadNodeBeforeWriting(t *testing.T) {
	at := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		doc  dgraphStruct.DgraphDoc
		want error
	}{
		"a page as a kind":        {dgraphStruct.DgraphDoc{NodeKind: "page"}, ErrBadNodeKind},
		"an unknown kind":         {dgraphStruct.DgraphDoc{NodeKind: "drive"}, ErrBadNodeKind},
		"a colour that isn't one": {dgraphStruct.DgraphDoc{NodeKind: NodeFolder, Color: "red"}, ErrBadColor},
		"a link with no address":  {dgraphStruct.DgraphDoc{NodeKind: NodeLink}, ErrBadLink},
		"a link over http":        {dgraphStruct.DgraphDoc{NodeKind: NodeLink, LinkURL: "http://example.com"}, ErrBadLink},
		"an address on a folder":  {dgraphStruct.DgraphDoc{NodeKind: NodeFolder, LinkURL: "https://example.com"}, ErrBadLink},
		"an address on a page":    {dgraphStruct.DgraphDoc{LinkURL: "https://example.com"}, ErrBadLink},
	} {
		doc := c.doc
		doc.Uuid = "5f0c4a2e-8a63-4f3e-9d2b-1c7a9e0b3d41"
		_, err := CreateDocNodes(context.Background(), []NewDocNode{{Doc: &doc, ParentRef: -1}}, nil, at)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

// Every decision read carries the node's kind and the decision says it, so a
// path only a page takes (the editor's session) refuses the rest with no
// other read; a row and the doc's page read its colour and link too; and the
// tree's leaf filter names exactly the kinds that hold nothing (D-30).
func TestADecisionAndARowSayWhatTheNodeIs(t *testing.T) {
	if got := DecideDoc(&dgraphStruct.DgraphDoc{Uuid: "u", NodeKind: NodeFolder}, Viewer{UID: "0x1"}); got.NodeKind != NodeFolder {
		t.Errorf("a folder's decision: %+v", got)
	}
	if got := DecideDoc(&dgraphStruct.DgraphDoc{Uuid: "u"}, Viewer{UID: "0x1"}); got.NodeKind != "" {
		t.Errorf("a page's decision names a kind: %+v", got)
	}
	if !strings.Contains(accessScalars, "doc_node_kind") || !strings.Contains(docRowFields, nodeLook) {
		t.Error("a decision's read or a row's doesn't carry the node's kind, colour and link")
	}
	for _, k := range []string{NodeFile, NodeLink} {
		if !strings.Contains(LeafFilter, `"`+k+`"`) {
			t.Errorf("the leaf filter %q leaves out %s", LeafFilter, k)
		}
	}
	if strings.Contains(LeafFilter, NodeFolder) {
		t.Errorf("the leaf filter %q takes a folder, which holds children", LeafFilter)
	}
}
