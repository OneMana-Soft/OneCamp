package businness

import (
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A copy of a page someone may only view is theirs alone: private, in no
// space, taking nobody's sharing, wherever it sits. Only a copy of a page they
// may edit goes beside it, as open as its new parent and in its space, and
// takes that parent's sharing; its subpages' copies follow it only where they
// may edit those too (the coordinator's ruling: a viewer's copy is private).
func TestAViewersCopyIsTheirsAlone(t *testing.T) {
	yes, no := true, false
	by := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "b0b00000-0000-4000-8000-000000000002"}
	// Q is open to the workspace and in project X's space; P is under it, and
	// S under P.
	q := &dgraphStruct.DgraphDoc{Uuid: "q", IsPrivate: &no, SpaceKind: "project", SpaceUUID: "x"}
	sources := map[string]*dgraphStruct.DgraphDoc{
		"p": {Uuid: "p", Title: "Plan", Position: "a0", Parent: &dgraphStruct.DgraphDoc{Uuid: "q"}, IsPrivate: &yes},
		"s": {Uuid: "s", Title: "Notes", Position: "b0", Parent: &dgraphStruct.DgraphDoc{Uuid: "p"}, IsPrivate: &yes},
	}
	order := []string{"p", "s"}
	now := time.Date(2026, 10, 14, 6, 0, 0, 0, time.UTC)
	private := func(doc *dgraphStruct.DgraphDoc) bool { return doc.IsPrivate != nil && *doc.IsPrivate }

	// A viewer of both: the caller puts the copy at the top of their pages.
	nodes, copies := planCopies(by, order, sources, map[string]bool{}, nil, "k0", now)
	if len(nodes) != 2 || copies[0].ParentUUID != "" || nodes[0].ParentUUID != "" || copies[1].ParentUUID != copies[0].UUID || nodes[1].ParentRef != 0 {
		t.Fatalf("a viewer's copies: %+v %+v", nodes, copies)
	}
	for i, n := range nodes {
		if !private(n.Doc) || n.Doc.SpaceKind != "" || n.Doc.SpaceUUID != "" || copies[i].Shared {
			t.Errorf("a viewer's copy %d is shared: %+v %+v", i, n.Doc, copies[i])
		}
		if n.Doc.Owner == nil || n.Doc.Owner.Uid != by.Uid {
			t.Errorf("copy %d isn't theirs: %+v", i, n.Doc.Owner)
		}
	}
	if nodes[0].Doc.Title != "Copy of Plan" || nodes[0].Doc.Position != "k0" || nodes[1].Doc.Title != "Notes" {
		t.Errorf("titles and places: %q at %q, %q", nodes[0].Doc.Title, nodes[0].Doc.Position, nodes[1].Doc.Title)
	}

	// An editor of P who may only view S: P's copy goes beside P, as open as
	// Q and in its space; S's copy under it is theirs alone.
	nodes, copies = planCopies(by, order, sources, map[string]bool{"p": true}, q, "k1", now)
	if c, d := copies[0], nodes[0].Doc; !c.Shared || c.ParentUUID != "q" || nodes[0].ParentUUID != "q" || private(d) || d.SpaceKind != "project" || d.SpaceUUID != "x" {
		t.Errorf("an editor's copy beside the page: %+v %+v", c, d)
	}
	if c, d := copies[1], nodes[1].Doc; c.Shared || c.ParentUUID != copies[0].UUID || !private(d) || d.SpaceKind != "" {
		t.Errorf("a viewer's copy under it: %+v %+v", c, d)
	}

	// An editor of both: S's copy follows its parent copy's space, and takes
	// the same sharing after (the caller gives it to every shared copy at once).
	nodes, copies = planCopies(by, order, sources, map[string]bool{"p": true, "s": true}, q, "k2", now)
	if c, d := copies[1], nodes[1].Doc; !c.Shared || d.SpaceKind != "project" || d.SpaceUUID != "x" || !private(d) {
		t.Errorf("an editor's copy under it: %+v %+v", c, d)
	}

	// An editor of S under a P they may only view: P's copy is theirs alone,
	// and so is S's under it, which takes no sharing from anywhere.
	nodes, copies = planCopies(by, order, sources, map[string]bool{"s": true}, nil, "k4", now)
	for i, c := range copies {
		if d := nodes[i].Doc; c.Shared || !private(d) || d.SpaceKind != "" {
			t.Errorf("copy %d under a viewer's copy: %+v %+v", i, c, d)
		}
	}
	// Nor is a copy beside nothing shared: at the top of their own pages.
	if _, copies = planCopies(by, order, sources, map[string]bool{"p": true, "s": true}, nil, "k5", now); copies[0].Shared || copies[1].Shared {
		t.Errorf("copies at the top of their own pages are shared: %+v", copies)
	}

	// A page whose parent isn't copied isn't either.
	nodes, _ = planCopies(by, []string{"p", "s"}, map[string]*dgraphStruct.DgraphDoc{"p": sources["p"], "s": {Uuid: "s", Parent: &dgraphStruct.DgraphDoc{Uuid: "elsewhere"}}}, map[string]bool{}, nil, "k3", now)
	if len(nodes) != 1 {
		t.Errorf("%d copies; S's parent wasn't copied", len(nodes))
	}
}

// A copy keeps what its source is (docs F1): a folder's copy is a folder in
// the same colour, a link's points where it did, and a file's copy is a file,
// whose newest version the duplicate copies apart (fileCopies).
func TestACopyKeepsTheNodesKindColourAndLink(t *testing.T) {
	by := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "b0b00000-0000-4000-8000-000000000002"}
	f, x, l := "1f000000-0000-4000-8000-000000000001", "1f000000-0000-4000-8000-000000000002", "1f000000-0000-4000-8000-000000000003"
	sources := map[string]*dgraphStruct.DgraphDoc{
		f: {Uuid: f, Title: "Contracts", NodeKind: "folder", Color: "moss"},
		x: {Uuid: x, Title: "msa.pdf", NodeKind: "file", Parent: &dgraphStruct.DgraphDoc{Uuid: f}},
		l: {Uuid: l, Title: "Roadmap", NodeKind: "link", LinkURL: "https://docs.google.com/d/1", Parent: &dgraphStruct.DgraphDoc{Uuid: f}},
	}
	nodes, copies := planCopies(by, []string{f, x, l}, sources, map[string]bool{}, nil, "k0", time.Now())
	if len(nodes) != 3 {
		t.Fatalf("copies: %+v", nodes)
	}
	if d := nodes[0].Doc; d.NodeKind != "folder" || d.Color != "moss" || d.Title != "Copy of Contracts" {
		t.Errorf("the folder's copy: %+v", d)
	}
	if d := nodes[1].Doc; d.NodeKind != "file" || d.Title != "msa.pdf" {
		t.Errorf("the file's copy: %+v", d)
	}
	if d := nodes[2].Doc; d.NodeKind != "link" || d.LinkURL != "https://docs.google.com/d/1" {
		t.Errorf("the link's copy: %+v", d)
	}
	got := fileCopies(nodes, copies)
	if len(got) != 1 || got[0].From.String() != x || got[0].To.String() != nodes[1].Doc.Uuid || copies[1].SourceUUID != x {
		t.Errorf("the files a duplicate copies the versions of: %+v", got)
	}
}

// Only a page's copy has its source's body seeded into it. A folder, a file or
// a link holds no body (docs F1): the collaboration service opens pages only,
// and a seed it can't take falls back to writing the body into the node itself.
func TestOnlyAPagesCopyIsSeeded(t *testing.T) {
	const body = "<p>Ship it.</p>"
	for _, c := range []struct {
		kind, body string
		want       bool
	}{
		{"", body, true},
		{"", "", false},
		{NodeFolder, body, false},
		{NodeLink, body, false},
		{domain.NodeFile, body, false},
	} {
		if got := seedsCopy(&dgraphStruct.DgraphDoc{NodeKind: c.kind}, c.body); got != c.want {
			t.Errorf("a copy of kind %q with body %q is seeded: %v, want %v", c.kind, c.body, got, c.want)
		}
	}
}
