package workGraphBusiness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
)

const (
	plan    = "10000000-0000-4000-8000-000000000001" // the root doc
	secret  = "10000000-0000-4000-8000-000000000002" // a private doc the reader can't open
	brief   = "10000000-0000-4000-8000-000000000003" // reached only through secret
	notes   = "10000000-0000-4000-8000-000000000004" // reached through secret and through task1
	task1   = "20000000-0000-4000-8000-000000000001"
	hidTask = "20000000-0000-4000-8000-000000000002"
	proj    = "30000000-0000-4000-8000-000000000001"
	sam     = "40000000-0000-4000-8000-000000000001"
)

var reader = accessBusiness.Subject{UserUID: "0x1", UserUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}

func ref(k domain.Kind, id string) domain.Ref { return domain.Ref{Kind: k, UUID: id} }

func hop(rel string, k domain.Kind, id string) domain.Hop {
	return domain.Hop{Rel: rel, To: ref(k, id)}
}

// fakes sets the graph to answer reached and paths, and the readers to see
// everything but hidden; it counts the calls to each.
type fakes struct{ graph, reads int }

func withFakes(t *testing.T, reached []domain.Reached, paths []domain.Path, hidden ...string) *fakes {
	t.Helper()
	f := &fakes{}
	savedN, savedP, savedR := readNeighbours, readPaths, readMany
	t.Cleanup(func() { readNeighbours, readPaths, readMany = savedN, savedP, savedR })
	readNeighbours = func(_ context.Context, root domain.Ref, depth int, kinds []domain.Kind) ([]domain.Reached, error) {
		f.graph++
		if root.UUID == hidTask+"-missing" {
			return nil, domain.ErrNotFound
		}
		return reached, nil
	}
	readPaths = func(_ context.Context, a, b domain.Ref) ([]domain.Path, error) {
		f.graph++
		return paths, nil
	}
	hide := map[string]bool{}
	for _, h := range hidden {
		hide[h] = true
	}
	readMany = func(_ context.Context, who accessBusiness.Subject, refs []entityLinkBusiness.Ref) ([]entityLinkBusiness.Entity, error) {
		f.reads++
		if who != reader {
			return nil, fmt.Errorf("read as %+v", who)
		}
		out := make([]entityLinkBusiness.Entity, len(refs))
		for i, r := range refs {
			out[i] = entityLinkBusiness.Entity{Kind: r.Kind, ID: r.ID, Access: entityLinkBusiness.AccessNone}
			if !hide[r.ID] {
				out[i].Access, out[i].Title = entityLinkBusiness.AccessOK, "T-"+r.ID[len(r.ID)-1:]
			}
		}
		return out, nil
	}
	return f
}

// lines is each item as "kind id-tail: rel>tail rel>tail".
func lines(c *Connected) []string {
	var out []string
	for _, g := range c.Groups {
		for _, it := range g.Items {
			var hops []string
			for _, h := range it.Path {
				hops = append(hops, h.Rel+">"+h.ID[len(h.ID)-1:])
			}
			out = append(out, g.Kind+" "+it.ID[len(it.ID)-1:]+": "+strings.Join(hops, " "))
		}
	}
	return out
}

// TestAPathThroughSomethingHiddenIsDroppedWhole: the private doc isn't
// shown; the doc reached only through it isn't either (and no gap stands in
// for it); a doc reached through it and another way is shown by the way
// the reader can see.
func TestAPathThroughSomethingHiddenIsDroppedWhole(t *testing.T) {
	reached := []domain.Reached{
		{Node: ref(domain.KindDoc, secret), Path: []domain.Hop{hop("links_to", domain.KindDoc, secret)}},
		{Node: ref(domain.KindTask, task1), Path: []domain.Hop{hop("linked_from", domain.KindTask, task1)}},
		{Node: ref(domain.KindDoc, brief), Path: []domain.Hop{hop("links_to", domain.KindDoc, secret), hop("links_to", domain.KindDoc, brief)}},
		{Node: ref(domain.KindDoc, notes), Path: []domain.Hop{hop("links_to", domain.KindDoc, secret), hop("links_to", domain.KindDoc, notes)}},
		{Node: ref(domain.KindDoc, notes), Path: []domain.Hop{hop("linked_from", domain.KindTask, task1), hop("links_to", domain.KindDoc, notes)}},
		{Node: ref(domain.KindProject, proj), Path: []domain.Hop{hop("linked_from", domain.KindTask, task1), hop("in_project", domain.KindProject, proj)}},
	}
	f := withFakes(t, reached, nil, secret)
	got, err := NeighboursOf(context.Background(), reader, Ref{"doc", strings.ToUpper(plan)}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"doc 4: linked_from>1 links_to>4",
		"task 1: linked_from>1",
		"project 1: linked_from>1 in_project>1",
	}
	if strings.Join(lines(got), "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(lines(got), "\n"), strings.Join(want, "\n"))
	}
	raw, _ := json.Marshal(got)
	for _, leak := range []string{secret, brief} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the answer names %s: %s", leak, raw)
		}
	}
	if f.graph != 1 || f.reads != 1 {
		t.Errorf("%d graph reads and %d entity reads, want one of each", f.graph, f.reads)
	}
}

// TestAHiddenRootIsNotFound: a thing the reader can't see answers as one
// that isn't there, and so does one that isn't there.
func TestAHiddenRootIsNotFound(t *testing.T) {
	withFakes(t, []domain.Reached{{Node: ref(domain.KindTask, task1), Path: []domain.Hop{hop("linked_from", domain.KindTask, task1)}}}, nil, secret)
	if _, err := NeighboursOf(context.Background(), reader, Ref{"doc", secret}, 1, nil); err != ErrNotFound {
		t.Errorf("a hidden root: %v", err)
	}
	if _, err := NeighboursOf(context.Background(), reader, Ref{"task", hidTask + "-missing"}, 1, nil); err != ErrBadRequest {
		t.Errorf("a bad id: %v", err)
	}
	if _, err := NeighboursOf(context.Background(), accessBusiness.Subject{}, Ref{"doc", plan}, 1, nil); err != ErrNotFound {
		t.Errorf("no one: %v", err)
	}
	for _, bad := range []Ref{{"doc", "0x1"}, {"folder", plan}, {"doc", ""}} {
		if _, err := NeighboursOf(context.Background(), reader, bad, 1, nil); err != ErrBadRequest {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if _, err := NeighboursOf(context.Background(), reader, Ref{"doc", plan}, 3, nil); err != ErrBadRequest {
		t.Errorf("depth 3: %v", err)
	}
}

// TestMoreCountsOnlyWhatTheReaderSees: a group shows PerKind things and says
// there are more only when more it can see were found; hidden ones never
// make it say so.
func TestMoreCountsOnlyWhatTheReaderSees(t *testing.T) {
	var reached []domain.Reached
	var hidden []string
	for i := 0; i < PerKind+5; i++ {
		id := fmt.Sprintf("50000000-0000-4000-8000-%012d", i)
		reached = append(reached, domain.Reached{Node: ref(domain.KindDoc, id), Path: []domain.Hop{hop("subpage", domain.KindDoc, id)}})
		if i >= PerKind {
			hidden = append(hidden, id)
		}
	}
	withFakes(t, reached, nil, hidden...)
	got, err := NeighboursOf(context.Background(), reader, Ref{"doc", plan}, 1, nil)
	if err != nil || len(got.Groups) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if g := got.Groups[0]; len(g.Items) != PerKind || g.More {
		t.Errorf("%d shown, more %v: hidden docs made it say more", len(g.Items), g.More)
	}
	withFakes(t, reached, nil)
	got, _ = NeighboursOf(context.Background(), reader, Ref{"doc", plan}, 1, nil)
	if g := got.Groups[0]; len(g.Items) != PerKind || !g.More {
		t.Errorf("%d shown, more %v, want more", len(g.Items), g.More)
	}
}

// TestKindsFilterTheGroupsNotThePaths: a read for projects shows projects,
// reached through a task, which is checked but not listed.
func TestKindsFilterTheGroupsNotThePaths(t *testing.T) {
	reached := []domain.Reached{
		{Node: ref(domain.KindTask, task1), Path: []domain.Hop{hop("linked_from", domain.KindTask, task1)}},
		{Node: ref(domain.KindProject, proj), Path: []domain.Hop{hop("linked_from", domain.KindTask, task1), hop("in_project", domain.KindProject, proj)}},
	}
	withFakes(t, reached, nil)
	got, err := NeighboursOf(context.Background(), reader, Ref{"doc", plan}, 2, []domain.Kind{domain.KindProject})
	if err != nil {
		t.Fatal(err)
	}
	if l := lines(got); len(l) != 1 || l[0] != "project 1: linked_from>1 in_project>1" {
		t.Errorf("got %v", l)
	}
	withFakes(t, reached, nil, task1)
	got, _ = NeighboursOf(context.Background(), reader, Ref{"doc", plan}, 2, []domain.Kind{domain.KindProject})
	if l := lines(got); len(l) != 0 {
		t.Errorf("a project through a hidden task: %v", l)
	}
}

// TestPathsDropEveryPathThroughSomethingHidden: of three paths, the one
// through the private doc goes; either end hidden is not found.
func TestPathsDropEveryPathThroughSomethingHidden(t *testing.T) {
	paths := []domain.Path{
		{From: ref(domain.KindDoc, plan), Hops: []domain.Hop{hop("links_to", domain.KindDoc, secret), hop("created_by", domain.KindPerson, sam)}},
		{From: ref(domain.KindDoc, plan), Hops: []domain.Hop{hop("created_by", domain.KindPerson, sam)}},
	}
	f := withFakes(t, nil, paths, secret)
	got, err := PathsBetween(context.Background(), reader, Ref{"doc", plan}, Ref{"person", sam})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Paths) != 1 || len(got.Paths[0].Hops) != 1 || got.Paths[0].Hops[0].Rel != "created_by" {
		t.Errorf("paths %+v", got.Paths)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), secret) {
		t.Errorf("names the private doc: %s", raw)
	}
	if f.graph != 1 || f.reads != 1 {
		t.Errorf("%d graph reads and %d entity reads, want one of each", f.graph, f.reads)
	}
	if _, err := PathsBetween(context.Background(), reader, Ref{"doc", secret}, Ref{"person", sam}); err != ErrNotFound {
		t.Errorf("from a hidden doc: %v", err)
	}
	if _, err := PathsBetween(context.Background(), reader, Ref{"person", sam}, Ref{"doc", secret}); err != ErrNotFound {
		t.Errorf("to a hidden doc: %v", err)
	}
	withFakes(t, nil, paths[:1], secret)
	got, err = PathsBetween(context.Background(), reader, Ref{"doc", plan}, Ref{"person", sam})
	if err != nil || got.Paths == nil || len(got.Paths) != 0 {
		t.Errorf("only a hidden path: %+v %v", got, err)
	}
}

func TestParseKinds(t *testing.T) {
	if k, err := ParseKinds(""); err != nil || len(k) != len(domain.DefaultKinds) {
		t.Errorf("empty: %v %v", k, err)
	}
	if k, err := ParseKinds("doc, task"); err != nil || len(k) != 2 || k[1] != domain.KindTask {
		t.Errorf("doc, task: %v %v", k, err)
	}
	if _, err := ParseKinds("doc,secrets"); err != ErrBadRequest {
		t.Errorf("an unknown kind: %v", err)
	}
}
