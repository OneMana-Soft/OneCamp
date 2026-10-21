package business

import (
	"context"
	"fmt"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/business/Tree/graph"
)

// countingRules decides from a table and counts how many times it was asked.
type countingRules struct {
	accessBusiness.Resolver
	levels map[string]accessBusiness.Level
	asked  int
}

func (r *countingRules) ResolveAll(_ context.Context, _ accessBusiness.Subject, res []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error) {
	r.asked++
	out := map[accessBusiness.Resource]accessBusiness.Decision{}
	for _, one := range res {
		if l, ok := r.levels[one.UUID]; ok {
			out[one] = accessBusiness.Decision{Level: l}
		}
	}
	return out, nil
}

// pageKind is a tree kind whose every level is the same n rows.
type pageKind struct {
	graph.Kind
	rows []graph.Row
}

func (pageKind) Schema() graph.Schema { return graph.Schema{Kind: "levelreads-test"} }
func (k pageKind) Children(context.Context, string, string, int) ([]graph.Row, error) {
	return k.rows, nil
}
func (k pageKind) Top(context.Context, string, string, int) ([]graph.Row, error) {
	return k.rows, nil
}

// A level of the tree asks who may open and change its nodes once, however
// many there are, and the parent's level once: a page used to ask four times
// (the parent's edit then view, the children's view, then their edit).
func TestALevelAsksAboutAccessOnceAPiece(t *testing.T) {
	for _, n := range []int{3, 300} {
		levels := map[string]accessBusiness.Level{"parent": accessBusiness.LevelView}
		var rows []graph.Row
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("n%03d", i)
			rows = append(rows, graph.Row{UUID: id, Position: id})
			switch i % 3 {
			case 0:
				levels[id] = accessBusiness.LevelEdit
			case 1:
				levels[id] = accessBusiness.LevelView
			}
		}
		graph.RegisterKind(pageKind{rows: rows})
		rules := &countingRules{levels: levels}
		was := accessBusiness.Rules
		accessBusiness.Rules = rules

		page, err := hierarchy{}.Level(context.Background(), accessBusiness.Subject{UserUUID: "u"}, "levelreads-test",
			LevelQuery{Parent: "parent", Limit: MaxPage})
		accessBusiness.Rules = was
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if rules.asked != 2 {
			t.Errorf("n=%d: asked about access %d times, want 2 (the parent, then the level)", n, rules.asked)
		}
		wantOpen := 0
		for i := 0; i < n; i++ {
			if i%3 != 2 {
				wantOpen++
			}
		}
		if page.Count != wantOpen || page.Hidden != n-wantOpen {
			t.Errorf("n=%d: open %d hidden %d, want %d and %d", n, page.Count, page.Hidden, wantOpen, n-wantOpen)
		}
		for _, node := range page.Nodes {
			if want := levels[node.UUID] >= accessBusiness.LevelEdit; node.CanEdit != want {
				t.Errorf("n=%d: %s CanEdit %v, want %v", n, node.UUID, node.CanEdit, want)
			}
		}
	}
}

// A parent the person can't open is not found, and nothing under it is read.
func TestALevelUnderAParentOutOfSightIsNotFound(t *testing.T) {
	graph.RegisterKind(pageKind{rows: []graph.Row{{UUID: "c"}}})
	rules := &countingRules{levels: map[string]accessBusiness.Level{"c": accessBusiness.LevelEdit}}
	was := accessBusiness.Rules
	accessBusiness.Rules = rules
	defer func() { accessBusiness.Rules = was }()
	if _, err := (hierarchy{}).Level(context.Background(), accessBusiness.Subject{UserUUID: "u"}, "levelreads-test",
		LevelQuery{Parent: "hidden"}); err != ErrNotFound {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if rules.asked != 1 {
		t.Errorf("asked %d times, want 1", rules.asked)
	}
}
