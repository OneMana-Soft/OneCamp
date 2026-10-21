package workGraphBusiness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	domain "github.com/akashc777/OneCamp/domain/WorkGraph"
)

func withExpand(t *testing.T, near []domain.Near, hides map[accessBusiness.Subject][]string) (*fakes, *[][]domain.Ref) {
	t.Helper()
	f := withReaders(t, nil, nil, hides)
	var asked [][]domain.Ref
	saved := readExpand
	t.Cleanup(func() { readExpand = saved })
	readExpand = func(_ context.Context, roots []domain.Ref, kinds []domain.Kind, perEdge int) ([]domain.Near, error) {
		f.graph++
		asked = append(asked, roots)
		return near, nil
	}
	return f, &asked
}

func at(day int) time.Time { return time.Date(2026, 10, day, 9, 0, 0, 0, time.UTC) }

// TestExpandDropsWhatAnyReaderCantSee: a seed hidden from the asker gives
// nothing; a link to a private doc is dropped; one graph read and one
// ReadMany per reader serve every seed.
func TestExpandDropsWhatAnyReaderCantSee(t *testing.T) {
	near := []domain.Near{
		{Root: ref(domain.KindDoc, plan), Rel: "links_to", Node: ref(domain.KindDoc, secret)},
		{Root: ref(domain.KindDoc, plan), Rel: "linked_from", Node: ref(domain.KindTask, task1), Changed: at(10)},
		{Root: ref(domain.KindDoc, notes), Rel: "linked_from", Node: ref(domain.KindTask, hidTask)},
		{Root: ref(domain.KindTask, task1), Rel: "in_project", Node: ref(domain.KindProject, proj)},
	}
	f, asked := withExpand(t, near, map[accessBusiness.Subject][]string{reader: nil, asker: {secret, notes}})
	got, err := ExpandFor(context.Background(), []accessBusiness.Subject{reader, asker},
		[]Ref{{"doc", plan}, {"doc", notes}, {"task", task1}, {"widget", plan}, {"doc", "0x1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range got {
		lines = append(lines, l.From.Kind+" "+l.From.ID[len(l.From.ID)-1:]+" "+l.Rel+" "+l.To.Kind+" "+l.To.ID[len(l.To.ID)-1:])
	}
	want := []string{"doc 1 linked_from task 1", "task 1 in_project project 1"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	raw, _ := json.Marshal(got)
	for _, leak := range []string{secret, hidTask, notes} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("names %s: %s", leak, raw)
		}
	}
	if f.graph != 1 || f.reads != 2 {
		t.Errorf("%d graph reads, %d entity reads; want 1 and 2", f.graph, f.reads)
	}
	if len(*asked) != 1 || len((*asked)[0]) != 3 {
		t.Errorf("the graph was asked from %v, want the three seeds it holds", *asked)
	}
}

// TestExpandRanksAndCaps: tasks before docs before people, the most recent
// first within a kind; never more than ExpandMaxLinks; each thing once.
func TestExpandRanksAndCaps(t *testing.T) {
	var near []domain.Near
	for i := 0; i < ExpandMaxLinks+10; i++ {
		near = append(near, domain.Near{Root: ref(domain.KindDoc, plan), Rel: "created_by",
			Node: ref(domain.KindPerson, fmt.Sprintf("60000000-0000-4000-8000-%012d", i))})
	}
	near = append(near,
		domain.Near{Root: ref(domain.KindDoc, plan), Rel: "links_to", Node: ref(domain.KindDoc, brief), Changed: at(3)},
		domain.Near{Root: ref(domain.KindDoc, plan), Rel: "linked_from", Node: ref(domain.KindTask, hidTask), Changed: at(1)},
		domain.Near{Root: ref(domain.KindDoc, plan), Rel: "linked_from", Node: ref(domain.KindTask, task1), Changed: at(9)},
		domain.Near{Root: ref(domain.KindTask, task1), Rel: "links_to", Node: ref(domain.KindDoc, brief), Changed: at(3)},
	)
	withExpand(t, near, map[accessBusiness.Subject][]string{reader: nil})
	got, err := ExpandFor(context.Background(), []accessBusiness.Subject{reader}, []Ref{{"doc", plan}, {"task", task1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != ExpandMaxLinks {
		t.Fatalf("%d links, want %d", len(got), ExpandMaxLinks)
	}
	if got[0].To.ID != task1 || got[1].To.ID != hidTask || got[2].To.ID != brief || got[3].To.Kind != "person" {
		t.Errorf("order: %s %s %s %s", got[0].To.ID, got[1].To.ID, got[2].To.ID, got[3].To.Kind)
	}
	n := 0
	for _, l := range got {
		if l.To.ID == brief {
			n++
		}
	}
	if n != 1 {
		t.Errorf("brief is given %d times", n)
	}
}

// TestExpandForNoOne: no readers, or an unnamed one, read nothing.
func TestExpandForNoOne(t *testing.T) {
	f, _ := withExpand(t, nil, map[accessBusiness.Subject][]string{reader: nil})
	if _, err := ExpandFor(context.Background(), []accessBusiness.Subject{reader, {}}, []Ref{{"doc", plan}}, nil); err != ErrNotFound {
		t.Errorf("an unnamed reader: %v", err)
	}
	if f.graph != 0 {
		t.Errorf("the graph was read for no one")
	}
}
