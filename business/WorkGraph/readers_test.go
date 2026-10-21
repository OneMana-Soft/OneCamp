package workGraphBusiness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
)

// asker is a second reader: the person who asked an agent that runs as
// reader (its sponsor), or a token's owner's colleague asking through it.
var asker = accessBusiness.Subject{UserUID: "0x2", UserUUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}

// withReaders sets the graph to answer reached and paths, and each reader to
// see everything but what hides lists for them; it counts the reads.
func withReaders(t *testing.T, reached []domain.Reached, paths []domain.Path, hides map[accessBusiness.Subject][]string) *fakes {
	t.Helper()
	f := withFakes(t, reached, paths)
	readMany = func(_ context.Context, who accessBusiness.Subject, refs []entityLinkBusiness.Ref) ([]entityLinkBusiness.Entity, error) {
		f.reads++
		hidden, known := hides[who]
		if !known {
			t.Fatalf("read as someone the test doesn't know: %+v", who)
		}
		out := make([]entityLinkBusiness.Entity, len(refs))
		for i, r := range refs {
			out[i] = entityLinkBusiness.Entity{Kind: r.Kind, ID: r.ID, Access: entityLinkBusiness.AccessOK, Title: "T-" + r.ID[len(r.ID)-1:]}
			for _, h := range hidden {
				if h == r.ID {
					out[i] = entityLinkBusiness.Entity{Kind: r.Kind, ID: r.ID, Access: entityLinkBusiness.AccessNone}
				}
			}
		}
		return out, nil
	}
	return f
}

// TestTwoReadersSeeOnlyWhatBothMaySee: an agent runs as its sponsor, who
// may open the private doc; the person who asked may not. What it tells
// them is what both may see: the private doc, the doc reached only through
// it, and the task only the sponsor's project holds are dropped whole, and
// one graph read serves both readers.
func TestTwoReadersSeeOnlyWhatBothMaySee(t *testing.T) {
	reached := []domain.Reached{
		{Node: ref(domain.KindDoc, secret), Path: []domain.Hop{hop("links_to", domain.KindDoc, secret)}},
		{Node: ref(domain.KindTask, task1), Path: []domain.Hop{hop("linked_from", domain.KindTask, task1)}},
		{Node: ref(domain.KindTask, hidTask), Path: []domain.Hop{hop("linked_from", domain.KindTask, hidTask)}},
		{Node: ref(domain.KindDoc, brief), Path: []domain.Hop{hop("links_to", domain.KindDoc, secret), hop("links_to", domain.KindDoc, brief)}},
		{Node: ref(domain.KindProject, proj), Path: []domain.Hop{hop("linked_from", domain.KindTask, task1), hop("in_project", domain.KindProject, proj)}},
	}
	f := withReaders(t, reached, nil, map[accessBusiness.Subject][]string{
		reader: nil,
		asker:  {secret, hidTask},
	})
	got, err := NeighboursFor(context.Background(), []accessBusiness.Subject{reader, asker}, Ref{"doc", plan}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"task 1: linked_from>1", "project 1: linked_from>1 in_project>1"}
	if strings.Join(lines(got), "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(lines(got), "\n"), strings.Join(want, "\n"))
	}
	raw, _ := json.Marshal(got)
	for _, leak := range []string{secret, brief, hidTask} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the answer names %s: %s", leak, raw)
		}
	}
	if f.graph != 1 || f.reads != 2 {
		t.Errorf("%d graph reads and %d entity reads, want one and one per reader", f.graph, f.reads)
	}

	// The sponsor alone sees all of it.
	withReaders(t, reached, nil, map[accessBusiness.Subject][]string{reader: nil})
	alone, err := NeighboursFor(context.Background(), []accessBusiness.Subject{reader}, Ref{"doc", plan}, 2, nil)
	if err != nil || len(lines(alone)) != 5 {
		t.Errorf("the sponsor alone: %v %v", lines(alone), err)
	}
}

// TestARootEitherReaderCantSeeIsNotFound: the root hidden from the asker is
// "not found", as a thing that isn't there, however much the sponsor sees.
func TestARootEitherReaderCantSeeIsNotFound(t *testing.T) {
	reached := []domain.Reached{{Node: ref(domain.KindTask, task1), Path: []domain.Hop{hop("linked_from", domain.KindTask, task1)}}}
	withReaders(t, reached, nil, map[accessBusiness.Subject][]string{reader: nil, asker: {plan}})
	if _, err := NeighboursFor(context.Background(), []accessBusiness.Subject{reader, asker}, Ref{"doc", plan}, 1, nil); err != ErrNotFound {
		t.Errorf("a root the asker can't see: %v", err)
	}
	for name, readers := range map[string][]accessBusiness.Subject{
		"no readers":       nil,
		"an unnamed asker": {reader, {}},
	} {
		if _, err := NeighboursFor(context.Background(), readers, Ref{"doc", plan}, 1, nil); err != ErrNotFound {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := PathsFor(context.Background(), readers, Ref{"doc", plan}, Ref{"person", sam}); err != ErrNotFound {
			t.Errorf("paths, %s: %v", name, err)
		}
	}
}

// TestPathsForTwoReadersDropWhatEitherCantSee: the path through the doc the
// asker can't open is dropped, the other kept; an end hidden from the asker
// is not found.
func TestPathsForTwoReadersDropWhatEitherCantSee(t *testing.T) {
	paths := []domain.Path{
		{From: ref(domain.KindDoc, plan), Hops: []domain.Hop{hop("links_to", domain.KindDoc, secret), hop("created_by", domain.KindPerson, sam)}},
		{From: ref(domain.KindDoc, plan), Hops: []domain.Hop{hop("created_by", domain.KindPerson, sam)}},
	}
	hides := map[accessBusiness.Subject][]string{reader: nil, asker: {secret}}
	withReaders(t, nil, paths, hides)
	got, err := PathsFor(context.Background(), []accessBusiness.Subject{reader, asker}, Ref{"doc", plan}, Ref{"person", sam})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Paths) != 1 || len(got.Paths[0].Hops) != 1 {
		t.Errorf("paths %+v", got.Paths)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), secret) {
		t.Errorf("names the doc the asker can't open: %s", raw)
	}
	withReaders(t, nil, paths, hides)
	if _, err := PathsFor(context.Background(), []accessBusiness.Subject{reader, asker}, Ref{"doc", plan}, Ref{"doc", secret}); err != ErrNotFound {
		t.Errorf("to a doc the asker can't open: %v", err)
	}
}
