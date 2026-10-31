//go:build integration
// +build integration

package integration_test

// The scans across docs take pages only (docs F1), on a real graph with the
// production schema: the doc repair's page, the
// retention's inactive docs, a doc's backlinks and the work graph's link
// reconciler all leave a folder, a file and a link out, and a save from the
// collaboration service writes nothing to a folder.
//
// Run: integ_rel.sh <wt> <log> '^TestScansAcrossDocsTakePagesOnly$'

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	domainDoc "github.com/akashc777/OneCamp/domain/Doc"
	workGraphDomain "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestScansAcrossDocsTakePagesOnly(t *testing.T) {
	ctx := context.Background()
	integration.SetupDgraph(t)
	live := time.Time{}
	long := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	node := func(name, kind, body string) *dgraphStruct.DgraphDoc {
		d := &dgraphStruct.DgraphDoc{Uid: "_:" + name, DType: []string{"Doc"}, Uuid: uuid.NewString(), Title: name,
			Body: body, NodeKind: kind, CreatedAt: &long, UpdatedAt: &long, DeletedAt: &live}
		if kind == domainDoc.NodeLink {
			d.LinkURL = "https://docs.google.com/document/d/1"
		}
		return d
	}
	target := node("target", "", "<p>linked to</p>")
	page := node("page", "", `<p>see <a href="/app/doc/x">it</a></p>`)
	folder := node("folder", domainDoc.NodeFolder, "")
	file := node("file", domainDoc.NodeFile, "")
	link := node("link", domainDoc.NodeLink, "")
	// A page links to the target; so, as no folder ever does, does the folder,
	// which the backlinks must still leave out.
	page.Mentions = []*dgraphStruct.DgraphDoc{{Uid: "_:target"}}
	folder.Mentions = []*dgraphStruct.DgraphDoc{{Uid: "_:target"}}
	file.Parent = &dgraphStruct.DgraphDoc{Uid: "_:folder"}
	if _, err := dgexec.Set(ctx, []*dgraphStruct.DgraphDoc{target, page, folder, file, link}); err != nil {
		t.Fatal(err)
	}
	pages := map[string]bool{target.Uuid: true, page.Uuid: true}
	others := map[string]string{folder.Uuid: "folder", file.Uuid: "file", link.Uuid: "link"}
	only := func(what string, got []string) {
		t.Helper()
		seen := map[string]bool{}
		for _, id := range got {
			if k, bad := others[id]; bad {
				t.Errorf("%s took the %s", what, k)
			}
			seen[id] = true
		}
		for id := range pages {
			if !seen[id] {
				t.Errorf("%s left out a page", what)
			}
		}
	}
	ids := func(docs []*dgraphStruct.DgraphDoc) []string {
		out := make([]string, 0, len(docs))
		for _, d := range docs {
			out = append(out, d.Uuid)
		}
		return out
	}

	bodies, err := domainDoc.GetDocBodyPage(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	only("the doc repair's page", ids(bodies))
	inactive, err := domainDoc.GetDocsInactiveSince(ctx, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), 1000)
	if err != nil {
		t.Fatal(err)
	}
	only("the retention's inactive docs", inactive)

	mentioners, err := domainDoc.GetDocMentioners(ctx, target.Uuid, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(mentioners); len(got) != 1 || got[0] != page.Uuid {
		t.Errorf("the target's backlinks: %v; want the page alone", got)
	}

	sources, err := workGraphDomain.ReadSources(ctx, workGraphDomain.KindDoc, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	var sourceIDs []string
	for _, s := range sources {
		sourceIDs = append(sourceIDs, s.UUID)
	}
	only("the reconciler's page of doc sources", sourceIDs)
	byID, err := workGraphDomain.ReadSourcesByUUID(ctx, workGraphDomain.KindDoc, []string{page.Uuid, folder.Uuid, file.Uuid})
	if err != nil || len(byID) != 1 || byID[0].UUID != page.Uuid {
		t.Errorf("doc sources by id: %+v %v; want the page alone", byID, err)
	}
	if one, err := workGraphDomain.ReadSource(ctx, workGraphDomain.KindDoc, link.Uuid); err != nil || one != nil {
		t.Errorf("a link read as a source: %+v %v", one, err)
	}

	// A save to the folder writes nothing and says why; a save to the page
	// is stored.
	_, err = domainDoc.WriteCollabSave(ctx, domainDoc.CollabWrite{DocUUID: folder.Uuid, Body: "<p>typed into a folder</p>", Snippet: "typed", State: "c3RhdGU=", Hash: "h1", Lineage: "L1"})
	if !errors.Is(err, domainDoc.ErrNotAPage) {
		t.Errorf("a save to the folder: %v, want ErrNotAPage", err)
	}
	stored, err := domainDoc.GetCollabDocByUUID(ctx, folder.Uuid)
	if err != nil || stored.Body != "" || stored.NodeKind != domainDoc.NodeFolder {
		t.Errorf("the folder after a refused save: %+v %v", stored, err)
	}
	if _, err := domainDoc.WriteCollabSave(ctx, domainDoc.CollabWrite{DocUUID: page.Uuid, Body: "<p>typed</p>", Snippet: "typed", State: "c3RhdGU=", Hash: "h1", Lineage: "L1"}); err != nil {
		t.Errorf("a save to the page: %v", err)
	}
}
