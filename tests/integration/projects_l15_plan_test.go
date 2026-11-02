//go:build integration

package integration_test

// linear-15: a project's plan. Who leads it, when it starts and should land
// (each at the resolution chosen: a day, a month, a quarter, a half or a
// year, a start kept as its period's first day and a target as its last), its
// priority, its labels and its status, in one of five categories with the
// workspace's own statuses beside the built-in ones. Whoever may see the
// project reads it; its admins and the members who post its updates change
// it; its admins choose its lead, who is one of its admins or members; an
// archived project's plan stays as it was. The workspace's statuses and labels
// are its admins' to add, rename and delete (softly: a deleted status reads as
// its category's built-in until a restore, and a restore whose name a live one
// has taken since is refused); a plan change makes a label as it gives it.
// Each test has its own database: the statuses and labels are the workspace's.
// Run: integ_rel.sh <wt> <log> 'TestLinear15'

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	planController "github.com/akashc777/OneCamp/controllers/ProjectPlan"
	projectUpdateController "github.com/akashc777/OneCamp/controllers/ProjectUpdate"
	restoreController "github.com/akashc777/OneCamp/controllers/Restore"
	"github.com/akashc777/OneCamp/tests/integration"
)

type l15When struct {
	Date       string `json:"date"`
	Resolution string `json:"resolution"`
}

type l15Label struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type l15Status struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Color    string `json:"color"`
}

type l15Plan struct {
	Lead *struct {
		UUID string `json:"user_uuid"`
		Name string `json:"name"`
	} `json:"lead"`
	Start    *l15When   `json:"start"`
	Target   *l15When   `json:"target"`
	Priority string     `json:"priority"`
	Status   l15Status  `json:"status"`
	Labels   []l15Label `json:"labels"`
}

type l15Read struct {
	Plan    l15Plan `json:"plan"`
	CanEdit bool    `json:"can_edit"`
	CanLead bool    `json:"can_lead"`
}

// l15Get reads s's project's plan as who.
func l15Get(s *wa2Scale, who string) (int, l15Read, string) {
	s.t.Helper()
	code, out := s.call(who, http.MethodGet, "/", planController.GetPlan, map[string]string{"project_uuid": s.project}, nil)
	var got struct{ Data l15Read }
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			s.t.Fatalf("the plan as %s: %v in %s", who, err, out)
		}
	}
	return code, got.Data, out
}

// l15Set changes s's project's plan as who, body being the change.
func l15Set(s *wa2Scale, who, body string) (int, l15Plan, string) {
	s.t.Helper()
	code, out := s.call(who, http.MethodPost, "/", planController.SetPlan, map[string]string{"project_uuid": s.project}, []byte(body))
	var got struct{ Data struct{ Plan l15Plan } }
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			s.t.Fatalf("a plan change as %s: %v in %s", who, err, out)
		}
	}
	return code, got.Data.Plan, out
}

// l15Post posts body (none when "") to a registry or restore handler as who,
// with params as the route's.
func l15Post(s *wa2Scale, who string, h http.HandlerFunc, params map[string]string, body string) (int, string) {
	s.t.Helper()
	var raw []byte
	if body != "" {
		raw = []byte(body)
	}
	return s.call(who, http.MethodPost, "/", h, params, raw)
}

func l15ID(id string) map[string]string { return map[string]string{"id": id} }

// l15Data is out's data, decoded into v; a 200 is wanted.
func l15Data(t *testing.T, what string, code int, out string, v any) {
	t.Helper()
	var got struct{ Data json.RawMessage }
	if code != http.StatusOK {
		t.Fatalf("%s: %d %s", what, code, out)
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || json.Unmarshal(got.Data, v) != nil {
		t.Fatalf("%s: %v in %s", what, err, out)
	}
}

func l15Names(labels []l15Label) string {
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return strings.Join(names, ",")
}

func TestLinear15APlansPostersChangeItAndItsAdminsChooseTheLead(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	s := newWA2Scale(t, env, integration.SetupDgraph(t))
	// Ana is the project's admin and Ben a member; Vic views it, Cal comments
	// on it, and Oz is outside it. Gus is a guest with a member's role row,
	// and Sam a member whose account is deactivated.
	s.person("vic", true)
	s.person("cal", true)
	s.exec(`UPDATE project_roles SET role = 'commenter' WHERE project_id = $1 AND user_id = $2`, s.project, s.people["cal"])
	s.person("oz", false)
	gus, sam := s.person("gus", true), s.person("sam", true)
	s.dg.Mutate(t, []map[string]any{{"uid": gus, "is_external": true}, {"uid": sam, "user_deleted_at": "2026-10-01T00:00:00Z"}})
	s.exec(`UPDATE project_roles SET role = 'member' WHERE project_id = $1 AND user_id IN ($2, $3)`, s.project, s.people["gus"], s.people["sam"])

	// Nobody has planned it: in progress and nothing else, its labels an empty
	// list, never null.
	code, read, out := l15Get(s, "ana")
	var flat bytes.Buffer
	_ = json.Compact(&flat, []byte(out))
	if code != http.StatusOK || read.Plan.Status != (l15Status{Category: "in_progress", Name: "In progress", Color: "amber"}) ||
		read.Plan.Lead != nil || read.Plan.Start != nil || read.Plan.Target != nil || read.Plan.Priority != "" ||
		!strings.Contains(flat.String(), `"labels":[]`) || !read.CanEdit || !read.CanLead {
		t.Fatalf("an admin's first read: %d %s", code, out)
	}
	for who, want := range map[string][2]bool{"ben": {true, false}, "cal": {false, false}, "vic": {false, false}} {
		if code, read, out := l15Get(s, who); code != http.StatusOK || read.CanEdit != want[0] || read.CanLead != want[1] {
			t.Fatalf("%s's read: %d %s, want can_edit %v and can_lead %v", who, code, out, want[0], want[1])
		}
	}
	if code, _, out := l15Get(s, "oz"); code != http.StatusNotFound {
		t.Fatalf("someone outside the project reads its plan: %d %s", code, out)
	}

	// A member who posts its updates sets its status, priority and labels.
	code, plan, out := l15Set(s, "ben", `{"status":"planned","priority":"high","new_labels":[{"name":"Q1 launch","color":"blue"}]}`)
	if code != http.StatusOK || plan.Status.Category != "planned" || plan.Status.Name != "Planned" || plan.Priority != "high" ||
		len(plan.Labels) != 1 || plan.Labels[0].Name != "Q1 launch" || plan.Labels[0].Color != "blue" {
		t.Fatalf("a member's change: %d %s", code, out)
	}
	// Choosing its lead is its admins', in its own words; a viewer and a
	// commenter change nothing, and someone outside finds nothing.
	ben := s.people["ben"].String()
	if code, _, out := l15Set(s, "ben", `{"lead_uuid":"`+ben+`"}`); code != http.StatusForbidden || !strings.Contains(out, "choose its lead") {
		t.Fatalf("a member chooses the lead: %d %s, want a 403 in words", code, out)
	}
	for who, want := range map[string]int{"vic": http.StatusForbidden, "cal": http.StatusForbidden, "oz": http.StatusNotFound} {
		if code, _, out := l15Set(s, who, `{"priority":"low"}`); code != want {
			t.Fatalf("%s changes the priority: %d %s, want %d", who, code, out, want)
		}
	}
	// The lead is one of its admins or members: not someone outside it, nor
	// one who only views or comments on it, nor a guest, whatever their role
	// row says.
	for _, who := range []string{"oz", "vic", "cal", "gus"} {
		if code, _, out := l15Set(s, "ana", `{"lead_uuid":"`+s.people[who].String()+`"}`); code != http.StatusBadRequest || !strings.Contains(out, "admins or members") {
			t.Fatalf("%s as the lead: %d %s, want a 400 in words", who, code, out)
		}
	}
	// A deactivated member keeps their place but takes no new work: refused
	// in the words an assignee's refusal takes.
	if code, _, out := l15Set(s, "ana", `{"lead_uuid":"`+s.people["sam"].String()+`"}`); code != http.StatusConflict ||
		!strings.Contains(out, "sam can't be made the project's lead: their account is deactivated.") {
		t.Fatalf("a deactivated member as the lead: %d %s, want a 409 in words", code, out)
	}
	code, plan, out = l15Set(s, "ana", `{"lead_uuid":"`+ben+`"}`)
	if code != http.StatusOK || plan.Lead == nil || plan.Lead.UUID != ben || plan.Lead.Name != "ben" || plan.Priority != "high" || plan.Status.Category != "planned" {
		t.Fatalf("an admin makes a member the lead: %d %s", code, out)
	}
	if _, read, out := l15Get(s, "vic"); read.Plan.Priority != "high" || read.Plan.Lead == nil || l15Names(read.Plan.Labels) != "Q1 launch" {
		t.Fatalf("a viewer's read after the refused changes: %s", out)
	}

	// Archived, the plan stays as it was, even for its admins.
	s.dg.Mutate(t, []map[string]any{{"uid": s.uids["p"], "project_deleted_at": "2026-10-01T00:00:00Z"}})
	if code, _, out := l15Set(s, "ana", `{"priority":"urgent"}`); code != http.StatusForbidden || !strings.Contains(out, "archived") {
		t.Fatalf("an admin changes an archived project's plan: %d %s, want a 403 in words", code, out)
	}
	if code, read, out := l15Get(s, "ana"); code != http.StatusOK || read.CanEdit || read.CanLead || read.Plan.Priority != "high" {
		t.Fatalf("an archived project's plan: %d %s", code, out)
	}
}

func TestLinear15APlansDatesAreKeptAtTheirResolution(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	s := newWA2Scale(t, env, integration.SetupDgraph(t))

	// A month's start is its first day, a quarter's target its last.
	code, plan, out := l15Set(s, "ben", `{"start":{"date":"2027-02-17","resolution":"month"},"target":{"date":"2027-02-17","resolution":"quarter"}}`)
	if code != http.StatusOK || plan.Start == nil || *plan.Start != (l15When{"2027-02-01", "month"}) || plan.Target == nil || *plan.Target != (l15When{"2027-03-31", "quarter"}) {
		t.Fatalf("a month's start and a quarter's target: %d %s", code, out)
	}
	// A start after the target is refused in words and changes nothing.
	if code, _, out := l15Set(s, "ben", `{"start":{"date":"2027-04-01","resolution":"day"}}`); code != http.StatusBadRequest || !strings.Contains(out, "start after its target") {
		t.Fatalf("a start after the target: %d %s, want a 400 in words", code, out)
	}
	// A half's target is its last day; parts left out keep what they hold.
	code, plan, out = l15Set(s, "ben", `{"target":{"date":"2027-08-09","resolution":"half"},"priority":"medium"}`)
	if code != http.StatusOK || *plan.Target != (l15When{"2027-12-31", "half"}) || *plan.Start != (l15When{"2027-02-01", "month"}) || plan.Priority != "medium" {
		t.Fatalf("a half's target beside the month's start: %d %s", code, out)
	}
	// A part sent as null is cleared; the others stay.
	code, plan, out = l15Set(s, "ben", `{"start":null,"priority":null}`)
	if code != http.StatusOK || plan.Start != nil || plan.Priority != "" || plan.Target == nil || plan.Target.Date != "2027-12-31" {
		t.Fatalf("clearing the start and the priority: %d %s", code, out)
	}
	// What isn't a change is refused in words.
	for _, bad := range []string{`{}`, `not a change`, `{"target":{"date":"2027-02-30","resolution":"day"}}`,
		`{"target":{"date":"2027-02-01","resolution":"week"}}`, `{"priority":"someday"}`, `{"status":"shipping"}`,
		`{"status":"` + uuid.NewString() + `"}`, `{"labels":["` + uuid.NewString() + `"]}`, `{"new_labels":[{"name":"   "}]}`} {
		if code, _, out := l15Set(s, "ben", bad); code != http.StatusBadRequest || msgFromBody(out) == "" {
			t.Fatalf("the change %s: %d %s, want a 400 in words", bad, code, out)
		}
	}

	// The projects overview carries each project's plan in its row.
	code, out = s.call("ben", http.MethodGet, "/", projectUpdateController.ProjectsOverview, nil, nil)
	var overview struct {
		Projects []struct {
			UUID string   `json:"project_uuid"`
			Plan *l15Plan `json:"plan"`
		} `json:"projects"`
	}
	l15Data(t, "the projects overview", code, out, &overview)
	for _, p := range overview.Projects {
		if p.UUID == s.project {
			if p.Plan == nil || p.Plan.Target == nil || *p.Plan.Target != (l15When{"2027-12-31", "half"}) || p.Plan.Status.Category != "in_progress" {
				t.Fatalf("the overview's row for the project: %s", out)
			}
			return
		}
	}
	t.Fatalf("the overview has no row for the project: %s", out)
}

func TestLinear15StatusesAndLabelsAreTheWorkspaceAdminsToChange(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	s := newWA2Scale(t, env, integration.SetupDgraph(t))
	// Ana is a workspace admin; Ben is the project's member and not one.
	s.exec(`INSERT INTO admin_users (email_id) VALUES ('ana@scale.test')`)

	// Everyone reads the registry: the five categories in a project's order.
	var reg struct {
		Categories []struct{ Key string } `json:"categories"`
		Statuses   []l15Status            `json:"statuses"`
		Labels     []l15Label             `json:"labels"`
		Colors     []string               `json:"colors"`
	}
	code, out := s.call("ben", http.MethodGet, "/", planController.Registry, nil, nil)
	l15Data(t, "a member's registry", code, out, &reg)
	keys := []string{}
	for _, c := range reg.Categories {
		keys = append(keys, c.Key)
	}
	if strings.Join(keys, ",") != "planned,in_progress,paused,done,cancelled" || len(reg.Statuses) != 0 || len(reg.Labels) != 0 || len(reg.Colors) == 0 {
		t.Fatalf("a member's first registry: %s", out)
	}

	// Only a workspace admin adds a status or a label.
	for what, h := range map[string]http.HandlerFunc{"a status": planController.AddStatus, "a label": planController.AddLabel} {
		if code, out := l15Post(s, "ben", h, nil, `{"name":"In review","category":"in_progress"}`); code != http.StatusForbidden {
			t.Fatalf("a member adds %s: %d %s, want 403", what, code, out)
		}
	}
	var review l15Status
	code, out = l15Post(s, "ana", planController.AddStatus, nil, `{"name":"  In   review ","category":"in_progress","color":"purple"}`)
	l15Data(t, "an admin adds a status", code, out, &review)
	if review.ID == "" || review.Name != "In review" || review.Category != "in_progress" || review.Color != "purple" {
		t.Fatalf("the added status: %s", out)
	}
	for _, bad := range []string{`{"name":"Paused","category":"paused"}`, `{"name":"in REVIEW","category":"in_progress"}`,
		`{"name":"Blocked","category":"stuck"}`, `{"name":"","category":"planned"}`, `{"name":"Blocked","category":"paused","color":"chartreuse"}`} {
		if code, out := l15Post(s, "ana", planController.AddStatus, nil, bad); code != http.StatusBadRequest || msgFromBody(out) == "" {
			t.Fatalf("adding the status %s: %d %s, want a 400 in words", bad, code, out)
		}
	}
	if code, out := l15Post(s, "ben", planController.EditStatus, l15ID(review.ID), `{"name":"Reviewing"}`); code != http.StatusForbidden {
		t.Fatalf("a member renames a status: %d %s", code, out)
	}
	if code, out := l15Post(s, "ben", planController.DeleteStatus, l15ID(review.ID), ""); code != http.StatusForbidden {
		t.Fatalf("a member deletes a status: %d %s", code, out)
	}

	// A project in it reads it. Deleted, the project reads as its category's
	// built-in and no one may give it; a restore, an admin's, brings it back.
	code, plan, out := l15Set(s, "ben", `{"status":"`+review.ID+`"}`)
	if code != http.StatusOK || plan.Status != review {
		t.Fatalf("a project in the workspace's status: %d %s", code, out)
	}
	if code, out := l15Post(s, "ana", planController.DeleteStatus, l15ID(review.ID), ""); code != http.StatusOK {
		t.Fatalf("an admin deletes a status: %d %s", code, out)
	}
	if _, read, out := l15Get(s, "ben"); read.Plan.Status != (l15Status{Category: "in_progress", Name: "In progress", Color: "amber"}) {
		t.Fatalf("a project in a deleted status: %s", out)
	}
	if code, _, out := l15Set(s, "ben", `{"status":"`+review.ID+`"}`); code != http.StatusBadRequest {
		t.Fatalf("giving a deleted status: %d %s, want 400", code, out)
	}
	restore := func(who, kind, id string) (int, string) {
		return l15Post(s, who, restoreController.Restore, map[string]string{"kind": kind, "id": id}, "")
	}
	if code, out := restore("ben", "project_status", review.ID); code != http.StatusForbidden {
		t.Fatalf("a member restores a status: %d %s, want 403", code, out)
	}
	if code, out := restore("ana", "project_status", review.ID); code != http.StatusOK {
		t.Fatalf("an admin restores a status: %d %s", code, out)
	}
	if _, read, out := l15Get(s, "ben"); read.Plan.Status != review {
		t.Fatalf("a project after its status's restore: %s", out)
	}

	// A label made as it's given is the workspace's, whatever case it's typed
	// in next; an admin adds one from the settings, but not a live one's name.
	code, plan, out = l15Set(s, "ben", `{"new_labels":[{"name":"Customer"}]}`)
	if code != http.StatusOK || len(plan.Labels) != 1 || plan.Labels[0].Color != "slate" {
		t.Fatalf("a member makes a label: %d %s", code, out)
	}
	customer := plan.Labels[0]
	code, plan, out = l15Set(s, "ben", `{"labels":[],"new_labels":[{"name":" CUSTOMER "}]}`)
	if code != http.StatusOK || len(plan.Labels) != 1 || plan.Labels[0] != customer {
		t.Fatalf("a label's name typed in another case: %d %s, want the same label", code, out)
	}
	var q4 l15Label
	code, out = l15Post(s, "ana", planController.AddLabel, nil, `{"name":"Q4 launch","color":"teal"}`)
	l15Data(t, "an admin adds a label", code, out, &q4)
	if code, out := l15Post(s, "ana", planController.AddLabel, nil, `{"name":"customer"}`); code != http.StatusBadRequest || !strings.Contains(out, "already") {
		t.Fatalf("adding a live label's name: %d %s, want a 400 in words", code, out)
	}
	code, plan, out = l15Set(s, "ben", `{"labels":["`+q4.ID+`","`+customer.ID+`","`+q4.ID+`"]}`)
	if code != http.StatusOK || l15Names(plan.Labels) != "Customer,Q4 launch" {
		t.Fatalf("labels given by id, one twice: %d %s", code, out)
	}

	// Deleted, a label leaves the project, and a save meanwhile keeps it for
	// its restore; a restore whose name a live label has taken since is
	// refused in words until that one is renamed.
	if code, out := l15Post(s, "ben", planController.DeleteLabel, l15ID(q4.ID), ""); code != http.StatusForbidden {
		t.Fatalf("a member deletes a label: %d %s", code, out)
	}
	if code, out := l15Post(s, "ana", planController.DeleteLabel, l15ID(q4.ID), ""); code != http.StatusOK {
		t.Fatalf("an admin deletes a label: %d %s", code, out)
	}
	if code, plan, out = l15Set(s, "ben", `{"labels":["`+customer.ID+`"]}`); code != http.StatusOK || l15Names(plan.Labels) != "Customer" {
		t.Fatalf("a save while a label is deleted: %d %s", code, out)
	}
	var again l15Label
	code, out = l15Post(s, "ana", planController.AddLabel, nil, `{"name":"q4 LAUNCH"}`)
	l15Data(t, "a new label with a deleted one's name", code, out, &again)
	if code, out := restore("ana", "project_label", q4.ID); code != http.StatusBadRequest || !strings.Contains(out, "Rename that one first") {
		t.Fatalf("restoring a label whose name is taken: %d %s, want a 400 in words", code, out)
	}
	if code, out := l15Post(s, "ana", planController.EditLabel, l15ID(again.ID), `{"name":"Q4 later"}`); code != http.StatusOK {
		t.Fatalf("an admin renames a label: %d %s", code, out)
	}
	if code, out := restore("ana", "project_label", q4.ID); code != http.StatusOK {
		t.Fatalf("restoring the label once its name is free: %d %s", code, out)
	}
	if _, read, out := l15Get(s, "ben"); l15Names(read.Plan.Labels) != "Customer,Q4 launch" {
		t.Fatalf("the project's labels after the restore: %s", out)
	}
	code, out = s.call("ben", http.MethodGet, "/", planController.Registry, nil, nil)
	l15Data(t, "the registry at the end", code, out, &reg)
	if l15Names(reg.Labels) != "Customer,Q4 later,Q4 launch" || len(reg.Statuses) != 1 || reg.Statuses[0] != review {
		t.Fatalf("the registry at the end: %s", out)
	}
}

// The caps are refused in words where they bite: a project's 21st label, the
// workspace's 301st label from its settings and from a plan change (a deleted
// one doesn't count, and a live one's name is given, not made), and its 51st
// status of its own. A deleted label's restore gives it back to each project
// that carried it, except one that has taken 20 others since (V-213).
func TestLinear15CapsAreRefusedWhereTheyBite(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	s := newWA2Scale(t, env, integration.SetupDgraph(t))
	s.exec(`INSERT INTO admin_users (email_id) VALUES ('ana@scale.test')`)
	// ids is the live labels named like like, in their names' order.
	ids := func(like string) []string {
		t.Helper()
		rows, err := s.env.PG.Query(`SELECT id FROM project_labels WHERE deleted_at IS NULL AND name LIKE $1 ORDER BY name`, like)
		if err != nil {
			t.Fatalf("the labels named like %s: %v", like, err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("a label's id: %v", err)
			}
			got = append(got, id)
		}
		return got
	}
	list := func(ids ...string) string { return `["` + strings.Join(ids, `","`) + `"]` }
	count := func(q string, args ...any) (n int) {
		t.Helper()
		if err := s.env.PG.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	// A project carries 20 labels: a 21st, by id or made as it's given, is
	// refused in words and makes nothing.
	s.exec(`INSERT INTO project_labels (name, created_by) SELECT 'Area ' || lpad(n::text, 2, '0'), $1 FROM generate_series(1, 21) n`, s.people["ana"])
	areas := ids("Area %")
	if code, _, out := l15Set(s, "ben", `{"labels":`+list(areas[:20]...)+`,"new_labels":[{"name":"One more"}]}`); code != http.StatusBadRequest || !strings.Contains(out, "at most 20 labels") {
		t.Fatalf("a project's 21st label, made as it's given: %d %s, want a 400 in words", code, out)
	}
	if code, _, out := l15Set(s, "ben", `{"labels":`+list(areas...)+`}`); code != http.StatusBadRequest || !strings.Contains(out, "at most 20 labels") {
		t.Fatalf("a project's 21st label, by id: %d %s, want a 400 in words", code, out)
	}
	if n := count(`SELECT COUNT(*) FROM project_labels WHERE name = 'One more'`); n != 0 {
		t.Fatalf("a refused label was made: %d", n)
	}
	if code, plan, out := l15Set(s, "ben", `{"labels":`+list(areas[:20]...)+`}`); code != http.StatusOK || len(plan.Labels) != 20 {
		t.Fatalf("a project's 20 labels: %d %s", code, out)
	}

	// Area 20 is deleted, and the project takes Area 21 in its place; Side, a
	// second project, carries Area 20 alone. Restored, Area 20 is back on Side
	// but not on the project, which carries its 20 still.
	var team string
	if err := s.env.PG.QueryRow(`SELECT team_id FROM projects WHERE id = $1`, s.project).Scan(&team); err != nil {
		t.Fatalf("the project's team: %v", err)
	}
	side := uuid.New()
	s.exec(`INSERT INTO projects (id, project_name, team_id, created_by) VALUES ($1, 'Side', $2, $3)`, side, team, s.people["ana"])
	s.exec(`INSERT INTO project_plans (project_id) VALUES ($1)`, side)
	s.exec(`INSERT INTO project_plan_labels (project_id, label_id) VALUES ($1, $2)`, side, areas[19])
	if code, out := l15Post(s, "ana", planController.DeleteLabel, l15ID(areas[19]), ""); code != http.StatusOK {
		t.Fatalf("an admin deletes Area 20: %d %s", code, out)
	}
	if code, plan, out := l15Set(s, "ben", `{"labels":`+list(append(areas[:19:19], areas[20])...)+`}`); code != http.StatusOK || len(plan.Labels) != 20 {
		t.Fatalf("Area 21 in the deleted label's place: %d %s", code, out)
	}
	if code, out := l15Post(s, "ana", restoreController.Restore, map[string]string{"kind": "project_label", "id": areas[19]}, ""); code != http.StatusOK {
		t.Fatalf("an admin restores Area 20: %d %s", code, out)
	}
	if _, read, out := l15Get(s, "ben"); len(read.Plan.Labels) != 20 || strings.Contains(l15Names(read.Plan.Labels), "Area 20") ||
		!strings.Contains(l15Names(read.Plan.Labels), "Area 21") {
		t.Fatalf("a project at 20 labels after a label it carried is restored: %s", out)
	}
	if n := count(`SELECT COUNT(*) FROM project_plan_labels WHERE project_id = $1 AND label_id = $2`, side, areas[19]); n != 1 {
		t.Fatalf("a project with room after a label it carried is restored: carries it %d times, want once", n)
	}

	// The workspace holds 300 live labels: at 299 live and one deleted, a
	// plan change makes the 300th; the 301st is refused in words from the
	// settings and from a plan change, and makes nothing; a live label's name
	// is still given.
	s.exec(`INSERT INTO project_labels (name, created_by) SELECT 'Bulk ' || lpad(n::text, 3, '0'), $1 FROM generate_series(1, 278) n`, s.people["ana"])
	s.exec(`INSERT INTO project_labels (name, created_by, deleted_at) VALUES ('Gone', $1, NOW())`, s.people["ana"])
	if n := count(`SELECT COUNT(*) FROM project_labels WHERE deleted_at IS NULL`); n != 299 {
		t.Fatalf("live labels before the 300th: %d, want 299", n)
	}
	if code, plan, out := l15Set(s, "ben", `{"labels":[],"new_labels":[{"name":"Three hundred"}]}`); code != http.StatusOK || l15Names(plan.Labels) != "Three hundred" {
		t.Fatalf("the workspace's 300th label, beside a deleted one: %d %s", code, out)
	}
	full := "The workspace has 300 labels"
	if code, out := l15Post(s, "ana", planController.AddLabel, nil, `{"name":"From the settings"}`); code != http.StatusBadRequest || !strings.Contains(out, full) {
		t.Fatalf("the workspace's 301st label from the settings: %d %s, want a 400 in words", code, out)
	}
	if code, _, out := l15Set(s, "ben", `{"new_labels":[{"name":"From a plan"}]}`); code != http.StatusBadRequest || !strings.Contains(out, full) {
		t.Fatalf("the workspace's 301st label from a plan change: %d %s, want a 400 in words", code, out)
	}
	if n := count(`SELECT COUNT(*) FROM project_labels WHERE deleted_at IS NULL`); n != 300 {
		t.Fatalf("live labels after the refused 301st: %d, want 300", n)
	}
	if code, plan, out := l15Set(s, "ben", `{"new_labels":[{"name":"area 01"}]}`); code != http.StatusOK || l15Names(plan.Labels) != "Area 01,Three hundred" {
		t.Fatalf("a live label's name given at the workspace's most: %d %s", code, out)
	}
	// A deleted label's restore is held to the 300 too (V-212): at the most it
	// is refused in the same words and stays deleted; at 299 it is the 300th.
	restoreNamed := func(kind, table, name string) (int, string) {
		var id string
		if err := s.env.PG.QueryRow(`SELECT id FROM `+table+` WHERE name = $1`, name).Scan(&id); err != nil {
			t.Fatalf("%s %s: %v", table, name, err)
		}
		return l15Post(s, "ana", restoreController.Restore, map[string]string{"kind": kind, "id": id}, "")
	}
	if code, out := restoreNamed("project_label", "project_labels", "Gone"); code != http.StatusBadRequest || !strings.Contains(out, full) {
		t.Fatalf("a deleted label's restore at the workspace's most: %d %s, want a 400 in words", code, out)
	}
	if n := count(`SELECT COUNT(*) FROM project_labels WHERE name = 'Gone' AND deleted_at IS NOT NULL`); n != 1 {
		t.Fatal("a label restored past the workspace's most")
	}
	s.exec(`UPDATE project_labels SET deleted_at = NOW() WHERE name = 'Bulk 001'`)
	if code, out := restoreNamed("project_label", "project_labels", "Gone"); code != http.StatusOK {
		t.Fatalf("a deleted label's restore as the workspace's 300th: %d %s", code, out)
	}
	if n := count(`SELECT COUNT(*) FROM project_labels WHERE deleted_at IS NULL`); n != 300 {
		t.Fatalf("live labels after the restore: %d, want 300", n)
	}

	// The workspace holds 50 statuses of its own: at 49 live and one deleted,
	// the 50th is added; the 51st is refused in words.
	s.exec(`INSERT INTO project_statuses (name, category, created_by) SELECT 'Stage ' || lpad(n::text, 2, '0'), 'planned', $1 FROM generate_series(1, 49) n`, s.people["ana"])
	s.exec(`INSERT INTO project_statuses (name, category, created_by, deleted_at) VALUES ('Shelved', 'paused', $1, NOW())`, s.people["ana"])
	if code, out := l15Post(s, "ana", planController.AddStatus, nil, `{"name":"Stage 50","category":"in_progress"}`); code != http.StatusOK {
		t.Fatalf("the workspace's 50th status, beside a deleted one: %d %s", code, out)
	}
	if code, out := l15Post(s, "ana", planController.AddStatus, nil, `{"name":"Stage 51","category":"in_progress"}`); code != http.StatusBadRequest ||
		!strings.Contains(out, "The workspace has 50 statuses") {
		t.Fatalf("the workspace's 51st status: %d %s, want a 400 in words", code, out)
	}
	// And a deleted status's restore is held to the 50 (V-212).
	if code, out := restoreNamed("project_status", "project_statuses", "Shelved"); code != http.StatusBadRequest || !strings.Contains(out, "The workspace has 50 statuses") {
		t.Fatalf("a deleted status's restore at the workspace's most: %d %s, want a 400 in words", code, out)
	}
	if n := count(`SELECT COUNT(*) FROM project_statuses WHERE name = 'Shelved' AND deleted_at IS NOT NULL`); n != 1 {
		t.Fatal("a status restored past the workspace's most")
	}
	s.exec(`UPDATE project_statuses SET deleted_at = NOW() WHERE name = 'Stage 01'`)
	if code, out := restoreNamed("project_status", "project_statuses", "Shelved"); code != http.StatusOK {
		t.Fatalf("a deleted status's restore as the workspace's 50th: %d %s", code, out)
	}
	if n := count(`SELECT COUNT(*) FROM project_statuses WHERE deleted_at IS NULL`); n != 50 {
		t.Fatalf("live statuses after the restore: %d, want 50", n)
	}
}

// msgFromBody is a JSON answer's msg.
func msgFromBody(out string) string {
	var body struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal([]byte(out), &body)
	return body.Msg
}
