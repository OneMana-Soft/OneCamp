//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/dgexec"
	"github.com/akashc777/OneCamp/models/pgexec"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// An import's file stages made a node holding the importer's user_uuid for each
// file they kept, and connects and role changes wrote through every node with
// the uuid. The merge moves each edge into such a stray onto the person's own
// node (the first holding the uuid), adds the stray's own edges the person
// lacks, backs both up and deletes the stray. A person renamed since (a stray
// holds an old name) is merged, keeping their own node's name, and an email
// is compared trimmed and in any case; a person whose nodes hold different
// emails is left as they are. The restore puts every node and edge back.
func TestStrayPersonNodesMergeIntoThePersonAndComeBack(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("postgres: %v", err)
	}
	dg := integration.SetupDgraph(t)
	pia, olga, mads, ivo := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	own := dg.Mutate(t, []map[string]any{
		{"uid": "_:pia", "dgraph.type": "User", "user_uuid": pia, "user_name": "pia", "user_full_name": "Pia Lund", "user_email_id": "pia@example.com"},
		{"uid": "_:olga", "dgraph.type": "User", "user_uuid": olga, "user_name": "olga", "user_email_id": "olga@example.com"},
		{"uid": "_:mads", "dgraph.type": "User", "user_uuid": mads, "user_name": "mads", "user_email_id": "mads@example.com"},
		{"uid": "_:ivo", "dgraph.type": "User", "user_uuid": ivo, "user_name": "ivo", "user_email_id": "ivo@example.com"},
	})
	// Made after their people's own nodes, as the imports made them: Pia's
	// typed stray (her connects wrote through it, before she took her new
	// surname), the untyped one a file stage made for her, Mads's, whose email
	// isn't his, and Ivo's, whose email is his written otherwise.
	g := dg.Mutate(t, []map[string]any{
		{"uid": "_:stray", "dgraph.type": "User", "user_uuid": pia, "user_full_name": "Pia Berg", "user_search_name": "pia", "user_device_connected": 1,
			"user_emoji_statuses": []map[string]any{{"uid": "_:status", "dgraph.type": "Status", "status_user_emoji_id": "palm"}},
			"user_projects":       []map[string]any{{"uid": "_:launch"}}},
		{"uid": "_:file_stray", "user_uuid": pia},
		{"uid": "_:mads_stray", "dgraph.type": "User", "user_uuid": mads, "user_email_id": "someone@example.org"},
		{"uid": "_:ivo_stray", "dgraph.type": "User", "user_uuid": ivo, "user_email_id": "  IVO@Example.com "},
		{"uid": own["pia"], "user_projects": []map[string]any{{"uid": "_:launch"}}},
		{"uid": "_:launch", "dgraph.type": "Project", "project_name": "Launch",
			"project_members": []map[string]any{{"uid": own["pia"]}, {"uid": "_:stray"}, {"uid": own["olga"]}},
			"project_admins":  []map[string]any{{"uid": "_:stray", "project_admins|via": "import"}}},
		{"uid": "_:team", "dgraph.type": "Team", "team_members": []map[string]any{{"uid": "_:stray"}}},
		{"uid": "_:channel", "dgraph.type": "Channel", "ch_members": []map[string]any{{"uid": own["pia"]}, {"uid": "_:stray"}}},
		{"uid": "_:doc", "dgraph.type": "Doc", "doc_owner": map[string]any{"uid": "_:stray"},
			"doc_editing_users": []map[string]any{{"uid": "_:stray"}}},
		{"uid": "_:task", "dgraph.type": "Task", "task_assignee": map[string]any{"uid": "_:stray"}},
		{"uid": "_:file", "dgraph.type": "Attachment", "attachment_created_by": map[string]any{"uid": "_:file_stray"}},
		{"uid": "_:comment", "dgraph.type": "Comment", "comment_by": map[string]any{"uid": "_:stray"}},
	})
	nodes := []string{own["pia"], own["olga"], own["mads"], own["ivo"], g["stray"], g["file_stray"], g["mads_stray"], g["ivo_stray"], g["status"],
		g["launch"], g["team"], g["channel"], g["doc"], g["task"], g["file"], g["comment"]}

	// Every node above with every predicate it was given, edges with their
	// facets, as JSON by uid.
	snapshot := func() map[string]string {
		t.Helper()
		got, err := dgexec.Read[struct {
			Nodes []map[string]any `json:"nodes"`
		}](ctx, `{ nodes(func: uid(`+strings.Join(nodes, ", ")+`)) {
			uid dgraph.type user_uuid user_name user_full_name user_email_id user_search_name user_device_connected
			status_user_emoji_id project_name
			project_members @facets { uid } project_admins @facets { uid } team_members @facets { uid }
			ch_members @facets { uid } doc_owner @facets { uid } doc_editing_users @facets { uid }
			task_assignee @facets { uid } attachment_created_by @facets { uid } comment_by @facets { uid }
			user_emoji_statuses @facets { uid } user_projects @facets { uid }
		} }`, nil)
		if err != nil {
			t.Fatalf("the graph: %v", err)
		}
		out := map[string]string{}
		for _, n := range got.Nodes {
			b, _ := json.Marshal(n)
			out[n["uid"].(string)] = string(b)
		}
		return out
	}
	backups := func() map[string]int {
		t.Helper()
		got, err := pgexec.Pairs[string, int](ctx, `SELECT kind, count(*) FROM person_merge_backups GROUP BY kind`)
		if err != nil {
			t.Fatalf("the backups: %v", err)
		}
		return got
	}
	same := func(when string, got, want map[string]string, uids ...string) {
		t.Helper()
		for _, u := range uids {
			if got[u] != want[u] {
				t.Errorf("%s: node %s is\n  %s\nwant\n  %s", when, u, got[u], want[u])
			}
		}
	}
	plan := userBusiness.StrayMerge{People: 2, Strays: 3, Moved: 7, Added: 1, Kept: 3, Skipped: 1, Renamed: 1}
	before := snapshot()

	// A dry run says what the merge would do and changes nothing.
	dry, err := userBusiness.MergeStrayPeople(ctx, true)
	if err != nil {
		t.Fatalf("the dry run: %v", err)
	}
	if dry != plan {
		t.Errorf("the dry run says %+v; want %+v", dry, plan)
	}
	same("after the dry run", snapshot(), before, nodes...)
	if b := backups(); len(b) != 0 {
		t.Errorf("the dry run backed up %v; want nothing", b)
	}

	rep, err := userBusiness.MergeStrayPeople(ctx, false)
	if err != nil || rep.Run == uuid.Nil {
		t.Fatalf("the merge: %+v (%v)", rep, err)
	}
	run := rep.Run
	if rep.Run = uuid.Nil; rep != plan {
		t.Errorf("the merge did %+v; want %+v", rep, plan)
	}
	after := snapshot()

	type edge struct {
		Uid string `json:"uid"`
		Via string `json:"project_admins|via"`
	}
	edges := func(node, pred string) []edge {
		t.Helper()
		var n map[string]json.RawMessage
		_ = json.Unmarshal([]byte(after[node]), &n)
		raw := n[pred]
		if len(raw) > 0 && raw[0] == '{' {
			raw = append(append([]byte{'['}, raw...), ']')
		}
		var out []edge
		_ = json.Unmarshal(raw, &out)
		slices.SortFunc(out, func(a, b edge) int { return strings.Compare(a.Uid, b.Uid) })
		return out
	}
	onPia := func(node, pred string, with ...string) {
		t.Helper()
		want := []edge{{Uid: own["pia"]}}
		for _, u := range with {
			want = append(want, edge{Uid: u})
		}
		slices.SortFunc(want, func(a, b edge) int { return strings.Compare(a.Uid, b.Uid) })
		if got := edges(node, pred); !slices.Equal(got, want) {
			t.Errorf("%s's %s after the merge: %v; want Pia's own node %s (with %v), not her strays %s, %s",
				node, pred, got, own["pia"], with, g["stray"], g["file_stray"])
		}
	}
	onPia(g["launch"], "project_members", own["olga"])
	onPia(g["team"], "team_members")
	onPia(g["channel"], "ch_members")
	onPia(g["doc"], "doc_owner")
	onPia(g["doc"], "doc_editing_users")
	onPia(g["task"], "task_assignee")
	onPia(g["file"], "attachment_created_by")
	onPia(g["comment"], "comment_by")
	if got := edges(g["launch"], "project_admins"); len(got) != 1 || got[0] != (edge{Uid: own["pia"], Via: "import"}) {
		t.Errorf("Launch's admins after the merge: %v; want Pia's own node with the edge's facet via=import", got)
	}
	if got := edges(own["pia"], "user_emoji_statuses"); len(got) != 1 || got[0].Uid != g["status"] {
		t.Errorf("Pia's statuses after the merge: %v; want the stray's %s", got, g["status"])
	}
	if got := edges(own["pia"], "user_projects"); len(got) != 1 || got[0].Uid != g["launch"] {
		t.Errorf("Pia's projects after the merge: %v; want Launch once", got)
	}
	var piaNode map[string]any
	_ = json.Unmarshal([]byte(after[own["pia"]]), &piaNode)
	if piaNode["user_name"] != "pia" || piaNode["user_full_name"] != "Pia Lund" || piaNode["user_email_id"] != "pia@example.com" ||
		piaNode["user_search_name"] != nil || piaNode["user_device_connected"] != nil {
		t.Errorf("Pia's own fields after the merge: %v; want hers as they were, nothing of the stray's", piaNode)
	}
	for _, s := range []string{g["stray"], g["file_stray"], g["ivo_stray"]} {
		if after[s] != `{"uid":"`+s+`"}` {
			t.Errorf("stray %s after the merge: %s; want nothing left", s, after[s])
		}
	}
	same("after the merge", after, before, own["olga"], own["mads"], g["mads_stray"], own["ivo"], g["status"])
	if got, want := backups(), map[string]int{"node": 3, "in_edge": 9, "out_edge": 2}; !maps.Equal(got, want) {
		t.Errorf("the merge backed up %v; want %v", got, want)
	}

	// Run again, the merge finds nothing left to do.
	again, err := userBusiness.MergeStrayPeople(ctx, false)
	if err != nil || again.People != 0 || again.Strays != 0 || again.Skipped != 1 {
		t.Errorf("the merge run again: %+v (%v); want nothing merged and Mads still left", again, err)
	}
	same("after the second merge", snapshot(), after, nodes...)

	// The restore brings every stray back, and every edge as it was.
	back, err := userBusiness.RestoreStrayMerge(ctx, run)
	if want := (userBusiness.StrayRestore{Strays: 3, Edges: 9}); err != nil || back != want {
		t.Errorf("the restore: %+v (%v); want %+v", back, err, want)
	}
	restored := snapshot()
	same("after the restore", restored, before, nodes...)
	if back, err := userBusiness.RestoreStrayMerge(ctx, run); err != nil || back != (userBusiness.StrayRestore{}) {
		t.Errorf("the restore run again: %+v (%v); want nothing left to restore", back, err)
	}
	same("after the second restore", snapshot(), restored, nodes...)
}

// A merge a crash stopped between writing a batch's backup and marking it
// leaves the rows pending. The next run keeps the backup of a batch whose
// strays are gone (its graph change landed) and drops the rows of one whose
// strays are all there (it never did), before it merges.
func TestAMergeACrashStoppedIsSettledBeforeTheNextRun(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("postgres: %v", err)
	}
	dg := integration.SetupDgraph(t)
	pia := uuid.NewString()
	g := dg.Mutate(t, []map[string]any{
		{"uid": "_:pia", "dgraph.type": "User", "user_uuid": pia, "user_name": "pia"},
	})
	g2 := dg.Mutate(t, []map[string]any{
		{"uid": "_:stray", "dgraph.type": "User", "user_uuid": pia},
		{"uid": "_:gone", "dgraph.type": "Status", "status_user_emoji_id": "palm"},
	})
	run := uuid.New()
	never, landed := uuid.New(), uuid.New()
	row := func(stray string) []userModels.MergeBackup {
		return []userModels.MergeBackup{{Kind: "node", UserUUID: pia, PersonUID: g["pia"], StrayUID: stray, Node: []byte(`{"uid":"` + stray + `"}`)}}
	}
	if err := userModels.BackUpMergeBatch(ctx, run, never, row(g2["stray"])); err != nil {
		t.Fatalf("a batch whose change never landed: %v", err)
	}
	if err := userModels.BackUpMergeBatch(ctx, run, landed, row(g2["gone"])); err != nil {
		t.Fatalf("a batch whose change landed: %v", err)
	}

	rep, err := userBusiness.MergeStrayPeople(ctx, false)
	if err != nil || rep.Strays != 1 {
		t.Fatalf("the next run: %+v (%v); want Pia's stray merged", rep, err)
	}
	states, err := pgexec.Pairs[string, string](ctx, `SELECT batch_id::text, string_agg(DISTINCT state, ',') FROM person_merge_backups
		WHERE run_id = $1 GROUP BY batch_id`, run)
	if err != nil {
		t.Fatalf("the backups: %v", err)
	}
	if want := map[string]string{landed.String(): "applied"}; !maps.Equal(states, want) {
		t.Errorf("the crashed run's batches after the next run: %v; want the landed one applied and the other gone %v", states, want)
	}
}

// A batch Dgraph aborts (a concurrent write to an edge it moves) is read,
// planned and backed up again under the same id. The retry's backup takes the
// place of the aborted try's, so a crash's settle and a restore read only the
// rows of the try that landed.
func TestARetriedBatchKeepsOnlyTheBackupOfItsLastTry(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("postgres: %v", err)
	}
	run, batch, pia := uuid.New(), uuid.New(), uuid.NewString()
	aborted := []userModels.MergeBackup{
		{Kind: "node", UserUUID: pia, PersonUID: "0x1", StrayUID: "0x2", Node: []byte(`{"uid":"0x2"}`)},
		{Kind: "in_edge", UserUUID: pia, PersonUID: "0x1", StrayUID: "0x2", Predicate: "team_members", OtherUID: "0x9", List: true, Added: true},
	}
	retried := []userModels.MergeBackup{{Kind: "node", UserUUID: pia, PersonUID: "0x1", StrayUID: "0x3", Node: []byte(`{"uid":"0x3"}`)}}
	for _, rows := range [][]userModels.MergeBackup{aborted, retried} {
		if err := userModels.BackUpMergeBatch(ctx, run, batch, rows); err != nil {
			t.Fatalf("backing up the batch: %v", err)
		}
	}
	pending, err := userModels.PendingMergeBatches(ctx)
	if want := []userModels.PendingMergeBatch{{Run: run, Batch: batch, Strays: []string{"0x3"}}}; err != nil || !reflect.DeepEqual(pending, want) {
		t.Errorf("the pending batches after a retry: %+v (%v); want only the retry's stray %+v", pending, err, want)
	}
	if err := userModels.MarkMergeBatch(ctx, run, batch, "pending", "applied"); err != nil {
		t.Fatalf("marking the batch applied: %v", err)
	}
	_, rows, err := userModels.AppliedMergeBackups(ctx, run)
	var got []string
	for _, r := range rows[batch] {
		got = append(got, r.Kind+" "+r.StrayUID)
	}
	if want := []string{"node 0x3"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("what a restore reads after a retry: %q (%v); want only the retry's rows %q", got, err, want)
	}
}
