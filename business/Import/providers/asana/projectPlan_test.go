package asana

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// Head 3 part 2 (linear-15's plan from Asana): a workspace read from Asana
// carries each project's plan as the import takes it. Its owner leads it, its
// start and due days come as days, and its latest status update's kind falls
// in its category with Asana's own name for it. A completed project is done
// whatever its last update said. A project that keeps none of it has no
// plan, and a kind the import doesn't know is named for the report, never
// dropped without a word. Before, every Asana project came with no plan.
func TestAWorkspaceReadFromAsanaCarriesEachProjectsPlan(t *testing.T) {
	project := func(gid, extra string) string {
		return `{"gid": "` + gid + `", "name": "` + gid + `", "notes": "", "archived": false, "created_at": "2026-09-01T09:00:00.000Z"` + extra + `}`
	}
	status := func(kind string) string {
		return `, "current_status_update": {"gid": "su-` + kind + `", "status_type": "` + kind + `"}`
	}
	projects := strings.Join([]string{
		project("launch", `, "owner": {"gid": "u-1"}, "start_on": "2026-10-01", "due_on": "2027-03-31"`+status("at_risk")),
		project("steady", status("on_track")),
		project("slipping", status("off_track")),
		project("held", `, "owner": null, "start_on": null, "due_on": null`+status("on_hold")),
		project("closed", `, "completed": true`+status("complete")),
		project("finished", `, "completed": true`+status("on_track")),
		project("quiet", `, "current_status_update": null`),
		project("odd", status("achieved")),
	}, ",")
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/1.0/workspaces/ws/projects" {
			asked = r.URL.Query().Get("opt_fields")
			fmt.Fprintf(w, `{"data": [%s], "next_page": null}`, projects)
			return
		}
		fmt.Fprint(w, `{"data": [], "next_page": null}`) // each project's members and fields: none
	}))
	defer srv.Close()
	defer func(old string) { apiBase = old }(apiBase)
	apiBase = srv.URL + "/api/1.0"

	out := make(chan importProvider.SourceProject, 16)
	if err := New().sendProjects(context.Background(), "tok", "ws", out); err != nil {
		t.Fatal(err)
	}
	close(out)
	for _, f := range []string{"owner.gid", "start_on", "due_on", "completed", "current_status_update.status_type"} {
		if !strings.Contains(","+asked+",", ","+f+",") {
			t.Errorf("projects aren't asked for %s: %s", f, asked)
		}
	}

	on := func(s string) *importProvider.SourceWhen {
		d, ok := importProvider.ParseDay(s)
		if !ok {
			t.Fatalf("bad day %q", s)
		}
		return &importProvider.SourceWhen{Day: d, Resolution: "day"}
	}
	state := func(category, name string) *importProvider.SourcePlan {
		return &importProvider.SourcePlan{Status: importProvider.SourcePlanStatus{Category: category, Name: name}}
	}
	want := map[string]*importProvider.SourcePlan{
		"launch": {LeadID: "u-1", Start: on("2026-10-01"), Target: on("2027-03-31"),
			Status: importProvider.SourcePlanStatus{Category: "in_progress", Name: "At risk"}},
		"steady":   state("in_progress", "On track"),
		"slipping": state("in_progress", "Off track"),
		"held":     state("paused", "On hold"),
		"closed":   state("done", "Complete"),
		"finished": state("done", "Complete"),
		"quiet":    nil,
		"odd":      {Unread: []string{`its status, "achieved" (a kind Asana gave that the import doesn't know)`}},
	}
	got := map[string]*importProvider.SourcePlan{}
	for sp := range out {
		got[sp.SourceID] = sp.Plan
	}
	if len(got) != len(want) {
		t.Fatalf("%d projects came, want %d", len(got), len(want))
	}
	for id, w := range want {
		if !reflect.DeepEqual(got[id], w) {
			t.Errorf("%s: plan %s, want %s", id, planText(got[id]), planText(w))
		}
	}
}

func planText(p *importProvider.SourcePlan) string {
	if p == nil {
		return "none"
	}
	when := func(w *importProvider.SourceWhen) string {
		if w == nil {
			return "-"
		}
		return w.Day.String() + "/" + w.Resolution
	}
	return fmt.Sprintf("{lead %q start %s target %s status %+v unread %q}", p.LeadID, when(p.Start), when(p.Target), p.Status, p.Unread)
}

// A completed Asana task comes with when it was completed, so a project's
// pace counts it then (business/Import statusSince). An open one comes with
// none: Asana keeps no time a task entered its section. Every task read
// asks for completed_at.
func TestAnAsanaTaskComesWithWhenItWasCompleted(t *testing.T) {
	asked, _ := url.QueryUnescape(taskOptFields)
	if !strings.Contains(","+asked+",", ",completed_at,") {
		t.Errorf("tasks aren't asked for completed_at: %s", asked)
	}
	var page asanaTasksResp
	body := `{"data": [
		{"gid": "1", "name": "Ship", "completed": true, "completed_at": "2026-10-09T12:00:00.000Z", "created_at": "2026-09-01T09:00:00.000Z", "modified_at": "2026-10-20T09:00:00.000Z"},
		{"gid": "2", "name": "Draft", "completed": false, "completed_at": null, "created_at": "2026-09-01T09:00:00.000Z", "modified_at": "2026-10-20T09:00:00.000Z"}],
		"next_page": null}`
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	done := buildSourceTask(&page.Data[0], "p", nil, "")
	open := buildSourceTask(&page.Data[1], "p", nil, "")
	if want := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC); done.StatusSince == nil || !done.StatusSince.Equal(want) {
		t.Errorf("a done task's time: %v, want %v", done.StatusSince, want)
	}
	if open.StatusSince != nil {
		t.Errorf("an open task's time: %v, want none", open.StatusSince)
	}
}
