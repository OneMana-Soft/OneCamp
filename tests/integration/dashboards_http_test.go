//go:build integration
// +build integration

package integration_test

// Dashboards over HTTP (B9-b2: jira-08, asana-08, linear-32), through the
// app's own router (router.Routes(), its middleware included), against real
// Postgres and Dgraph, in seedWorkWorld's Website project. Each case of the
// permission model (business/Dashboard) is one block here:
//  1. mine: only its owner lists, reads and changes it; anyone else finds
//     nothing (404, never 403: no oracle); it is never shared;
//  2. project: its admins keep its dashboards (ben, a member, is refused 403;
//     someone outside it finds nothing, 404, the team's admin included);
//     its members, commenters and viewers list and read one only once it is
//     shared; an archived project's are read-only (403 on every change);
//  3. workspace: its admins keep them; every member reads one once shared;
//  4. a guest's link is no session: every dashboards route answers 401;
//  5. the data is the reader's: a shared widget on a project the reader
//     isn't in reads empty (counts 0, no names), never its maker's numbers.
//
// Deleting a dashboard or a widget is soft, and either comes back.
//
// Run: PKGS=./tests/integration/ (go test -tags=integration ./tests/integration/ -run '^TestDashboardsOverHTTP$' -v)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	roleModel "github.com/akashc777/OneCamp/models/postgres/ProjectRole"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestDashboardsOverHTTP(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	integration.SetupRedis(t)
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	t.Cleanup(firebaseInit.ObservePushesForTest(func(map[string]string, []string) {}))

	t.Setenv("CSRF_DISABLED", "1")
	t.Setenv("JWT_SECRET", "dashboards-integration-secret")
	routes := router.Routes()
	// The report reads a person's projects through user_projects, which
	// adding a member writes beside project_members: seedWorkWorld writes
	// only the latter, so its members get the former here.
	var seeded struct {
		P []struct {
			UID     string `json:"uid"`
			Members []struct {
				UID string `json:"uid"`
			} `json:"project_members"`
		} `json:"p"`
	}
	if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ p(func: eq(project_uuid, %q)) { uid project_members { uid } } }`, w.project)), &seeded); err != nil || len(seeded.P) != 1 {
		t.Fatalf("the seeded project: %v %+v", err, seeded)
	}
	for _, m := range seeded.P[0].Members {
		w.dg.Mutate(t, map[string]any{"uid": m.UID, "user_projects": []map[string]any{{"uid": seeded.P[0].UID}}})
	}
	send := func(cookie, method, path string, body any) (int, map[string]any, string) {
		t.Helper()
		var rd io.Reader = http.NoBody
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, rd)
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "Authorization", Value: cookie})
			req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
		}
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Body.String()
	}
	session := func(who string) string {
		t.Helper()
		s, err := helpers.SignSessionToken(w.people[who].String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	call := func(who, method, path string, body any) (int, map[string]any) {
		t.Helper()
		code, out, _ := send(session(who), method, path, body)
		return code, out
	}
	expect := func(what string, code int, out map[string]any, want int) map[string]any {
		t.Helper()
		if code != want {
			t.Fatalf("%s: %d %v, want %d", what, code, out, want)
		}
		data, _ := out["data"].(map[string]any)
		return data
	}
	// listed is the names of the dashboards a list answers under key.
	listed := func(who, path, key string) map[string]string {
		t.Helper()
		code, out := call(who, http.MethodGet, path, nil)
		data := expect(who+" lists "+path, code, out, http.StatusOK)
		got := map[string]string{}
		list, _ := data[key].([]any)
		for _, x := range list {
			m, _ := x.(map[string]any)
			got[fmt.Sprint(m["name"])] = fmt.Sprint(m["id"])
		}
		return got
	}
	create := func(who, path string, body map[string]any) string {
		t.Helper()
		code, out := call(who, http.MethodPost, path, body)
		d := expect(who+" makes a dashboard at "+path, code, out, http.StatusOK)
		id, _ := d["id"].(string)
		if id == "" {
			t.Fatalf("%s made %v", who, d)
		}
		return id
	}
	widget := func(who, dash string, body map[string]any) string {
		t.Helper()
		code, out := call(who, http.MethodPost, "/project/dashboards/"+dash+"/widgets", body)
		d := expect(who+" adds a widget", code, out, http.StatusOK)
		id, _ := d["id"].(string)
		if id == "" {
			t.Fatalf("%s added %v", who, d)
		}
		return id
	}
	// read is a dashboard's widgets worked out for who, by widget id.
	read := func(who, dash string) map[string]map[string]any {
		t.Helper()
		code, out := call(who, http.MethodGet, "/project/dashboards/"+dash+"/data?tz=Asia/Kolkata", nil)
		d := expect(who+" reads "+dash+"'s data", code, out, http.StatusOK)
		got := map[string]map[string]any{}
		list, _ := d["widgets"].([]any)
		for _, x := range list {
			m, _ := x.(map[string]any)
			if e, _ := m["error"].(string); e != "" {
				t.Fatalf("%s reads widget %v: error %q", who, m["id"], e)
			}
			data, _ := m["data"].(map[string]any)
			got[fmt.Sprint(m["id"])] = data
		}
		return got
	}
	value := func(data map[string]any) float64 {
		v, _ := data["value"].(float64)
		return v
	}
	mine := "/project/dashboards"
	project := "/project/" + w.project + "/dashboards"
	// placed is where each of a dashboard's widgets is, as who reads it.
	placed := func(who, dash string) map[string]string {
		t.Helper()
		code, out := call(who, http.MethodGet, mine+"/"+dash, nil)
		d := expect(who+" reads "+dash, code, out, http.StatusOK)
		got := map[string]string{}
		list, _ := d["widgets"].([]any)
		for _, x := range list {
			m, _ := x.(map[string]any)
			got[fmt.Sprint(m["id"])] = fmt.Sprint(m["x"], ",", m["y"], ",", m["w"], ",", m["h"])
		}
		return got
	}

	// 4. A guest's link is no session: the sign-in refuses it, as it refuses
	// no cookie at all, before any dashboard is read.
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	link, err := guestBusiness.CreateResourceGrant(ctx, w.people["ana"], guestModel.ResourceProject, w.project, guestModel.CapabilityComment, 0, false)
	if err != nil {
		t.Fatalf("a guest link: %v", err)
	}
	someID := uuid.NewString()
	for _, cookie := range []string{link.Token, ""} {
		for _, r := range [][2]string{{http.MethodGet, mine}, {http.MethodPost, mine}, {http.MethodGet, project}, {http.MethodPost, project},
			{http.MethodGet, mine + "/" + someID}, {http.MethodGet, mine + "/" + someID + "/data"}, {http.MethodPost, mine + "/" + someID + "/widgets"}} {
			if code, out, _ := send(cookie, r[0], r[1], map[string]any{"name": "Guest's"}); code != http.StatusUnauthorized {
				t.Fatalf("a guest's link (%q) %s %s: %d %v, want 401", cookie, r[0], r[1], code, out)
			}
		}
	}

	// 1. mine: ben keeps his own, with a widget on the project he works in.
	bens := create("ben", mine, map[string]any{"name": "My week", "scope": "mine"})
	open := widget("ben", bens, map[string]any{"kind": "metric", "title": "Open", "query": map[string]any{"projects": []string{w.project}, "measure": "open"},
		"x": 0, "y": 0, "w": 3, "h": 2})
	if got := listed("ben", mine, "mine"); got["My week"] != bens {
		t.Fatalf("ben's own list: %v", got)
	}
	if v := value(read("ben", bens)[open]); v < 1 {
		t.Fatalf("ben's open work in the project reads %v, want at least 1", v)
	}
	for _, who := range []string{"ana", "cy", "wes"} {
		if got := listed(who, mine, "mine"); got["My week"] != "" {
			t.Fatalf("%s lists ben's own dashboard: %v", who, got)
		}
		code, out := call(who, http.MethodGet, mine+"/"+bens, nil)
		expect(who+" reads ben's own", code, out, http.StatusNotFound)
		code, out = call(who, http.MethodGet, mine+"/"+bens+"/data", nil)
		expect(who+" reads ben's own data", code, out, http.StatusNotFound)
		code, out = call(who, http.MethodPost, mine+"/"+bens, map[string]any{"name": "Taken"})
		expect(who+" renames ben's own", code, out, http.StatusNotFound)
		code, out = call(who, http.MethodPost, mine+"/"+bens+"/widgets", map[string]any{"kind": "throughput", "x": 0, "y": 2, "w": 6, "h": 3})
		expect(who+" adds to ben's own", code, out, http.StatusNotFound)
	}
	code, out := call("ben", http.MethodPost, mine+"/"+bens, map[string]any{"name": "My week", "shared": true})
	expect("ben shares his own", code, out, http.StatusBadRequest)

	// 2. project: ben, its member, can't keep one; eve, outside it, and dee,
	// the team's admin outside it, find nothing.
	code, out = call("ben", http.MethodPost, project, map[string]any{"name": "Ben's"})
	expect("ben makes one for the project", code, out, http.StatusForbidden)
	for _, who := range []string{"eve", "dee", "cy"} {
		code, out = call(who, http.MethodGet, project, nil)
		expect(who+" lists the project's", code, out, http.StatusNotFound)
		code, out = call(who, http.MethodPost, project, map[string]any{"name": "Theirs"})
		expect(who+" makes one for the project", code, out, http.StatusNotFound)
	}
	// ana, its admin, keeps one; a widget there is about the project alone.
	health := create("ana", project, map[string]any{"name": "Sprint health"})
	throughput := widget("ana", health, map[string]any{"kind": "throughput", "query": map[string]any{"weeks": 4}, "x": 0, "y": 0, "w": 6, "h": 3})
	people := widget("ana", health, map[string]any{"kind": "breakdown", "query": map[string]any{"by": "person"}, "x": 6, "y": 0, "w": 6, "h": 3})
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/widgets", map[string]any{"kind": "metric", "query": map[string]any{"projects": []string{uuid.NewString()}, "measure": "open"},
		"x": 0, "y": 3, "w": 3, "h": 2})
	expect("ana adds a widget on another project to the project's", code, out, http.StatusBadRequest)
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/widgets", map[string]any{"kind": "metric", "query": map[string]any{"measure": "open"}, "x": 10, "y": 3, "w": 4, "h": 2})
	expect("ana adds a widget off the grid", code, out, http.StatusBadRequest)
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/widgets", map[string]any{"kind": "pie", "x": 0, "y": 3, "w": 3, "h": 2})
	expect("ana adds a kind there isn't", code, out, http.StatusBadRequest)
	if got := listed("ana", project, "dashboards"); got["Sprint health"] != health {
		t.Fatalf("ana's list of the project's: %v", got)
	}
	// Not shared yet: ben neither lists nor reads it.
	if got := listed("ben", project, "dashboards"); got["Sprint health"] != "" {
		t.Fatalf("ben lists the project's unshared dashboard: %v", got)
	}
	code, out = call("ben", http.MethodGet, mine+"/"+health, nil)
	expect("ben reads the project's unshared dashboard", code, out, http.StatusNotFound)
	// Shared: ben lists and reads it, as himself, and still can't change it.
	code, out = call("ana", http.MethodPost, mine+"/"+health, map[string]any{"name": "Sprint health", "shared": true})
	expect("ana shares it", code, out, http.StatusOK)
	if got := listed("ben", project, "dashboards"); got["Sprint health"] != health {
		t.Fatalf("ben's list of the project's, once shared: %v", got)
	}
	code, out = call("ben", http.MethodGet, mine+"/"+health, nil)
	d := expect("ben reads it", code, out, http.StatusOK)
	if ws, _ := d["widgets"].([]any); len(ws) != 2 || d["can_keep"] != false {
		t.Fatalf("ben reads %v", d)
	}
	data := read("ben", health)
	if weeks, _ := data[throughput]["weeks"].([]any); len(weeks) != 4 {
		t.Fatalf("the throughput widget's weeks: %v", data[throughput])
	}
	if rows, _ := data[people]["rows"].([]any); len(rows) == 0 {
		t.Fatalf("the people widget reads no one for ben: %v", data[people])
	}
	code, out = call("ben", http.MethodPost, mine+"/"+health, map[string]any{"name": "Mine now"})
	expect("ben renames it", code, out, http.StatusForbidden)
	code, out = call("ben", http.MethodPost, mine+"/"+health+"/widgets/"+people+"/delete", nil)
	expect("ben removes a widget", code, out, http.StatusForbidden)
	for _, who := range []string{"eve", "dee", "cy"} {
		code, out = call(who, http.MethodGet, mine+"/"+health, nil)
		expect(who+" reads the project's shared dashboard", code, out, http.StatusNotFound)
		code, out = call(who, http.MethodGet, mine+"/"+health+"/data", nil)
		expect(who+" reads its data", code, out, http.StatusNotFound)
	}
	// ana arranges it, and a place off the grid is refused.
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/arrange", map[string]any{"places": []map[string]any{
		{"id": people, "x": 0, "y": 0, "w": 12, "h": 3}, {"id": throughput, "x": 0, "y": 3, "w": 12, "h": 4}}})
	expect("ana arranges it", code, out, http.StatusOK)
	code, out = call("ana", http.MethodGet, mine+"/"+health, nil)
	d = expect("ana reads it", code, out, http.StatusOK)
	if ws, _ := d["widgets"].([]any); len(ws) != 2 || fmt.Sprint(ws[0].(map[string]any)["id"]) != people || d["can_keep"] != true {
		t.Fatalf("arranged: %v", d)
	}
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/arrange", map[string]any{"places": []map[string]any{{"id": people, "x": 6, "y": 0, "w": 12, "h": 3}}})
	expect("ana arranges a widget off the grid", code, out, http.StatusBadRequest)
	// Arranging is all or none: a place for a widget that isn't the
	// dashboard's (ben's own) refuses the whole arrangement, and nothing moves.
	arranged, bensPlaces := placed("ana", health), placed("ben", bens)
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/arrange", map[string]any{"places": []map[string]any{
		{"id": people, "x": 0, "y": 7, "w": 6, "h": 3}, {"id": open, "x": 6, "y": 7, "w": 6, "h": 3}}})
	expect("ana arranges with ben's widget", code, out, http.StatusNotFound)
	if got := placed("ana", health); fmt.Sprint(got) != fmt.Sprint(arranged) {
		t.Fatalf("a refused arrangement moved Sprint health's widgets from %v to %v", arranged, got)
	}
	if got := placed("ben", bens); fmt.Sprint(got) != fmt.Sprint(bensPlaces) {
		t.Fatalf("a refused arrangement moved ben's widget from %v to %v", bensPlaces, got)
	}
	// Removing a widget is soft, and it comes back.
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/widgets/"+people+"/delete", nil)
	expect("ana removes a widget", code, out, http.StatusOK)
	if _, has := read("ana", health)[people]; has {
		t.Fatalf("a removed widget is still read")
	}
	// A removed widget can't be placed either: the arrangement is refused
	// whole, and the live widget beside it stays where it was.
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/arrange", map[string]any{"places": []map[string]any{
		{"id": throughput, "x": 0, "y": 7, "w": 6, "h": 3}, {"id": people, "x": 6, "y": 7, "w": 6, "h": 3}}})
	expect("ana arranges with a removed widget", code, out, http.StatusNotFound)
	if got := placed("ana", health)[throughput]; got != arranged[throughput] {
		t.Fatalf("a refused arrangement moved the throughput widget from %s to %s", arranged[throughput], got)
	}
	// A widget is its own dashboard's alone: ben, keeping his own, finds
	// Sprint health's nowhere through it, to restore, change or remove.
	code, out = call("ben", http.MethodPost, mine+"/"+bens+"/widgets/"+people+"/restore", nil)
	expect("ben restores Sprint health's widget through his own", code, out, http.StatusNotFound)
	if _, has := read("ana", health)[people]; has {
		t.Fatalf("ben's restore through his own dashboard brought Sprint health's widget back")
	}
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/widgets/"+people+"/restore", nil)
	expect("ana puts it back", code, out, http.StatusOK)
	if _, has := read("ana", health)[people]; !has {
		t.Fatalf("a restored widget isn't read")
	}
	restored := placed("ana", health)
	code, out = call("ben", http.MethodPost, mine+"/"+bens+"/widgets/"+people, map[string]any{"kind": "breakdown", "title": "Taken",
		"query": map[string]any{"by": "person"}, "x": 0, "y": 7, "w": 6, "h": 3})
	expect("ben changes Sprint health's widget through his own", code, out, http.StatusNotFound)
	code, out = call("ben", http.MethodPost, mine+"/"+bens+"/widgets/"+people+"/delete", nil)
	expect("ben removes Sprint health's widget through his own", code, out, http.StatusNotFound)
	if got := placed("ana", health); fmt.Sprint(got) != fmt.Sprint(restored) {
		t.Fatalf("ben's writes through his own dashboard reached Sprint health's widgets: %v, want %v", got, restored)
	}
	code, out = call("ana", http.MethodGet, mine+"/"+health, nil)
	for _, x := range expect("ana reads Sprint health", code, out, http.StatusOK)["widgets"].([]any) {
		if m, _ := x.(map[string]any); m["title"] == "Taken" {
			t.Fatalf("ben's change through his own dashboard renamed Sprint health's widget: %v", m)
		}
	}
	// A viewer reads the shared one and changes nothing.
	if err := roleModel.Set(ctx, uuid.MustParse(w.project), w.people["ben"], roleModel.Viewer, w.people["ana"]); err != nil {
		t.Fatal(err)
	}
	code, out = call("ben", http.MethodGet, mine+"/"+health+"/data", nil)
	expect("a viewer reads its data", code, out, http.StatusOK)
	code, out = call("ben", http.MethodPost, mine+"/"+health+"/widgets", map[string]any{"kind": "flow", "x": 0, "y": 7, "w": 6, "h": 3})
	expect("a viewer adds a widget", code, out, http.StatusForbidden)

	// 3. workspace: ana, no workspace admin, can't keep one; wes keeps one.
	code, out = call("ana", http.MethodPost, mine, map[string]any{"name": "Company", "scope": "workspace"})
	expect("ana makes one for the workspace", code, out, http.StatusForbidden)
	company := create("wes", mine, map[string]any{"name": "Company", "scope": "workspace"})
	if got := listed("ben", mine, "workspace"); got["Company"] != "" {
		t.Fatalf("ben lists the workspace's unshared dashboard: %v", got)
	}
	code, out = call("ben", http.MethodGet, mine+"/"+company, nil)
	expect("ben reads the workspace's unshared dashboard", code, out, http.StatusNotFound)
	code, out = call("wes", http.MethodPost, mine+"/"+company, map[string]any{"name": "Company", "shared": true})
	expect("wes shares it", code, out, http.StatusOK)
	if got := listed("cy", mine, "workspace"); got["Company"] != company {
		t.Fatalf("cy's list of the workspace's, once shared: %v", got)
	}
	code, out = call("cy", http.MethodPost, mine+"/"+company, map[string]any{"name": "Cy's"})
	expect("cy renames the workspace's", code, out, http.StatusForbidden)

	// 5. The data is the reader's. wes, a workspace admin, joins the
	// project (in the graph, as adding a member writes it) and puts its open
	// work and its projects on a shared workspace dashboard. cy and eve are
	// outside it but in a project of their own, Garden, with open work: a
	// widget on Website reads nothing for them, never their own Garden.
	graphUID := func(who string) string {
		t.Helper()
		var u struct {
			U []struct {
				UID string `json:"uid"`
			} `json:"u"`
		}
		if err := json.Unmarshal(w.dg.Query(t, fmt.Sprintf(`{ u(func: eq(user_uuid, %q)) { uid } }`, w.people[who].String())), &u); err != nil || len(u.U) != 1 {
			t.Fatalf("%s in the graph: %v %+v", who, err, u)
		}
		return u.U[0].UID
	}
	wes, cy, eve := graphUID("wes"), graphUID("cy"), graphUID("eve")
	w.dg.Mutate(t, map[string]any{"uid": seeded.P[0].UID, "project_members": []map[string]any{{"uid": wes}}})
	w.dg.Mutate(t, map[string]any{"uid": wes, "user_projects": []map[string]any{{"uid": seeded.P[0].UID}}})
	gardenID, weedID, live := uuid.NewString(), uuid.NewString(), "0001-01-01T00:00:00Z"
	w.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Garden', $2, $3)`, gardenID, w.team, w.people["cy"])
	w.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, weedID, gardenID, w.people["cy"])
	w.dg.Mutate(t, []map[string]any{
		{"uid": "_:garden", "dgraph.type": "Project", "project_uuid": gardenID, "project_name": "Garden", "project_deleted_at": live,
			"project_members": []map[string]any{{"uid": cy}, {"uid": eve}}, "project_admins": []map[string]any{{"uid": cy}},
			"project_created_by": map[string]any{"uid": cy},
			"project_tasks": []map[string]any{{"uid": "_:weed", "dgraph.type": "Task", "task_uuid": weedID, "task_name": "Weed the beds",
				"task_status": "todo", "task_deleted_at": live, "task_project": map[string]any{"uid": "_:garden"}, "task_created_by": map[string]any{"uid": cy}}}},
		{"uid": cy, "user_projects": []map[string]any{{"uid": "_:garden"}}},
		{"uid": eve, "user_projects": []map[string]any{{"uid": "_:garden"}}},
	})
	launch := create("wes", mine, map[string]any{"name": "Launch", "scope": "workspace"})
	launchOpen := widget("wes", launch, map[string]any{"kind": "metric", "query": map[string]any{"projects": []string{w.project}, "measure": "open"}, "x": 0, "y": 0, "w": 3, "h": 2})
	byProject := widget("wes", launch, map[string]any{"kind": "breakdown", "query": map[string]any{"projects": []string{w.project}, "by": "project"}, "x": 3, "y": 0, "w": 9, "h": 3})
	allOpen := widget("wes", launch, map[string]any{"kind": "metric", "query": map[string]any{"measure": "open"}, "x": 0, "y": 3, "w": 3, "h": 2})
	code, out = call("wes", http.MethodPost, mine+"/"+launch, map[string]any{"name": "Launch", "shared": true})
	expect("wes shares Launch", code, out, http.StatusOK)
	asWes := read("wes", launch)
	if value(asWes[launchOpen]) < 1 {
		t.Fatalf("ana's open work reads %v", asWes[launchOpen])
	}
	if rows, _ := asWes[byProject]["rows"].([]any); len(rows) != 1 {
		t.Fatalf("ana's projects: %v", asWes[byProject])
	}
	// wes keeps Launch, so he reads each widget's whole query, to change it.
	code, raw, body := send(session("wes"), http.MethodGet, mine+"/"+launch, nil)
	if expect("wes reads Launch", code, raw, http.StatusOK)["can_keep"] != true || !strings.Contains(body, w.project) {
		t.Fatalf("wes, who keeps Launch, reads its widgets without their projects: %s", body)
	}
	for _, who := range []string{"cy", "eve"} {
		// A widget's query is its keeper's: a reader gets what it shows
		// (its kind, title, place, weeks, measure and by), never the
		// projects, filters, search or goal it reads.
		code, raw, body := send(session(who), http.MethodGet, mine+"/"+launch, nil)
		d := expect(who+" reads Launch", code, raw, http.StatusOK)
		if strings.Contains(body, w.project) {
			t.Errorf("%s's GET of Launch names the project: %s", who, body)
		}
		shown := map[string]map[string]any{}
		for _, x := range d["widgets"].([]any) {
			m, _ := x.(map[string]any)
			q, _ := m["query"].(map[string]any)
			shown[fmt.Sprint(m["id"])] = q
		}
		if len(shown) != 3 || shown[launchOpen]["measure"] != "open" || shown[byProject]["by"] != "project" || d["can_keep"] != false {
			t.Fatalf("%s reads Launch's widgets as %v (can_keep %v)", who, shown, d["can_keep"])
		}
		code, raw, body = send(session(who), http.MethodGet, mine+"/"+launch+"/data", nil)
		expect(who+" reads Launch's data", code, raw, http.StatusOK)
		if strings.Contains(body, w.project) {
			t.Fatalf("%s's read of Launch names the project: %s", who, body)
		}
		got := read(who, launch)
		if v := value(got[allOpen]); v < 1 {
			t.Fatalf("%s's own open work in Garden reads %v, want at least 1", who, v)
		}
		if v := value(got[launchOpen]); v != 0 {
			t.Fatalf("%s, outside the project, reads open work on Website's widget: %v", who, v)
		}
		if rows, _ := got[byProject]["rows"].([]any); len(rows) != 0 {
			t.Fatalf("%s, outside the project, reads rows on Website's widget: %v", who, rows)
		}
	}

	// Deleting is soft: off the list, not there, and back on restore.
	kept := placed("ana", health)
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/delete", nil)
	expect("ana deletes Sprint health", code, out, http.StatusOK)
	if got := listed("ana", project, "dashboards"); got["Sprint health"] != "" {
		t.Fatalf("a deleted dashboard is listed: %v", got)
	}
	code, out = call("ben", http.MethodGet, mine+"/"+health, nil)
	expect("ben reads a deleted dashboard", code, out, http.StatusNotFound)
	// A deleted dashboard is there only to be restored: the one who keeps it
	// neither reads nor changes it until then.
	for _, r := range []struct {
		what, path string
		body       any
	}{
		{"renames", mine + "/" + health, map[string]any{"name": "Renamed", "shared": true}},
		{"adds a widget to", mine + "/" + health + "/widgets", map[string]any{"kind": "flow", "x": 0, "y": 7, "w": 6, "h": 3}},
		{"arranges", mine + "/" + health + "/arrange", map[string]any{"places": []map[string]any{{"id": throughput, "x": 0, "y": 0, "w": 12, "h": 4}}}},
		{"removes a widget of", mine + "/" + health + "/widgets/" + people + "/delete", nil},
	} {
		code, out = call("ana", http.MethodPost, r.path, r.body)
		expect("ana "+r.what+" her deleted dashboard", code, out, http.StatusNotFound)
	}
	for _, path := range []string{mine + "/" + health, mine + "/" + health + "/data"} {
		code, out = call("ana", http.MethodGet, path, nil)
		expect("ana reads her deleted dashboard at "+path, code, out, http.StatusNotFound)
	}
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/restore", nil)
	expect("ana restores it", code, out, http.StatusOK)
	if got := listed("ana", project, "dashboards"); got["Sprint health"] != health {
		t.Fatalf("a restored dashboard isn't listed: %v", got)
	}
	if got := placed("ana", health); fmt.Sprint(got) != fmt.Sprint(kept) {
		t.Fatalf("writes on the deleted dashboard reached it: %v, want %v", got, kept)
	}

	// An archived project's dashboards are read-only.
	code, out = call("ana", http.MethodPost, "/project/deleteProject", map[string]any{"project_uuid": w.project})
	expect("ana archives the project", code, out, http.StatusOK)
	code, out = call("ana", http.MethodGet, mine+"/"+health+"/data", nil)
	expect("ana reads an archived project's dashboard", code, out, http.StatusOK)
	code, out = call("ana", http.MethodPost, mine+"/"+health, map[string]any{"name": "Archived"})
	expect("ana renames an archived project's dashboard", code, out, http.StatusForbidden)
	code, out = call("ana", http.MethodPost, project, map[string]any{"name": "New"})
	expect("ana makes one in an archived project", code, out, http.StatusForbidden)
	code, out = call("ana", http.MethodPost, mine+"/"+health+"/widgets", map[string]any{"kind": "flow", "x": 0, "y": 7, "w": 6, "h": 3})
	expect("ana adds a widget in an archived project", code, out, http.StatusForbidden)
}

// The migration goes up, down and up again on Postgres 12, makes no ids of
// its own, and its rules hold: a dashboard has one scope, a person's own is
// never shared, and a widget sits on the grid of a dashboard that is there.
func TestDashboardsMigrationUpDownUp(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	sql := func(dir string) (string, string) {
		t.Helper()
		name := migrationNamed(t, "dashboards", dir)
		b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return name, string(b)
	}
	name, up := sql("up")
	_, down := sql("down")
	makesNoIDs(t, name, up)
	env := integration.SetupEnv(t)
	tables := []string{"dashboards", "dashboard_widgets"}
	// Later migrations that build on these tables come down first.
	for _, later := range laterMigrationsOn(t, name, "down", tables...) {
		if _, err := env.PG.Exec(later.sql); err != nil {
			t.Fatalf("%s: %v", later.name, err)
		}
	}
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		for _, rel := range append(tables, "dashboards_project_live", "dashboards_mine_live", "dashboard_widgets_dashboard") {
			if got := tableExists(t, env.PG, rel); got != step.want {
				t.Fatalf("step %d: %s exists = %v", i, rel, got)
			}
		}
	}
	someone, dash := uuid.New(), uuid.New()
	exec := func(q string, args ...any) error { _, err := env.PG.Exec(q, args...); return err }
	if err := exec(`INSERT INTO dashboards (id, mine_of, name, created_by) VALUES ($1, $2, 'Mine', $2)`, dash, someone); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO dashboards (name, created_by) VALUES ('No id', $1)`, someone); err == nil {
		t.Error("a dashboard without an id made from Go")
	}
	if err := exec(`INSERT INTO dashboards (id, project_id, mine_of, name, created_by) VALUES ($1, $2, $2, 'Both', $2)`, uuid.New(), someone); err == nil {
		t.Error("a dashboard both a project's and a person's own")
	}
	if err := exec(`INSERT INTO dashboards (id, mine_of, name, shared, created_by) VALUES ($1, $2, 'Shared', true, $2)`, uuid.New(), someone); err == nil {
		t.Error("a person's own dashboard shared")
	}
	widget := `INSERT INTO dashboard_widgets (id, dashboard_id, kind, query, x, y, w, h, created_by) VALUES ($1, $2, 'metric', '{}', $3, 0, $4, 2, $5)`
	if err := exec(widget, uuid.New(), dash, 0, 12, someone); err != nil {
		t.Fatal(err)
	}
	if err := exec(widget, uuid.New(), dash, 6, 12, someone); err == nil {
		t.Error("a widget off the grid")
	}
	if err := exec(widget, uuid.New(), uuid.New(), 0, 3, someone); err == nil {
		t.Error("a widget on a dashboard that isn't there")
	}
}
