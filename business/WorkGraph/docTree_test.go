package workGraphBusiness

import (
	"context"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
)

// The doc tree (docs F1): folders, files and links are doc nodes, and the
// tree's one edge (doc_parent) joins them to pages and folders.
const (
	finance     = "50000000-0000-4000-8000-000000000001" // a folder, coloured moss
	q3          = "50000000-0000-4000-8000-000000000002" // a file in it
	budgetNotes = "50000000-0000-4000-8000-000000000003" // a page in it
	plan2       = "50000000-0000-4000-8000-000000000004" // a page in it, holding pages and a file
	minutes     = "50000000-0000-4000-8000-000000000005" // a page under plan2
	scan        = "50000000-0000-4000-8000-000000000006" // a file under plan2
	vault       = "50000000-0000-4000-8000-000000000007" // a folder the reader can't open
	salary      = "50000000-0000-4000-8000-000000000008" // a file in it
)

// nodeData is what each node's chip says beyond its title: its kind when it
// isn't a page, and its colour.
var nodeData = map[string]map[string]any{
	finance:     {"node_kind": "folder", "color": "moss"},
	q3:          {"node_kind": "file"},
	budgetNotes: {},
	plan2:       {},
	minutes:     {},
	scan:        {"node_kind": "file", "color": "sky"},
	vault:       {"node_kind": "folder"},
	salary:      {"node_kind": "file"},
}

// withTree sets the graph to answer reached, paths and near, and the reader
// to see every node but hidden, each with its chip's data (nodeData).
func withTree(t *testing.T, reached []domain.Reached, paths []domain.Path, near []domain.Near, hidden ...string) {
	t.Helper()
	withFakes(t, reached, paths, hidden...)
	savedR, savedE := readMany, readExpand
	t.Cleanup(func() { readMany, readExpand = savedR, savedE })
	plain := readMany
	readMany = func(ctx context.Context, who accessBusiness.Subject, refs []entityLinkBusiness.Ref) ([]entityLinkBusiness.Entity, error) {
		got, err := plain(ctx, who, refs)
		for i := range got {
			if d, ok := nodeData[got[i].ID]; ok && got[i].Access == entityLinkBusiness.AccessOK {
				got[i].Data = map[string]any{"title": got[i].Title}
				for k, v := range d {
					got[i].Data[k] = v
				}
			}
		}
		return got, err
	}
	readExpand = func(context.Context, []domain.Ref, []domain.Kind, int) ([]domain.Near, error) { return near, nil }
}

// treeHop is a hop along the doc tree's edge, from the previous node's side:
// to its parent (doc_parent) or to a child (~doc_parent).
func treeHop(pred string, id string) domain.Hop {
	rel := "parent"
	if strings.HasPrefix(pred, "~") {
		rel = "subpage"
	}
	return domain.Hop{Rel: rel, Pred: pred, To: ref(domain.KindDoc, id)}
}

func rels(hops []Hop) string {
	var out []string
	for _, h := range hops {
		out = append(out, h.Rel+">"+h.ID[len(h.ID)-1:])
	}
	return strings.Join(out, " ")
}

// TestTheDocTreesEdgeIsNamedByWhatItJoins: a folder's contents are in_folder
// (to the folder) and contains (from it); a folder, a file or a link under a
// page is held by it (holds), the page still its parent; two pages keep
// parent and subpage. Named from the nodes as the reader's own read decided
// them, the same on the panel's items, its paths, and the links Ask AI reads.
func TestTheDocTreesEdgeIsNamedByWhatItJoins(t *testing.T) {
	up, down := "doc_parent", "~doc_parent"

	// The file's panel: its folder, and what else the folder holds.
	withTree(t, []domain.Reached{
		{Node: ref(domain.KindDoc, finance), Path: []domain.Hop{treeHop(up, finance)}},
		{Node: ref(domain.KindDoc, budgetNotes), Path: []domain.Hop{treeHop(up, finance), treeHop(down, budgetNotes)}},
		{Node: ref(domain.KindDoc, plan2), Path: []domain.Hop{treeHop(up, finance), treeHop(down, plan2)}},
	}, nil, nil)
	got, err := NeighboursOf(context.Background(), reader, Ref{"doc", q3}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"doc 1: in_folder>1", "doc 3: in_folder>1 contains>3", "doc 4: in_folder>1 contains>4"}
	if g := lines(got); strings.Join(g, "\n") != strings.Join(want, "\n") {
		t.Errorf("the file's panel:\n%s\nwant\n%s", strings.Join(g, "\n"), strings.Join(want, "\n"))
	}

	// A page's panel: its folder, its subpage, and the file it holds.
	withTree(t, []domain.Reached{
		{Node: ref(domain.KindDoc, finance), Path: []domain.Hop{treeHop(up, finance)}},
		{Node: ref(domain.KindDoc, minutes), Path: []domain.Hop{treeHop(down, minutes)}},
		{Node: ref(domain.KindDoc, scan), Path: []domain.Hop{treeHop(down, scan)}},
	}, nil, nil)
	got, err = NeighboursOf(context.Background(), reader, Ref{"doc", plan2}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"doc 1: in_folder>1", "doc 5: subpage>5", "doc 6: holds>6"}
	if g := lines(got); strings.Join(g, "\n") != strings.Join(want, "\n") {
		t.Errorf("the page's panel:\n%s\nwant\n%s", strings.Join(g, "\n"), strings.Join(want, "\n"))
	}

	// The subpage's panel: its parent page, as before docs F1.
	withTree(t, []domain.Reached{{Node: ref(domain.KindDoc, plan2), Path: []domain.Hop{treeHop(up, plan2)}}}, nil, nil)
	got, err = NeighboursOf(context.Background(), reader, Ref{"doc", minutes}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g := lines(got); strings.Join(g, "\n") != "doc 4: parent>4" {
		t.Errorf("the subpage's panel: %v", g)
	}

	// A path: from the folder through a page it holds to that page's file.
	withTree(t, nil, []domain.Path{{From: ref(domain.KindDoc, finance), Hops: []domain.Hop{treeHop(down, plan2), treeHop(down, scan)}}}, nil)
	p, err := PathsBetween(context.Background(), reader, Ref{"doc", finance}, Ref{"doc", scan})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Paths) != 1 || rels(p.Paths[0].Hops) != "contains>4 holds>6" {
		t.Errorf("the path: %+v", p.Paths)
	}

	// Ask AI's one hop: the file's folder, the folder's file.
	withTree(t, nil, nil, []domain.Near{
		{Root: ref(domain.KindDoc, q3), Rel: "parent", Pred: up, Node: ref(domain.KindDoc, finance)},
		{Root: ref(domain.KindDoc, finance), Rel: "subpage", Pred: down, Node: ref(domain.KindDoc, budgetNotes)},
	})
	links, err := ExpandFor(context.Background(), []accessBusiness.Subject{reader}, []Ref{{"doc", q3}, {"doc", finance}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lrels []string
	for _, l := range links {
		lrels = append(lrels, l.From.ID[len(l.From.ID)-1:]+" "+l.Rel+" "+l.To.ID[len(l.To.ID)-1:]+"/"+l.To.Rel)
	}
	if strings.Join(lrels, ", ") != "2 in_folder 1/in_folder, 1 contains 3/contains" {
		t.Errorf("Ask AI's links: %v", lrels)
	}
}

// TestADocItemCarriesItsKindAndColour: a folder's, a file's and a link's row
// carry what the node is and its colour, as its chip says them, and the
// thing asked about does too; a path through a folder the reader can't open
// is dropped whole, its file with it.
func TestADocItemCarriesItsKindAndColour(t *testing.T) {
	withTree(t, []domain.Reached{
		{Node: ref(domain.KindDoc, q3), Path: []domain.Hop{treeHop("~doc_parent", q3)}},
		{Node: ref(domain.KindDoc, plan2), Path: []domain.Hop{treeHop("~doc_parent", plan2)}},
		{Node: ref(domain.KindDoc, scan), Path: []domain.Hop{treeHop("~doc_parent", plan2), treeHop("~doc_parent", scan)}},
		{Node: ref(domain.KindDoc, vault), Path: []domain.Hop{{Rel: "links_to", Pred: "doc_mentions", To: ref(domain.KindDoc, vault)}}},
		{Node: ref(domain.KindDoc, salary), Path: []domain.Hop{{Rel: "links_to", Pred: "doc_mentions", To: ref(domain.KindDoc, vault)}, treeHop("~doc_parent", salary)}},
	}, nil, nil, vault)
	got, err := NeighboursOf(context.Background(), reader, Ref{"doc", finance}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root.Data["node_kind"] != "folder" || got.Root.Data["color"] != "moss" {
		t.Errorf("the folder asked about: %v", got.Root.Data)
	}
	data := map[string]map[string]any{}
	why := map[string]string{}
	for _, g := range got.Groups {
		for _, it := range g.Items {
			if g.Kind != "doc" {
				t.Errorf("a %s group: a folder's contents are docs", g.Kind)
			}
			data[it.ID], why[it.ID] = it.Data, rels(it.Path)
		}
	}
	if d := data[q3]; d["node_kind"] != "file" || d["color"] != nil || why[q3] != "contains>2" {
		t.Errorf("the file's row: %v, %s", d, why[q3])
	}
	if why[scan] != "contains>4 holds>6" {
		t.Errorf("the file under the folder's page: %s", why[scan])
	}
	if d := data[scan]; d["node_kind"] != "file" || d["color"] != "sky" {
		t.Errorf("the coloured file's row: %v", d)
	}
	if d := data[plan2]; d["node_kind"] != nil {
		t.Errorf("a page's row says a kind: %v", d)
	}
	for _, hiddenID := range []string{vault, salary} {
		if _, ok := data[hiddenID]; ok {
			t.Errorf("the hidden folder's path shows %s", hiddenID)
		}
	}
	if len(data) != 3 {
		t.Errorf("%d rows, want the file, the page and its file: %v", len(data), data)
	}
}
