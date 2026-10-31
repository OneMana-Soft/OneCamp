package business

import (
	"context"
	"fmt"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workGraph "github.com/akashc777/OneCamp/business/WorkGraph"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// The doc tree (docs F1, DGF1): folders, files and links are doc nodes, and
// their chips say what they are (data.node_kind).
const (
	dkFolder = "d0000000-0000-4000-8000-000000000001" // Finance, a folder
	dkFile   = "d0000000-0000-4000-8000-000000000002" // Q3 budget.xlsx, a file in it
	dkPage   = "d0000000-0000-4000-8000-000000000003" // Budget notes, a page in it
	dkLink   = "d0000000-0000-4000-8000-000000000004" // Forecast, a link under the page
)

var (
	dkFolderHop = workGraph.Hop{Kind: "doc", ID: dkFolder, Title: "Finance", Data: map[string]any{"title": "Finance", "node_kind": "folder", "color": "moss"}}
	dkFileHop   = workGraph.Hop{Kind: "doc", ID: dkFile, Title: "Q3 budget.xlsx", Data: map[string]any{"title": "Q3 budget.xlsx", "node_kind": "file"}}
	dkPageHop   = workGraph.Hop{Kind: "doc", ID: dkPage, Title: "Budget notes", Data: map[string]any{"title": "Budget notes"}}
	dkLinkHop   = workGraph.Hop{Kind: "doc", ID: dkLink, Title: "Forecast", Data: map[string]any{"title": "Forecast", "node_kind": "link"}}
)

// as is h reached along rel.
func as(h workGraph.Hop, rel string) workGraph.Hop { h.Rel = rel; return h }

// TestGraphContextNamesWhatADocIs: Ask AI's lines name a folder, a file and
// a link by what they are, and the doc tree's edge by what it joins (in a
// folder, a folder contains, a page holds); a page is still a doc. Each
// opens from an answer as a doc, where the doc's route shows it by its kind.
func TestGraphContextNamesWhatADocIs(t *testing.T) {
	links := []workGraph.Link{
		{From: dkFileHop, Rel: workGraph.RelInFolder, To: as(dkFolderHop, workGraph.RelInFolder)},
		{From: dkFolderHop, Rel: workGraph.RelContains, To: as(dkPageHop, workGraph.RelContains)},
		{From: dkPageHop, Rel: workGraph.RelHolds, To: as(dkLinkHop, workGraph.RelHolds)},
	}
	block, sources := formatGraphContext(links)
	for _, want := range []string{
		`file "Q3 budget.xlsx" (id: ` + dkFile + `) in folder "Finance" (id: ` + dkFolder + `)`,
		`folder "Finance" (id: ` + dkFolder + `) contains doc "Budget notes" (id: ` + dkPage + `)`,
		`doc "Budget notes" (id: ` + dkPage + `) holds link "Forecast" (id: ` + dkLink + `)`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the block lacks %q:\n%s", want, block)
		}
	}
	if len(sources) != 4 {
		t.Errorf("%d sources, want the four docs: %+v", len(sources), sources)
	}
	for _, s := range sources {
		if s.ContentType != "doc" || s.DocUUID != s.ContentUUID {
			t.Errorf("a source that doesn't open as a doc: %+v", s)
		}
	}
}

// TestGraphToolsNameWhatADocIs: related_items' and how_connected's answers
// name a folder, a file and a link by what they are, the thing asked about
// too, and say in, contains and holds for the doc tree's edge.
func TestGraphToolsNameWhatADocIs(t *testing.T) {
	c := &workGraph.Connected{Root: dkFolderHop, Depth: 2, Groups: []workGraph.Group{{Kind: "doc", Items: []workGraph.Item{
		{Hop: as(dkFileHop, workGraph.RelContains), Depth: 1, Path: []workGraph.Hop{as(dkFileHop, workGraph.RelContains)}},
		{Hop: as(dkLinkHop, workGraph.RelHolds), Depth: 2, Path: []workGraph.Hop{as(dkPageHop, workGraph.RelContains), as(dkLinkHop, workGraph.RelHolds)}},
	}}}}
	got := describeConnected(c)
	for _, want := range []string{
		`Connected to folder "Finance" (` + dkFolder + `):`,
		`- file "Q3 budget.xlsx" (` + dkFile + `): contains file "Q3 budget.xlsx" (` + dkFile + `)`,
		`- link "Forecast" (` + dkLink + `): contains doc "Budget notes" (` + dkPage + `), then holds link "Forecast" (` + dkLink + `)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("related_items lacks %q:\n%s", want, got)
		}
	}
	p := &workGraph.Paths{From: dkFileHop, To: dkLinkHop, Paths: []workGraph.Path{{Hops: []workGraph.Hop{
		as(dkFolderHop, workGraph.RelInFolder), as(dkPageHop, workGraph.RelContains), as(dkLinkHop, workGraph.RelHolds),
	}}}}
	got = describePaths(p)
	want := `1. file "Q3 budget.xlsx" (` + dkFile + `) in folder "Finance" (` + dkFolder + `), then contains doc "Budget notes" (` + dkPage +
		`), then holds link "Forecast" (` + dkLink + `)`
	if !strings.Contains(got, `How file "Q3 budget.xlsx" (`+dkFile+`) connects to link "Forecast" (`+dkLink+`):`) || !strings.Contains(got, want) {
		t.Errorf("how_connected:\n%s\nwant the line\n%s", got, want)
	}
}

// TestGraphToolsTakeADocsKindWords: an agent that names a folder, a file or a
// link by the word an answer gave it isn't refused: each is a doc to the
// work graph, as the thing asked about, as either end of how_connected, and
// in the kinds filter (each once). A word that is no kind still is refused.
func TestGraphToolsTakeADocsKindWords(t *testing.T) {
	savedN, savedP, savedU := graphNeighbours, graphPaths, personByUUID
	t.Cleanup(func() { graphNeighbours, graphPaths, personByUUID = savedN, savedP, savedU })
	personByUUID = func(context.Context, string) (*userModels.UserInfo, error) { return gcUser(), nil }
	var roots []string
	var kinds []string
	graphNeighbours = func(_ context.Context, _ []accessBusiness.Subject, root workGraph.Ref, _ int, k []domain.Kind) (*workGraph.Connected, error) {
		roots = append(roots, root.Kind)
		kinds = append(kinds, fmt.Sprint(k))
		return &workGraph.Connected{Root: dkFolderHop}, nil
	}
	var ends []string
	graphPaths = func(_ context.Context, _ []accessBusiness.Subject, a, b workGraph.Ref) (*workGraph.Paths, error) {
		ends = append(ends, a.Kind+">"+b.Kind)
		return &workGraph.Paths{From: dkFileHop, To: dkLinkHop}, nil
	}
	ctx := context.Background()
	for _, word := range []string{"folder", "File", " link ", "doc"} {
		text, _, err := executeRelatedItems(ctx, ai.ProposedAction{ToolName: "related_items", Params: map[string]string{"kind": word, "uuid": dkFolder, "kinds": "folder,task,file,link"}}, gcAsker)
		if err != nil || text == graphBadAsk {
			t.Errorf("related_items as %q: %q, %v", word, text, err)
		}
	}
	if got := strings.Join(roots, ","); got != "doc,doc,doc,doc" {
		t.Errorf("asked the graph about %s, want a doc each time", got)
	}
	for _, k := range kinds {
		if k != "[doc task]" {
			t.Errorf("the kinds filter reached the graph as %s, want [doc task]", k)
		}
	}
	if text, _, err := executeHowConnected(ctx, ai.ProposedAction{ToolName: "how_connected", Params: map[string]string{"from_kind": "file", "from_uuid": dkFile, "to_kind": "Link", "to_uuid": dkLink}}, gcAsker); err != nil || text == graphBadAsk {
		t.Errorf("how_connected from a file to a link: %q, %v", text, err)
	}
	if got := strings.Join(ends, ","); got != "doc>doc" {
		t.Errorf("how_connected asked the graph %s, want doc>doc", got)
	}
	graphNeighbours = workGraph.NeighboursFor // the real check refuses what isn't a kind
	if text, _, _ := executeRelatedItems(ctx, ai.ProposedAction{ToolName: "related_items", Params: map[string]string{"kind": "nonsense", "uuid": dkFolder}}, gcAsker); text != graphBadAsk {
		t.Errorf("a word that is no kind: %q", text)
	}
	if text, _, _ := executeRelatedItems(ctx, ai.ProposedAction{ToolName: "related_items", Params: map[string]string{"kind": "doc", "uuid": dkFolder, "kinds": "folder,nonsense"}}, gcAsker); text != graphBadAsk {
		t.Errorf("a kinds filter with a word that is no kind: %q", text)
	}
}
