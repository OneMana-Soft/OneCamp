//go:build integration

package business

// Old Basecamp imports, run before Basecamp's fix, re-checked against
// Basecamp (recheckPeople.go, P-IMP-6D). Ada runs the workspace. Her import
// made Launch and Archive (archived here since) and everyone on them members:
// Ben, who joins projects at Basecamp; Cleo and Dan, Basecamp's clients; and
// Eve, whom Basecamp no longer lists. Dan has left Launch since. Cy's import
// made Ops, with Fay; Cy has no Basecamp connection. Ben and Dan had no
// account here, so the import made them, as it makes everyone it can't match
// by email, external placeholders. Our own records can't tell a client among
// them: every placeholder is external, and Basecamp's client flag was never
// recorded.
//   - The install recorded the re-check's reviews before Basecamp's came (the
//     candidates' earlier key); Basecamp's are recorded all the same. Cy's
//     import can't be re-checked, so everyone it added is listed for the
//     admins from the start, as a guess.
//   - Ada's connection reaches another Basecamp account at first: her import
//     goes to the admins, and everyone it added who is still in one of its
//     projects is listed, as a guess, placeholders too. Its tasks can't be
//     kept to the admins: it has none to keep.
//   - Ada keeps Cleo in Archive, then re-checks the import with her own
//     connection, which reaches its account: Cleo in Launch and Dan in
//     Archive are flagged as clients, Ada's choice about Cleo in Archive
//     stands, Eve stays a guess, and Ben's guess goes. Each project's review
//     is re-checked with its counts (Cleo is still a client in Archive, kept
//     by Ada), there is nothing to undo, and nobody has been taken off a
//     project.
// Run: PKGS=./business/Import/ integ_rel.sh <wt> <log> '^TestAnOldBasecampImportIsRecheckedAgainstBasecamp$'

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/google/uuid"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
)

// basecampNow is Basecamp as it has the imports' people now: Cleo and Dan are
// clients, Ben joins projects, and Eve is gone. Until Ada reconnects
// (another), her connection reaches another account, which has none of the
// import's projects and other people.
type basecampNow struct {
	importProvider.Provider
	another *bool
}

func (basecampNow) Name() string { return importModels.ProviderBasecamp }

func (b basecampNow) RecheckPeople(_ context.Context, _ uuid.UUID, _ *importModels.Job, projects []string) (importProvider.RecheckedPeople, error) {
	if *b.another {
		return importProvider.RecheckedPeople{KeptOut: map[string]bool{"u-zed": false}}, nil
	}
	answered := map[string]bool{}
	for _, p := range projects {
		answered[p] = p == "101" || p == "102"
	}
	return importProvider.RecheckedPeople{Answered: answered,
		KeptOut: map[string]bool{"u-ada": false, "u-ben": false, "u-cleo": true, "u-dan": true}}, nil
}

func TestAnOldBasecampImportIsRecheckedAgainstBasecamp(t *testing.T) {
	ctx := context.Background()
	world := seenWorld(t)
	env, dg := world.env, world.dg
	names := []string{"ada", "ben", "cleo", "dan", "eve", "cy", "fay"}
	placeholder := map[string]bool{"ben": true, "dan": true}
	people := map[string]uuid.UUID{}
	for _, n := range names {
		people[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, is_external, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, $3, NOW(), NOW())`, people[n], n, placeholder[n]); err != nil {
			t.Fatal(err)
		}
	}
	ada, cy := people["ada"], people["cy"]
	launch, archive, ops, team := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO admin_users (email_id) VALUES ('ada@acme.test')`, nil},
		{`INSERT INTO teams (id, team_name, created_by) VALUES ($1, 'Builders', $2)`, []any{team, ada}},
		{`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Launch', $2, $3)`, []any{launch, team, ada}},
		{`INSERT INTO projects (id, project_name, team_id, created_by, deleted_at) VALUES ($1, 'Archive', $2, $3, NOW())`, []any{archive, team, ada}},
		{`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Ops', $2, $3)`, []any{ops, team, cy}},
		{`INSERT INTO import_oauth_tokens (provider, owner_user_id, access_token_enc) VALUES ('basecamp', $1, '\x00')`, []any{ada}},
	} {
		if _, err := env.PG.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	live, gone := "0001-01-01T00:00:00Z", time.Now().UTC().Format(time.RFC3339)
	in := func(names ...string) []map[string]any {
		var out []map[string]any
		for _, n := range names {
			out = append(out, map[string]any{"uid": "_:" + n})
		}
		return out
	}
	project := func(key string, id uuid.UUID, name, deleted, maker string, members ...string) map[string]any {
		return map[string]any{"uid": "_:" + key, "dgraph.type": "Project", "project_uuid": id.String(), "project_name": name,
			"project_deleted_at": deleted, "project_members": in(members...), "project_admins": in(maker),
			"project_created_by": map[string]any{"uid": "_:" + maker}, "project_team": map[string]any{"uid": "_:builders"}}
	}
	nodes := []map[string]any{
		{"uid": "_:builders", "dgraph.type": "Team", "team_uuid": team.String(), "team_name": "Builders", "team_members": in(names...),
			"team_admins": in("ada"), "team_projects": in("launch", "archive", "ops")},
		project("launch", launch, "Launch", live, "ada", "ada", "ben", "cleo", "eve"),
		project("archive", archive, "Archive", gone, "ada", "ada", "cleo", "dan"),
		project("ops", ops, "Ops", live, "cy", "cy", "fay"),
	}
	for _, n := range names {
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": people[n].String(), "user_name": n,
			"user_deleted_at": live, "user_teams": in("builders")})
	}
	dg.Mutate(t, nodes)

	importOf := func(by uuid.UUID, projects map[string]uuid.UUID, who ...string) *importModels.Job {
		t.Helper()
		job := &importModels.Job{Id: uuid.New(), Provider: importModels.ProviderBasecamp, SourceWorkspaceName: "Acme", Source: "api",
			Status: importModels.StatusRunning, TriggeredBy: &by}
		if err := importModels.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if _, err := env.PG.Exec(`UPDATE import_jobs SET status = 'completed', started_at = NOW() - interval '3 hours', created_at = NOW() - interval '3 hours' WHERE id = $1`, job.Id); err != nil {
			t.Fatal(err)
		}
		for _, n := range who {
			matched := "email"
			if placeholder[n] {
				matched = "external"
			}
			if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityUser, "u-"+n, people[n], nil,
				mustMarshal(map[string]any{"matched_by": matched}), placeholder[n]); err != nil {
				t.Fatal(err)
			}
		}
		for source, id := range projects {
			if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityProject, source, id, nil, mustMarshal(map[string]any{}), true); err != nil {
				t.Fatal(err)
			}
		}
		return job
	}
	job := importOf(ada, map[string]uuid.UUID{"101": launch, "102": archive}, "ada", "ben", "cleo", "dan", "eve")
	opsJob := importOf(cy, map[string]uuid.UUID{"301": ops}, "cy", "fay")

	// The install ran the re-check's candidates before Basecamp's came.
	if err := configModel.UpsertConfig(recheckCandidatesKey, "done: 0 reviews of what old imports let every project member read"); err != nil {
		t.Fatal(err)
	}
	if err := ClaimImportFixes(ctx); err != nil {
		t.Fatal(err)
	}
	another := true
	if prev := importProvider.Get(importModels.ProviderBasecamp); prev != nil {
		t.Cleanup(func() { importProvider.Register(prev) })
	}
	importProvider.Register(basecampNow{another: &another})

	byName := map[uuid.UUID]string{launch: "Launch", archive: "Archive", ops: "Ops"}
	for n, id := range people {
		byName[id] = n
	}
	reviews := func(j *importModels.Job) map[string]string {
		t.Helper()
		revs, err := reviewModel.AccessReviewsOf(ctx, j.Id)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range revs {
			c := countsOf(&r)
			out[byName[r.ProjectUUID]+" "+r.Gap] = r.State + " " + c.Reason
		}
		return out
	}
	listed := func() map[string]string {
		t.Helper()
		rows, err := env.PG.Query(`SELECT project_uuid, user_id, COALESCE(reason, ''), COALESCE(outcome, '') FROM import_member_reviews`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var p, u uuid.UUID
			var reason, outcome string
			if err := rows.Scan(&p, &u, &reason, &outcome); err != nil {
				t.Fatal(err)
			}
			out[byName[p]+" "+byName[u]] = reason + "/" + outcome
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if _, _, err := RecheckCandidates.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := reviews(job), map[string]string{"Launch basecamp_clients": "rechecking ", "Archive basecamp_clients": "rechecking "}; !maps.Equal(got, want) {
		t.Fatalf("Ada's import's reviews: %v; want %v", got, want)
	}
	if got, want := reviews(opsJob), map[string]string{"Ops basecamp_clients": "needs_admin no_connection"}; !maps.Equal(got, want) {
		t.Errorf("Cy's import's reviews: %v; want %v", got, want)
	}
	if got, want := listed(), map[string]string{"Ops fay": "/"}; !maps.Equal(got, want) {
		t.Errorf("listed for the admins after the candidates: %v; want %v", got, want)
	}

	// Ada's connection reaches another account: her import goes to the admins,
	// everyone it added who is still in one of its projects listed.
	if rep, err := recheckPending(ctx); err != nil {
		t.Fatalf("the first pass: %v (%s)", err, rep)
	}
	waiting := map[string]string{"Launch basecamp_clients": "needs_admin other_source", "Archive basecamp_clients": "needs_admin other_source"}
	if got := reviews(job); !maps.Equal(got, waiting) {
		t.Errorf("Ada's import after a pass that reached another account: %v; want %v", got, waiting)
	}
	if got, want := listed(), map[string]string{"Launch ben": "/", "Launch cleo": "/", "Launch eve": "/", "Archive cleo": "/", "Archive dan": "/",
		"Ops fay": "/"}; !maps.Equal(got, want) {
		t.Errorf("listed for the admins when the import can't be re-checked: %v; want %v", got, want)
	}
	if err := ContainImport(ctx, job.Id, ada); !errors.Is(err, ErrNotRecheckable) {
		t.Errorf("keeping the tasks of an import of people: %v; want ErrNotRecheckable", err)
	}

	// Ada keeps Cleo in Archive, then re-checks with her own connection.
	var cleoInArchive uuid.UUID
	if err := env.PG.QueryRow(`SELECT id FROM import_member_reviews WHERE project_uuid = $1 AND user_id = $2`, archive, people["cleo"]).Scan(&cleoInArchive); err != nil {
		t.Fatal(err)
	}
	if won, err := reviewModel.Decide(ctx, cleoInArchive, reviewModel.OutcomeKept, ada); err != nil || !won {
		t.Fatalf("Ada keeping Cleo in Archive: %v, %v", won, err)
	}
	another = false
	if err := RecheckWithConnection(ctx, job.Id, ada); err != nil {
		t.Fatalf("Ada re-checking with her connection: %v", err)
	}
	settled := map[string]string{"Launch basecamp_clients": "rechecked ", "Archive basecamp_clients": "rechecked "}
	for deadline := time.Now().Add(30 * time.Second); !maps.Equal(reviews(job), settled); time.Sleep(250 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waited 30 s for the re-check: %v", reviews(job))
		}
	}
	if got, want := listed(), map[string]string{"Launch cleo": reviewModel.ReasonBasecampClient + "/", "Launch eve": "/",
		"Archive cleo": "/" + reviewModel.OutcomeKept, "Archive dan": reviewModel.ReasonBasecampClient + "/", "Ops fay": "/"}; !maps.Equal(got, want) {
		t.Errorf("listed for the admins after the re-check: %v; want %v", got, want)
	}
	revs, err := reviewModel.AccessReviewsOf(ctx, job.Id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range revs {
		c := countsOf(&r)
		want := map[uuid.UUID][2]int{launch: {1, 1}, archive: {2, 0}}[r.ProjectUUID]
		if c.Clients != want[0] || c.PeopleUnlisted != want[1] || c.CheckedBy == nil || *c.CheckedBy != ada {
			t.Errorf("%s's counts: %+v; want %d clients and %d no longer listed, checked by Ada", byName[r.ProjectUUID], c, want[0], want[1])
		}
	}
	if err := UndoRecheck(ctx, job.Id, ada); !errors.Is(err, ErrNotRecheckable) {
		t.Errorf("undoing the re-check of an import of people: %v; want ErrNotRecheckable", err)
	}
	for p, who := range map[uuid.UUID][]string{launch: {"ben", "cleo", "eve"}, archive: {"cleo", "dan"}, ops: {"fay"}} {
		ids := make([]string, len(who))
		for i, n := range who {
			ids[i] = people[n].String()
		}
		stays, err := projectBusiness.MembersAmong(ctx, p.String(), ids)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if !stays[id] {
				t.Errorf("%s was taken off %s; only an admin takes anyone off", byName[uuid.MustParse(id)], byName[p])
			}
		}
	}
}
