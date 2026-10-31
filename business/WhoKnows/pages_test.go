package whoKnowsBusiness

import (
	"context"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	domain "github.com/akashc777/OneCamp/domain/WhoKnows"
)

// TestWhoKnowsCountsPagesOnly (docs F1, DGF1): a folder, a file and a link
// are docs the asker may open, but making one isn't writing about the topic,
// and a comment on an upload isn't either. They are left out once decided:
// never sent to the count, never evidence and never "considered", which says
// how many items that could count were looked at. The page in the same
// folder still credits whoever wrote it. The decision is the one read it was
// (its chip's data says the kind): no read is added.
func TestWhoKnowsCountsPagesOnly(t *testing.T) {
	page, folder, file, link := id(1), id(2), id(3), id(4)
	cy, hal, jo := id(10), id(11), id(12)
	w := &world{
		open:   map[string]string{page: "Budget notes", folder: "Budget", file: "budget.xlsx", link: "Budget sheet"},
		people: map[string]string{cy: "cy", hal: "hal", jo: "jo"},
		seeds:  []domain.Seed{{Kind: "doc", ID: folder}, {Kind: "doc", ID: file}, {Kind: "doc", ID: page}, {Kind: "doc", ID: link}},
		did: []domain.Contribution{
			{Kind: "doc", Item: page, Person: cy, Role: domain.RoleWrote},
			{Kind: "doc", Item: folder, Person: hal, Role: domain.RoleWrote},
			{Kind: "doc", Item: file, Person: hal, Role: domain.RoleWrote},
			{Kind: "doc", Item: link, Person: hal, Role: domain.RoleWrote},
			{Kind: "doc", Item: file, Person: jo, Role: domain.RoleCommented},
		},
	}
	w.install(t)
	kinds := map[string]string{folder: "folder", file: "file", link: "link"}
	plain := readMany
	reads := 0
	readMany = func(ctx context.Context, who accessBusiness.Subject, refs []entityLinkBusiness.Ref) ([]entityLinkBusiness.Entity, error) {
		reads++
		got, err := plain(ctx, who, refs)
		for i := range got {
			if k, ok := kinds[got[i].ID]; ok && got[i].Access == entityLinkBusiness.AccessOK {
				got[i].Data = map[string]any{"title": got[i].Title, "node_kind": k}
			}
		}
		return got, err
	}
	a, err := Ask(context.Background(), asker, "budget", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(a), ","); got != "cy" {
		t.Errorf("named %q, want cy alone: a folder, an upload or a link isn't writing", got)
	}
	if a.Considered != 1 {
		t.Errorf("considered %d, want 1: only the page could count", a.Considered)
	}
	if w.counted == nil || strings.Join(w.counted.Docs, ",") != page {
		t.Errorf("counted %+v, want the page alone", w.counted)
	}
	for _, p := range a.People {
		for _, e := range p.Evidence {
			if e.ID != page {
				t.Errorf("%s's evidence names %s", p.Name, e.ID)
			}
		}
	}
	if reads != 2 {
		t.Errorf("%d reads, want the seeds decided once and the people named once", reads)
	}
}
