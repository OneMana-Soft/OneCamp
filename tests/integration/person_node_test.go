//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// A person who imported files has more graph nodes holding their user_uuid:
// one per kept file, typed a User by their next connect. Changes to what they
// may reach, and to their own fields, land on their own node, the first one,
// and taking something away reaches every node with their uuid.
func TestAPersonsChangesReachTheirOwnNode(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	olga, pia, doc, project := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:olga", "dgraph.type": "User", "user_uuid": olga.String(), "user_name": "olga"},
		{"uid": "_:pia", "dgraph.type": "User", "user_uuid": pia.String(), "user_name": "pia"},
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_uuid": doc.String(), "doc_name": "Plan", "doc_owner": map[string]any{"uid": "_:olga"}},
		{"uid": "_:project", "dgraph.type": "Project", "project_uuid": project.String(), "project_name": "Launch",
			"project_members": []map[string]any{{"uid": "_:pia"}}},
	})
	// The node an import made for Pia, after hers, and a membership that went
	// through every node holding her uuid.
	stray := dg.Mutate(t, []map[string]any{
		{"uid": "_:stray", "dgraph.type": "User", "user_uuid": pia.String(), "devices_connected": 1},
		{"uid": uids["project"], "project_members": []map[string]any{{"uid": "_:stray"}}},
	})["stray"]
	own := uids["pia"]

	type node struct {
		Uid string `json:"uid"`
	}
	type view struct {
		Doc []struct {
			Full    []node `json:"full"`
			Edit    []node `json:"edit"`
			Comment []node `json:"comment"`
			View    []node `json:"view"`
			Owner   node   `json:"doc_owner"`
			By      node   `json:"doc_updated_by"`
		} `json:"doc"`
		Project []struct {
			Members []node `json:"members"`
			Admins  []node `json:"admins"`
		} `json:"project"`
		People []struct {
			Uid      string `json:"uid"`
			Name     string `json:"user_name"`
			Capacity int    `json:"user_weekly_capacity"`
		} `json:"people"`
	}
	read := func() view {
		t.Helper()
		g, err := dgexec.Read[view](ctx, `query graph($doc: string, $project: string, $pia: string) {
			doc(func: eq(doc_uuid, $doc)) {
				full: doc_full_access_users @filter(eq(user_uuid, $pia)) { uid }
				edit: doc_editing_users @filter(eq(user_uuid, $pia)) { uid }
				comment: doc_commenting_users @filter(eq(user_uuid, $pia)) { uid }
				view: doc_reading_users @filter(eq(user_uuid, $pia)) { uid }
				doc_owner { uid }
				doc_updated_by { uid }
			}
			project(func: eq(project_uuid, $project)) {
				members: project_members @filter(eq(user_uuid, $pia)) { uid }
				admins: project_admins @filter(eq(user_uuid, $pia)) { uid }
			}
			people(func: eq(user_uuid, $pia)) { uid user_name user_weekly_capacity }
		}`, map[string]string{"$doc": doc.String(), "$project": project.String(), "$pia": pia.String()})
		if err != nil || len(g.Doc) != 1 || len(g.Project) != 1 {
			t.Fatalf("the graph: %+v (%v)", g, err)
		}
		return g
	}
	only := func(what string, got []node) {
		t.Helper()
		if len(got) != 1 || got[0].Uid != own {
			t.Errorf("%s: %v; want Pia's own node %s alone (the import's is %s)", what, got, own, stray)
		}
	}

	// Olga shares the doc with Pia, then lets her edit it, then takes it back.
	if err := docDomain.WriteDocAccess(ctx, []string{doc.String()}, docDomain.AccessWrite{People: map[string]string{pia.String(): "view"}}); err != nil {
		t.Fatalf("sharing the doc with Pia: %v", err)
	}
	only("Pia's view of the doc", read().Doc[0].View)
	if err := docDomain.WriteDocAccess(ctx, []string{doc.String()}, docDomain.AccessWrite{People: map[string]string{pia.String(): "edit"}}); err != nil {
		t.Fatalf("letting Pia edit the doc: %v", err)
	}
	g := read()
	only("Pia's edit of the doc", g.Doc[0].Edit)
	if len(g.Doc[0].View) != 0 {
		t.Errorf("Pia is still on the doc's view list: %v", g.Doc[0].View)
	}
	if err := docDomain.WriteDocAccess(ctx, []string{doc.String()}, docDomain.AccessWrite{People: map[string]string{pia.String(): ""}}); err != nil {
		t.Fatalf("taking the doc back from Pia: %v", err)
	}
	if d := read().Doc[0]; len(d.Full)+len(d.Edit)+len(d.Comment)+len(d.View) != 0 {
		t.Errorf("after the doc was taken back, a list still names Pia: %+v", d)
	}
	// Olga hands the doc to Pia.
	if err := docDomain.WriteDocAccess(ctx, []string{doc.String()}, docDomain.AccessWrite{OwnerUUID: pia.String()}); err != nil {
		t.Fatalf("handing the doc to Pia: %v", err)
	}
	if o := read().Doc[0].Owner; o.Uid != own {
		t.Errorf("the doc's owner: %s; want Pia's own node %s", o.Uid, own)
	}
	if err := docDomain.TouchDoc(ctx, doc.String(), pia.String(), time.Now()); err != nil {
		t.Fatalf("Pia's change to the doc: %v", err)
	}
	if by := read().Doc[0].By; by.Uid != own {
		t.Errorf("the doc's last editor: %s; want Pia's own node %s", by.Uid, own)
	}

	// Pia is made an admin of Launch, then leaves its members: the import's
	// node goes with her.
	if err := dgraphStruct.ChangeRoles(ctx, "project_uuid", project.String(), []dgraphStruct.RoleChange{{Predicate: "project_admins", Add: []string{pia.String()}}}); err != nil {
		t.Fatalf("making Pia an admin: %v", err)
	}
	only("Launch's admins", read().Project[0].Admins)
	if err := dgraphStruct.ChangeRoles(ctx, "project_uuid", project.String(), []dgraphStruct.RoleChange{{Predicate: "project_members", Remove: []string{pia.String()}}}); err != nil {
		t.Fatalf("taking Pia off Launch's members: %v", err)
	}
	if m := read().Project[0].Members; len(m) != 0 {
		t.Errorf("Launch's members still name Pia: %v", m)
	}

	// Her own fields: a new name, and her weekly capacity.
	if _, err := userDomain.CreateOrUpdateDgraphUser(ctx, &dgraphStruct.DgraphUser{Uuid: pia.String(), UserName: "pia.k"}); err != nil {
		t.Fatalf("renaming Pia: %v", err)
	}
	if saved, err := projectDomain.SetDgraphCapacity(ctx, pia.String(), projectDomain.CapacityTasks, 7); err != nil || !saved {
		t.Fatalf("Pia's capacity: saved %v (%v)", saved, err)
	}
	for _, p := range read().People {
		mine := p.Uid == own
		if (p.Name == "pia.k") != mine || (p.Capacity == 7) != mine {
			t.Errorf("node %s (Pia's own: %v) has name %q and capacity %d; want her fields on her own node alone", p.Uid, mine, p.Name, p.Capacity)
		}
	}
}
