package entityLinkBusiness

import (
	"context"
	"reflect"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// TestADocChipSaysWhatTheNodeIs (docs F1, DGF1): a doc's chip says what the
// node is when it isn't a page (a folder, a file or a link, with a link's
// provider family) and its colour when it has one, from the read that names it, so every reader of a doc's
// chip (the work graph's items, Ask AI's lines, the agents' answers) can say
// "the file Q3 budget.xlsx in the folder Finance" without reading again. A
// page says no kind, and a doc with no colour none; a hidden folder says
// nothing at all.
func TestADocChipSaysWhatTheNodeIs(t *testing.T) {
	withRules(t)
	const (
		folder  = "f0000000-0000-4000-8000-000000000001"
		file    = "f0000000-0000-4000-8000-000000000002"
		link    = "f0000000-0000-4000-8000-000000000003"
		page    = "f0000000-0000-4000-8000-000000000004"
		private = "f0000000-0000-4000-8000-000000000005"
	)
	accessBusiness.Rules = rules{levels: map[string]map[string]accessBusiness.Level{
		reader.UserUUID: {folder: accessBusiness.LevelView, file: accessBusiness.LevelView, link: accessBusiness.LevelView, page: accessBusiness.LevelView, private: accessBusiness.LevelNone},
	}}
	docs := map[string]*dgraphStruct.DgraphDoc{
		folder:  {Uuid: folder, Title: "Finance", NodeKind: "folder", Color: "moss"},
		file:    {Uuid: file, Title: "Q3 budget.xlsx", NodeKind: "file", Parent: &dgraphStruct.DgraphDoc{Uuid: folder}},
		link:    {Uuid: link, Title: "Forecast", NodeKind: "link", LinkURL: "https://docs.google.com/spreadsheets/d/1/edit", Color: "sky"},
		page:    {Uuid: page, Title: "Budget notes", Icon: "📒", Color: "sun"},
		private: {Uuid: private, Title: "Vault", NodeKind: "folder", Color: "berry"},
	}
	readDocChips = func(_ context.Context, ids []string) (map[string]*dgraphStruct.DgraphDoc, error) {
		out := map[string]*dgraphStruct.DgraphDoc{}
		for _, id := range ids {
			if d := docs[strings.ToLower(id)]; d != nil {
				out[strings.ToLower(id)] = d
			}
		}
		return out, nil
	}
	got, err := ReadEntities(context.Background(), reader, []Ref{{"doc", folder}, {"doc", file}, {"doc", link}, {"doc", page}, {"doc", private}})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"title": "Finance", "icon": "", "node_kind": "folder", "color": "moss"},
		{"title": "Q3 budget.xlsx", "icon": "", "node_kind": "file", "parent_id": folder},
		{"title": "Forecast", "icon": "", "node_kind": "link", "color": "sky", "link_family": "sheet"},
		{"title": "Budget notes", "icon": "📒", "color": "sun"},
		nil,
	}
	for i, e := range got {
		if !reflect.DeepEqual(e.Data, want[i]) {
			t.Errorf("%s: data %v, want %v", e.ID, e.Data, want[i])
		}
	}
	// A link's address isn't a chip's to show: the link's own page opens it.
	// Its provider's family is, so the panel marks it as the docs tree does
	// (a Google Sheet a moss sheet).
	if _, ok := got[2].Data["link_url"]; ok {
		t.Errorf("the link's chip carries its address: %v", got[2].Data)
	}
	if got[4].Access != AccessNone || got[4].Data != nil || got[4].Title != "" {
		t.Errorf("the hidden folder says %+v", got[4])
	}
}
