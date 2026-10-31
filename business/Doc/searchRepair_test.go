package businness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	openSearchGlobalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
)

// fakeDocSearchGraph stands in for the graph and search under RepairDocSearch.
// Its docs are in uid order. A page of more than maxPage docs is too large to
// send, as is any page holding a doc in tooLarge, whatever its size; with
// downFrom set, every read that reaches that offset fails with down. It
// counts the entries written per doc and the offsets whose uuid was read.
type fakeDocSearchGraph struct {
	docs     []*dgraphStruct.DgraphDoc
	maxPage  int
	tooLarge map[string]bool
	downFrom int
	down     error
	written  map[string]int
	named    []int
}

func newFakeDocSearchGraph(t *testing.T, docs int) *fakeDocSearchGraph {
	t.Helper()
	g := &fakeDocSearchGraph{maxPage: docs, tooLarge: map[string]bool{}, written: map[string]int{}}
	for i := 0; i < docs; i++ {
		g.docs = append(g.docs, &dgraphStruct.DgraphDoc{Uuid: fmt.Sprintf("doc-%03d", i), Title: "Notes", Body: "<p>Ship it</p>"})
	}
	oldPage, oldUUIDs, oldBulk, oldChildren := docSearchEntryPage, docUUIDPage, bulkIndexDocEntries, repairDocChildren
	t.Cleanup(func() {
		docSearchEntryPage, docUUIDPage, bulkIndexDocEntries, repairDocChildren = oldPage, oldUUIDs, oldBulk, oldChildren
	})
	page := func(offset, first int) []*dgraphStruct.DgraphDoc {
		if offset >= len(g.docs) {
			return nil
		}
		return g.docs[offset:min(offset+first, len(g.docs))]
	}
	docSearchEntryPage = func(_ context.Context, offset, first int) ([]*dgraphStruct.DgraphDoc, error) {
		if offset > len(g.docs)+docSearchRepairPage {
			t.Fatalf("the repair read on past the last doc, at offset %d", offset)
		}
		if g.down != nil && offset+first > g.downFrom {
			return nil, g.down
		}
		// What the graph's client says when an answer is over its 64 MB, wrapped
		// as a caller would wrap it.
		tooBig := fmt.Errorf("docs page: %w", status.Error(codes.ResourceExhausted, "grpc: received message larger than max (70254301 vs. 67108864)"))
		if first > g.maxPage {
			return nil, tooBig
		}
		docs := page(offset, first)
		for _, d := range docs {
			if g.tooLarge[d.Uuid] {
				return nil, tooBig
			}
		}
		return docs, nil
	}
	docUUIDPage = func(_ context.Context, offset, first int) ([]string, error) {
		g.named = append(g.named, offset)
		var ids []string
		for _, d := range page(offset, first) {
			ids = append(ids, d.Uuid)
		}
		return ids, nil
	}
	bulkIndexDocEntries = func(_ context.Context, entries []*openSearchStruct.OpenSearchDocEntry) (int, error) {
		for _, e := range entries {
			g.written[e.Uuid]++
		}
		return len(entries), nil
	}
	repairDocChildren = func(context.Context, map[string]openSearchGlobalSearchModels.DocChildFields) (int, int, error) {
		return 0, 0, nil
	}
	return g
}

// The graph's client takes at most 64 MB an answer, and a doc's pasted images
// are in its body, so a page of docs can be too large to send. That failed the
// whole repair, which started again from the first doc at every boot, never
// got past that page, and was never recorded as done. Now such a page is read
// again in smaller ones, and a doc too large to read alone is skipped and
// counted: every other doc's entry is written once.
func TestDocSearchRepairReadsAroundADocTooLargeToSend(t *testing.T) {
	const docs = 250
	g := newFakeDocSearchGraph(t, docs)
	g.maxPage = 10               // a page of 100 is too large
	g.tooLarge["doc-137"] = true // and this doc is, even alone

	rep, err := RepairDocSearch(context.Background())
	if err != nil {
		t.Fatalf("the repair failed: %v (so far: %s)", err, rep)
	}
	for _, d := range g.docs {
		want := 1
		if d.Uuid == "doc-137" {
			want = 0
		}
		if got := g.written[d.Uuid]; got != want {
			t.Errorf("%s's entry written %d times, want %d", d.Uuid, got, want)
		}
	}
	if rep.Docs != docs-1 || rep.Live != docs-1 || rep.Skipped != 1 {
		t.Errorf("the report: %s; want %d entries written and 1 doc skipped", rep, docs-1)
	}
	if !strings.Contains(rep.String(), "1 docs too large to read, skipped") {
		t.Errorf("the report recorded does not say what was skipped: %s", rep)
	}
	if len(g.named) != 1 || g.named[0] != 137 {
		t.Errorf("the docs named as skipped, by offset: %v, want [137]", g.named)
	}
}

// Any other failure of the graph still ends the repair with its error, to be
// tried again later, rather than skipping docs it could have read.
func TestDocSearchRepairStopsOnOtherErrors(t *testing.T) {
	g := newFakeDocSearchGraph(t, 250)
	g.maxPage = 10
	g.down, g.downFrom = status.Error(codes.Unavailable, "graph: connection refused"), 150

	rep, err := RepairDocSearch(context.Background())
	if !errors.Is(err, g.down) {
		t.Fatalf("the repair returned %v (%s), want the graph's own error", err, rep)
	}
	if rep.Skipped != 0 {
		t.Errorf("the repair skipped %d docs on an error that was not about their size", rep.Skipped)
	}
	if rep.Docs != 100 {
		t.Errorf("%d entries written before the graph stopped answering, want the first page's 100", rep.Docs)
	}
}

// Were every read refused as too large (a ResourceExhausted that is not about
// size), the repair still ends: a doc is skipped only where there is one, so
// it stops where the docs run out rather than skipping on for ever.
func TestDocSearchRepairEndsWhenEveryReadIsRefused(t *testing.T) {
	const docs = 25
	g := newFakeDocSearchGraph(t, docs)
	g.maxPage = 0 // every read is refused, even of nothing

	rep, err := RepairDocSearch(context.Background())
	if err != nil {
		t.Fatalf("the repair failed: %v (so far: %s)", err, rep)
	}
	if rep.Docs != 0 || rep.Skipped != docs {
		t.Errorf("the report: %s; want all %d docs skipped and nothing written", rep, docs)
	}
}

// The repair writes what a doc's own write does (docs F1): a folder and a
// link get an entry with their title alone, a file none of its own (it is
// found through its versions' entries), and every one of them still has its
// files' and comments' fields written for it.
func TestDocSearchRepairWritesFoldersLinksAndFilesAsTheirOwnWritesDo(t *testing.T) {
	g := newFakeDocSearchGraph(t, 0)
	g.maxPage = docSearchRepairPage
	g.docs = []*dgraphStruct.DgraphDoc{
		{Uuid: "page", Title: "Plan", Body: "<p>Ship it</p>"},
		{Uuid: "folder", Title: "Contracts", NodeKind: "folder"},
		{Uuid: "file", Title: "msa.pdf", NodeKind: "file"},
		{Uuid: "link", Title: "Roadmap", NodeKind: "link", LinkURL: "https://docs.google.com/d/1"},
	}
	var bodies = map[string]string{}
	bulkIndexDocEntries = func(_ context.Context, entries []*openSearchStruct.OpenSearchDocEntry) (int, error) {
		for _, e := range entries {
			g.written[e.Uuid]++
			bodies[e.Uuid] = e.DocBody
		}
		return len(entries), nil
	}
	var fields map[string]openSearchGlobalSearchModels.DocChildFields
	repairDocChildren = func(_ context.Context, f map[string]openSearchGlobalSearchModels.DocChildFields) (int, int, error) {
		fields = f
		return 0, 0, nil
	}
	rep, err := RepairDocSearch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g.written["page"] != 1 || g.written["folder"] != 1 || g.written["link"] != 1 || g.written["file"] != 0 {
		t.Errorf("entries written: %v; want the page, the folder and the link once, the file never", g.written)
	}
	if bodies["page"] != "Ship it" || bodies["folder"] != "" || bodies["link"] != "" {
		t.Errorf("bodies written: %v", bodies)
	}
	if rep.Docs != 3 || rep.Live != 3 {
		t.Errorf("the report: %+v", rep)
	}
	if fields["file"].Title != "msa.pdf" || len(fields) != 4 {
		t.Errorf("the files' and comments' fields: %v; want all four, the file's under its title", fields)
	}
}
