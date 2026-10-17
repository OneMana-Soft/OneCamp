package business

import (
	"context"
	"slices"
	"testing"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
)

// Someone else moving a page into a space, or out of one, tells its owner who
// can open it now, through the notify sink's own rules; a move that keeps its
// space, and someone who can't open the page, are told nothing.
func TestASpaceMoveTellsTheOwner(t *testing.T) {
	ts := withNotifySeams(t, nil)
	oldN := spaceName
	t.Cleanup(func() { spaceName = oldN })
	spaceName = func(_ context.Context, kind, id string) string {
		if kind == "project" && id == "x" {
			return "Launch"
		}
		return ""
	}
	ctx := context.Background()
	moved := func(data map[string]string, subjects ...string) resourceEventBusiness.Event {
		return resourceEventBusiness.Event{Kind: resourceEventBusiness.DocMoved, ResourceKind: "doc", ResourceUUID: docID,
			Actor: resourceEventBusiness.Actor{UserUUID: editID}, At: noon, Subjects: subjects, Data: data}
	}
	into := moved(map[string]string{"parent": "", "space": "project:x", "was": "none"}, ownerID)
	if !notifies(into) {
		t.Fatal("a move into a space that names the owner tells nobody")
	}
	notify(ctx, into)
	got := ts.of("doc.moved")
	if len(got) != 1 || !slices.Equal(got[0].to, []string{ownerID + ":owner"}) {
		t.Fatalf("into Launch told %+v", got)
	}
	if n := got[0].notice; n.Title != "Person a2 moved ‘Launch notes’ into Launch" || n.Body != "Everyone in Launch can open it now." ||
		n.Data["space"] != "project:x" || n.Path != "/app/doc/"+docID {
		t.Errorf("into Launch reads %+v", n)
	}

	notify(ctx, moved(map[string]string{"parent": "", "space": "none", "was": "team:gone"}, ownerID))
	if got = ts.of("doc.moved"); len(got) != 2 || got[1].notice.Title != "Person a2 moved ‘Launch notes’ out of its team" ||
		got[1].notice.Body != "Being in that team no longer lets people open it." {
		t.Errorf("out of a team that can't be read: %+v", got)
	}

	// Whoever it names is asked of the Access Resolver, once, with the rest.
	notify(ctx, moved(map[string]string{"parent": "", "space": "project:x", "was": "none"}, awayID))
	if len(ts.of("doc.moved")) != 2 {
		t.Error("someone who can't open the page was told")
	}
	for _, e := range []resourceEventBusiness.Event{
		moved(map[string]string{"parent": "p"}, ownerID),
		moved(map[string]string{"parent": "", "space": "project:x", "was": "none"}),
	} {
		if notifies(e) {
			t.Errorf("%+v tells someone", e)
		}
	}
}
