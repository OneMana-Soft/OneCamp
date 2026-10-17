package business

import (
	"testing"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
)

// A6: the lazy backfill of flat comments into threads is silent. Reading
// them, marking them read and adopting one on a write emit nothing beyond the
// write's own event.
func TestTheBackfillOfFlatCommentsEmitsNothing(t *testing.T) {
	w := newWorld(t)
	w.doc.comments = []Comment{
		{ID: "11111111-1111-1111-1111-111111111111", Text: "<p>old one</p>", By: Person{UUID: "bob"}, CreatedAt: w.svc.now()},
		{ID: "22222222-2222-2222-2222-222222222222", Text: "<p>old two</p>", By: Person{UUID: "ann"}, CreatedAt: w.svc.now()},
	}
	kinds := func() []resourceEventBusiness.Kind {
		w.ev.mu.Lock()
		defer w.ev.mu.Unlock()
		var out []resourceEventBusiness.Kind
		for _, e := range w.ev.got {
			out = append(out, e.Kind)
		}
		return out
	}
	list, err := w.svc.List(ctx, who("ann"), w.res, Filter{})
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %d, %v", len(list), err)
	}
	for _, v := range list {
		if err := w.svc.MarkRead(ctx, who("ann"), v.ID); err != nil {
			t.Fatalf("mark read: %v", err)
		}
	}
	if got := kinds(); len(got) != 0 {
		t.Fatalf("reading flat comments emitted %v", got)
	}
	if _, err := w.svc.Reply(ctx, who("ann"), list[0].ID, Body{HTML: "<p>reply</p>"}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if got := kinds(); len(got) != 1 || got[0] != ThreadReplied {
		t.Fatalf("adopting a flat comment on a reply emitted %v, want only the reply's", got)
	}
	if _, err := w.svc.Resolve(ctx, who("ann"), list[1].ID); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := kinds(); len(got) != 2 || got[1] != ThreadResolved {
		t.Fatalf("adopting a flat comment on a resolve emitted %v, want only the resolve's", got)
	}
}
