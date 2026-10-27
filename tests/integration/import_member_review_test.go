//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	importBusiness "github.com/akashc777/OneCamp/business/Import"
	reviewBusiness "github.com/akashc777/OneCamp/business/ImportReview"
	importController "github.com/akashc777/OneCamp/controllers/Import"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// Old Todoist and Notion imports made every person they matched a member of
// every project they made. Once per install the review records each such
// person still in each such project (never the import's runner or the
// project's creator, never a Trello import's, never an import first run after
// the importers were fixed), tells the admins once, and takes nobody off: only
// an admin does, in the app.
func TestOldImportsMembersAreReviewedByAdmins(t *testing.T) {
	t.Setenv("JWT_SECRET", "import-member-review-secret")
	t.Setenv("ALLOWED_USERS", "")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	names := []string{"admin", "member", "runner", "maker", "ann", "ben", "cal", "dan", "eve"}
	ids := map[string]uuid.UUID{}
	var nodes []map[string]any
	for _, n := range names {
		ids[n] = uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, NOW(), NOW())`, ids[n], n+"@example.com", n, n); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + n, "dgraph.type": "User", "user_uuid": ids[n].String(), "user_name": n})
	}
	// The admin role is a row in admin_users, by address, as the workspace
	// grants it: users.is_admin is not what sign-in or the admins' list reads.
	if _, err := env.PG.Exec(`INSERT INTO admin_users (email_id) SELECT email_id FROM users WHERE id = $1`, ids["admin"]); err != nil {
		t.Fatalf("make the admin: %v", err)
	}
	uids := dg.Mutate(t, nodes)
	project := func(name string, members ...string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO projects (id, project_name, created_by) VALUES ($1, $2, $3)`, id, name, ids["maker"]); err != nil {
			t.Fatal(err)
		}
		ms := []map[string]any{}
		for _, m := range members {
			ms = append(ms, map[string]any{"uid": uids[m]})
		}
		dg.Mutate(t, map[string]any{"uid": "_:p", "dgraph.type": "Project", "project_uuid": id.String(), "project_name": name,
			"project_members": ms, "project_created_by": map[string]any{"uid": uids["maker"]}})
		return id
	}
	launch := project("Launch", "ann", "ben", "runner", "maker")
	notes := project("Notes") // Ann has left it since
	board := project("Board", "ann")
	cleanup := project("Cleanup", "cal", "dan")
	job := func(provider string, firstRun time.Time, projects []uuid.UUID, people ...string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := env.PG.QueryRow(`INSERT INTO import_jobs (provider, source_workspace_name, source, triggered_by, started_at)
			VALUES ($1, 'Old workspace', 'api', $2, $3) RETURNING id`, provider, ids["runner"], firstRun).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for i, p := range projects {
			if _, err := env.PG.Exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid) VALUES ($1, 'project', $2, $3)`, id, "p"+string(rune('0'+i)), p); err != nil {
				t.Fatal(err)
			}
		}
		for _, n := range people {
			if _, err := env.PG.Exec(`INSERT INTO import_id_map (import_id, entity_type, source_id, onecamp_uuid) VALUES ($1, 'user', $2, $3)`, id, n, ids[n]); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	monthAgo := time.Now().AddDate(0, -1, 0)
	todoistJob := job("todoist", monthAgo, []uuid.UUID{launch}, "ann", "ben", "runner", "maker")
	job("notion", monthAgo, []uuid.UUID{notes, cleanup}, "ann", "cal", "dan")
	job("trello", monthAgo, []uuid.UUID{board}, "ann")
	// The importers are fixed from here on: a Todoist import first run after it
	// made Garden's members the people Garden is shared with, Eve among them.
	if err := importBusiness.ClaimImportFixes(ctx); err != nil {
		t.Fatalf("the fixed server's claim: %v", err)
	}
	fixedAt, claimed, err := importBusiness.FixedAt(importBusiness.MembershipFixed)
	if err != nil || !claimed {
		t.Fatalf("the claim's record: %v %v", claimed, err)
	}
	garden := project("Garden", "eve", "maker")
	job("todoist", fixedAt.Add(time.Second), []uuid.UUID{garden}, "eve", "maker")

	in := func(p uuid.UUID, who ...string) map[string]bool {
		t.Helper()
		var people []string
		for _, w := range who {
			people = append(people, ids[w].String())
		}
		got, err := projectDomain.MembersAmong(ctx, p.String(), people)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, w := range who {
			out[w] = got[ids[w].String()]
		}
		return out
	}
	notices := func() int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM resource_notices WHERE kind = $1`, reviewBusiness.NoticeEvent).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	added, ran, err := reviewBusiness.Scan(ctx)
	if err != nil || !ran || added != 4 {
		t.Fatalf("the scan: %d added, ran %v, %v; want Ann and Ben in Launch, Cal and Dan in Cleanup", added, ran, err)
	}
	if got := in(launch, "ann", "ben"); !got["ann"] || !got["ben"] {
		t.Errorf("the scan took someone off Launch: %v", got)
	}
	var gardenReviews int
	if err := env.PG.QueryRow(`SELECT COUNT(*) FROM import_member_reviews WHERE project_uuid = $1`, garden).Scan(&gardenReviews); err != nil || gardenReviews != 0 {
		t.Errorf("Garden has %d reviews (%v): a fixed import's people are never under review", gardenReviews, err)
	}
	if n := notices(); n != 1 {
		t.Errorf("%d notices, want one, to the one admin", n)
	}
	if _, ran, err := reviewBusiness.Scan(ctx); ran || err != nil {
		t.Errorf("a second scan ran (%v, %v): it runs once per install", ran, err)
	}
	if err := reviewBusiness.TellAdmins(ctx); err != nil || notices() != 1 {
		t.Errorf("telling again: %v, %d notices, want still one", err, notices())
	}

	r := chi.NewRouter()
	r.Use(customMiddleware.VerifyAuth)
	r.Use(customMiddleware.VerifyAdminAuthOnlyPostgres)
	r.Get("/admin/import-reviews", importController.ListImportReviews)
	r.Get("/admin/import-reviews/projects/{project_uuid}", importController.ListImportReviewPeople)
	r.Post("/admin/import-reviews/{id}/remove", importController.RemoveImportReview)
	r.Post("/admin/import-reviews/projects/{project_uuid}/remove-all", importController.RemoveAllImportReviews)
	r.Post("/admin/import-reviews/projects/{project_uuid}/keep", importController.KeepImportReviews)
	call := func(as, method, path string) (int, []byte) {
		t.Helper()
		token, err := helpers.SignSessionToken(ids[as].String(), helpers.TokenTypeAccess, time.Now().Add(5*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}

	if code, _ := call("member", http.MethodGet, "/admin/import-reviews"); code != http.StatusForbidden {
		t.Errorf("a member listing the reviews: %d, want 403", code)
	}
	code, body := call("admin", http.MethodGet, "/admin/import-reviews")
	var listed struct {
		Pending  int `json:"pending"`
		Projects []struct {
			ProjectUUID string   `json:"project_uuid"`
			Providers   []string `json:"providers"`
			Undecided   int      `json:"undecided"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(body, &listed); code != http.StatusOK || err != nil || listed.Pending != 4 || len(listed.Projects) != 2 {
		t.Fatalf("the admin's list: %d, %v, %d pending in %d projects; want 4 in Launch and Cleanup", code, err, listed.Pending, len(listed.Projects))
	}
	for _, p := range listed.Projects {
		if p.Undecided != 2 {
			t.Errorf("project %s lists %d undecided, want 2", p.ProjectUUID, p.Undecided)
		}
	}
	type person struct {
		ID       string `json:"id"`
		UserUUID string `json:"user_uuid"`
		Outcome  string `json:"outcome"`
	}
	peopleOf := func(project uuid.UUID) map[string]person {
		t.Helper()
		code, body := call("admin", http.MethodGet, "/admin/import-reviews/projects/"+project.String())
		var page struct {
			People []person `json:"people"`
			More   bool     `json:"more"`
		}
		if err := json.Unmarshal(body, &page); code != http.StatusOK || err != nil || page.More {
			t.Fatalf("the admin's page of %s: %d, %v, more %v", project, code, err, page.More)
		}
		out := map[string]person{}
		for _, pp := range page.People {
			for n, id := range ids {
				if pp.UserUUID == id.String() {
					out[n] = pp
				}
			}
		}
		return out
	}
	if code, _ := call("member", http.MethodGet, "/admin/import-reviews/projects/"+launch.String()); code != http.StatusForbidden {
		t.Errorf("a member reading Launch's people: %d, want 403", code)
	}
	inLaunch := peopleOf(launch)
	if len(inLaunch) != 2 || inLaunch["ann"].ID == "" || inLaunch["ben"].ID == "" {
		t.Fatalf("Launch's people: %v, want Ann and Ben", inLaunch)
	}
	annInLaunch := inLaunch["ann"].ID
	if code, _ := call("member", http.MethodPost, "/admin/import-reviews/"+annInLaunch+"/remove"); code != http.StatusForbidden {
		t.Errorf("a member taking Ann off: %d, want 403", code)
	}

	// Through a token, even an admin's, nobody is taken off.
	admin := userModel.UserInfo{}
	admin.UserPostgresInfo.Id, admin.UserPostgresInfo.IsAdmin, admin.UserDgraphInfo.Uid = ids["admin"], true, uids["admin"]
	tokenReq := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(
		helpers.WithActsThroughToken(context.WithValue(ctx, helpers.UserInfoContextKey, admin)))
	if err := reviewBusiness.Remove(tokenReq, uuid.MustParse(annInLaunch)); !errors.Is(err, reviewBusiness.ErrThroughToken) {
		t.Errorf("an admin's token taking Ann off: %v, want ErrThroughToken", err)
	}
	if !in(launch, "ann")["ann"] {
		t.Fatal("Ann was taken off Launch through a token")
	}

	if code, _ := call("admin", http.MethodPost, "/admin/import-reviews/"+annInLaunch+"/remove"); code != http.StatusOK {
		t.Fatalf("the admin taking Ann off: %d", code)
	}
	if in(launch, "ann")["ann"] {
		t.Error("Ann is still in Launch")
	}
	// The audit chain writes its rows after the request, in order: wait for it.
	audited(t, env.PG, "import.review.member_removed", 1)
	var actor uuid.UUID
	if err := env.PG.QueryRow(`SELECT actor_id FROM admin_audit_log WHERE action = 'import.review.member_removed'`).Scan(&actor); err != nil || actor != ids["admin"] {
		t.Errorf("taking Ann off was recorded as %v (%v), want the admin who did it", actor, err)
	}
	if code, _ := call("admin", http.MethodPost, "/admin/import-reviews/"+annInLaunch+"/remove"); code != http.StatusConflict {
		t.Errorf("deciding Ann again: %d, want 409", code)
	}
	if code, _ := call("admin", http.MethodPost, "/admin/import-reviews/projects/"+launch.String()+"/keep"); code != http.StatusOK {
		t.Errorf("keeping Launch's others: %d", code)
	}
	if !in(launch, "ben")["ben"] {
		t.Error("keeping took Ben off Launch")
	}
	if code, _ := call("admin", http.MethodPost, "/admin/import-reviews/projects/"+cleanup.String()+"/remove-all"); code != http.StatusOK {
		t.Errorf("taking everyone off Cleanup: %d", code)
	}
	if got := in(cleanup, "cal", "dan"); got["cal"] || got["dan"] {
		t.Errorf("after remove-all, Cleanup still has %v", got)
	}
	if code, body := call("admin", http.MethodGet, "/admin/import-reviews"); code != http.StatusOK || json.Unmarshal(body, &listed) != nil || listed.Pending != 0 {
		t.Errorf("after deciding all: %d, %d pending, want 0", code, listed.Pending)
	}

	// Should a review ever name a project's creator, they stay in it: the
	// project's own rule, and nothing is claimed for them.
	if _, err := reviewModel.Add(ctx, []reviewModel.Candidate{{ImportID: todoistJob, Provider: "todoist", ProjectUUID: launch, UserID: ids["maker"]}}); err != nil {
		t.Fatal(err)
	}
	maker := peopleOf(launch)["maker"]
	if code, _ := call("admin", http.MethodPost, "/admin/import-reviews/"+maker.ID+"/remove"); code != http.StatusForbidden {
		t.Errorf("taking Launch's creator off: %d, want 403", code)
	}
	if !in(launch, "maker")["maker"] {
		t.Error("Launch's creator was taken off it")
	}
	if got := peopleOf(launch)["maker"]; got.Outcome != "" {
		t.Errorf("the refused review was decided %q, want undecided", got.Outcome)
	}
}
