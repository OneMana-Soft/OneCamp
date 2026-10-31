package csv

import (
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// A list as our export writes it (business/Project taskListExport).
const ourExport = "Key,Title,Type,Status,Priority,Assignee,Start date,Due date,Estimate (hours),Tags,Parent,Created,Created by,Budget (USD),Area,Signed off,Description,Link\n" +
	"Q4-12,Write the announcement,Task,In progress,High,Ana Lima,2026-10-20,2026-10-24,1.50,\"copy, marketing\",,2026-10-01 09:00,Ben Ray,1250.50,Design,Yes,\"First line\nSecond\",https://x.test/app/task/u1\n" +
	"Q4-13,Draft the FAQ,Task,To do,Low,Ben Ray,,,,,Q4-12,2026-10-02 10:00,Ana Lima,,Research,,,https://x.test/app/task/u2\n" +
	"Q4-128,Launch day,Milestone,To do,Medium,,,2026-11-01,,,,2026-10-03 11:00,Ana Lima,,Design,,,https://x.test/app/task/u3\n"

func sheetOf(t *testing.T, in string, opts importProvider.JobOptions) (*table, *sheet) {
	t.Helper()
	tb := mustRead(t, in)
	if opts == nil {
		opts = importProvider.JobOptions{}
	}
	return tb, read(tb, "Q4 launch", opts)
}

func fieldValues(st importProvider.SourceTask) map[string]any {
	out := map[string]any{}
	for _, v := range st.Fields {
		out[v.Field.Name] = v.Value
	}
	return out
}

// Our own export comes back as it went: keys and numbers, the sub-task under
// its parent, dates as days, hours, tags, people, the description's lines,
// a milestone, and each field with its kind.
func TestOurExportReadsBack(t *testing.T) {
	tb, s := sheetOf(t, ourExport, importProvider.JobOptions{"time_zone": "Asia/Kolkata"})
	if p := s.project; p.SourceID != tb.id || p.Name != "Q4 launch" || p.SourceKey != "Q4" || p.HighestNumber != 128 {
		t.Errorf("project: %+v", p)
	}
	kinds := map[string]string{}
	for _, f := range s.project.Fields {
		kinds[f.Name] = f.Type + f.Currency
	}
	if want := map[string]string{"Budget": "moneyUSD", "Area": "select", "Signed off": "checkbox"}; len(kinds) != 3 || kinds["Budget"] != want["Budget"] || kinds["Area"] != want["Area"] || kinds["Signed off"] != want["Signed off"] {
		t.Errorf("fields: %v", kinds)
	}
	if len(s.tasks) != 3 || s.tasks[2].SourceID != "__sub__:"+tb.id+":q4-12" || s.tasks[2].ParentTaskID != tb.id+":q4-12" {
		t.Fatalf("top level and the marker: %+v", s.tasks)
	}
	first, launch := s.tasks[0], s.tasks[1]
	if first.SourceKey != "Q4-12" || first.Status != "In progress" || first.Priority != "High" || first.EstimateMinutes != 90 ||
		strings.Join(first.Labels, "|") != "copy|marketing" || first.StartDay.String() != "2026-10-20" || first.DueDay.String() != "2026-10-24" ||
		first.StartDate != nil || strings.Join(first.AssigneeIds, "|") != "name:ana lima" || first.CreatedBy != "name:ben ray" {
		t.Errorf("first: %+v", first)
	}
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	if !first.Created.Equal(time.Date(2026, 10, 1, 9, 0, 0, 0, kolkata)) {
		t.Errorf("created: %v", first.Created)
	}
	if !strings.Contains(first.Description, "First line") || !strings.Contains(first.Description, "Second") || !strings.HasPrefix(first.Description, "<p>") {
		t.Errorf("description: %q", first.Description)
	}
	if v := fieldValues(first); v["Budget"] != 1250.5 || v["Area"] != "design" || v["Signed off"] != true {
		t.Errorf("values: %v", v)
	}
	if first.Metadata["link"] != "https://x.test/app/task/u1" || first.Metadata["type"] != "Task" {
		t.Errorf("metadata: %v", first.Metadata)
	}
	if launch.Type != importProvider.TaskMilestone || launch.DueDay.String() != "2026-11-01" {
		t.Errorf("milestone: %+v", launch)
	}
	sub := s.children[tb.id+":q4-12"]
	if len(sub) != 1 || sub[0].Name != "Draft the FAQ" || sub[0].ParentTaskID != tb.id+":q4-12" || sub[0].SourceKey != "Q4-13" {
		t.Errorf("sub-task: %+v", sub)
	}
	p := s.plan
	if p.TaskCount != 2 || p.SubtaskCount != 1 || p.UserCount != 2 || len(p.Warnings) != 0 ||
		strings.Join(p.StatusValues, "|") != "In progress|To do" || strings.Join(p.PriorityValues, "|") != "high|low|medium" {
		t.Errorf("plan: %+v", p)
	}
	people := map[string]int{}
	for _, person := range p.People {
		people[person.Name] = person.Rows
	}
	if people["Ana Lima"] != 3 || people["Ben Ray"] != 2 {
		t.Errorf("people: %v", p.People)
	}
}

// Asana's export reads too: its Task IDs, a parent named by its title,
// Completed At, an assignee's address beside them (and on another row, the
// name alone), an assignee who is an address, and a parent it doesn't have.
func TestAsanasExportReadsToo(t *testing.T) {
	tb, s := sheetOf(t, "Task ID,Created At,Completed At,Last Modified,Name,Section/Column,Assignee,Assignee Email,Start Date,Due Date,Tags,Notes,Projects,Parent task\n"+
		"1201,2026-10-01,,2026-10-05,Plan the launch,Doing,Ana Lima,ana@acme.test,,2026-10-24,launch,Some notes,Q4,\n"+
		"1202,2026-10-02,2026-10-06,2026-10-06,Book the venue,Done,Ana Lima,,,,,,Q4,Plan the launch\n"+
		"1203,2026-10-02,,,Order swag,Doing,ben@acme.test,,,,,,Q4,Nope\n", nil)
	if len(s.tasks) != 3 || s.tasks[0].SourceID != tb.id+":1201" || s.tasks[1].SourceID != tb.id+":1203" {
		t.Fatalf("top level: %+v", s.tasks)
	}
	venue := s.children[tb.id+":1201"]
	if len(venue) != 1 || !venue[0].Completed || venue[0].Status != "Done" || strings.Join(venue[0].AssigneeIds, "") != "email:ana@acme.test" {
		t.Errorf("the venue: %+v", venue)
	}
	if got := strings.Join(s.tasks[1].AssigneeIds, ""); got != "email:ben@acme.test" {
		t.Errorf("an address as the assignee: %q", got)
	}
	if s.tasks[0].SourceKey != "" || s.tasks[0].DueDay.String() != "2026-10-24" || s.tasks[0].Updated.IsZero() {
		t.Errorf("plan the launch: %+v", s.tasks[0])
	}
	if len(s.people) != 2 || s.people[0].Email != "ana@acme.test" || s.people[0].DisplayName != "Ana Lima" || s.people[1].DisplayName != "ben" {
		t.Errorf("people: %+v", s.people)
	}
	if len(s.project.Fields) != 1 || s.project.Fields[0].Name != "Projects" || s.project.Fields[0].Type != importProvider.FieldSelect {
		t.Errorf("fields: %+v", s.project.Fields)
	}
	if len(s.plan.Warnings) != 1 || !strings.Contains(s.plan.Warnings[0], `"Nope"`) {
		t.Errorf("warnings: %q", s.plan.Warnings)
	}
	for _, c := range s.plan.Columns {
		if c.Name == "Assignee Email" && c.Property != string(propEmail) {
			t.Errorf("Assignee Email: %+v", c)
		}
	}
}

// A row whose parent can't be comes in on its own, and the plan says why: a
// parent the file doesn't have or the row itself, rows that are each other's
// parents, a key two rows share, and a row with no title.
func TestParentsThatCantBeComeInOnTheirOwn(t *testing.T) {
	tb, s := sheetOf(t, "Key,Title,Parent\nA-1,One,A-2\nA-2,Two,A-1\nA-3,Three,A-3\nA-1,Dup,\n,No key,A-1\nA-5,,\n", nil)
	id := func(local string) string { return tb.id + ":" + local }
	var top []string
	for _, st := range s.tasks {
		top = append(top, st.SourceID)
	}
	want := []string{id("a-2"), id("a-3"), id("line:5"), "__sub__:" + id("a-1"), "__sub__:" + id("a-2")}
	if strings.Join(top, " ") != strings.Join(want, " ") {
		t.Errorf("top level:\n got %v\nwant %v", top, want)
	}
	if c := s.children[id("a-2")]; len(c) != 1 || c[0].SourceID != id("a-1") {
		t.Errorf("under A-2: %+v", c)
	}
	if c := s.children[id("a-1")]; len(c) != 1 || c[0].Name != "No key" {
		t.Errorf("under A-1: %+v", c)
	}
	w := strings.Join(s.plan.Warnings, "\n")
	for _, part := range []string{"1 rows have no title", "1 rows repeat another row's key", `1 rows name a parent the file doesn't have (such as "A-3")`, "1 groups of rows are each other's parents"} {
		if !strings.Contains(w, part) {
			t.Errorf("warnings lack %q:\n%s", part, w)
		}
	}
}

// The admin's choice of a column's kind wins over the guess, and skip leaves
// the column out; a kind no field has is no choice.
func TestTheAdminsChoicesWin(t *testing.T) {
	_, s := sheetOf(t, ourExport, importProvider.JobOptions{"columns": map[string]any{"area": "text", "Budget (USD)": "skip", "Signed off": "bogus"}})
	kinds := map[string]string{}
	for _, f := range s.project.Fields {
		kinds[f.Name] = f.Type
		if f.Name == "Area" && f.Options != nil {
			t.Errorf("a text field has no options: %+v", f.Options)
		}
	}
	if len(kinds) != 2 || kinds["Area"] != importProvider.FieldText || kinds["Signed off"] != importProvider.FieldCheckbox {
		t.Errorf("fields: %v", kinds)
	}
	for _, c := range s.plan.Columns {
		if c.Name == "Budget (USD)" && (c.Kind != importProvider.KindSkip || c.Guess != importProvider.FieldMoney || c.Currency != "USD" || c.Values != 1) {
			t.Errorf("Budget: %+v", c)
		}
	}
	if _, ok := fieldValues(s.tasks[0])["Budget"]; ok {
		t.Error("a skipped column brings no values")
	}
}

// A column the admin makes people takes the addresses beside it, and names
// the same person on every row; left a guess, both are fields.
func TestAPeopleColumnTakesItsAddresses(t *testing.T) {
	in := "Title,Reviewer,Reviewer Email\nA,Ana Lima,ana@acme.test\nB,Ana Lima,\n"
	_, s := sheetOf(t, in, importProvider.JobOptions{"columns": map[string]any{"Reviewer": "person"}})
	if len(s.project.Fields) != 1 || s.project.Fields[0].Type != importProvider.FieldPerson {
		t.Fatalf("fields: %+v", s.project.Fields)
	}
	for _, st := range s.tasks {
		if v := fieldValues(st)["Reviewer"]; v != "email:ana@acme.test" {
			t.Errorf("%s: %v", st.Name, v)
		}
	}
	if len(s.people) != 1 || s.plan.People[0].Rows != 2 {
		t.Errorf("people: %+v", s.plan.People)
	}
	_, s = sheetOf(t, in, nil)
	if len(s.project.Fields) != 2 || s.project.Fields[0].Type != importProvider.FieldSelect {
		t.Errorf("guessed: %+v", s.project.Fields)
	}
}

// A date or an estimate a task's column can't read is kept in its
// description, and the plan says how many.
func TestADateItCantReadIsKept(t *testing.T) {
	_, s := sheetOf(t, "Title,Due date,Estimate (hours)\nA,10/24/2026,lots\nB,2026-10-24,2\n", nil)
	a := s.tasks[0]
	if !a.DueDay.IsZero() || a.EstimateMinutes != 0 || len(a.Fields) != 2 || a.Fields[0].Field.Type != "" || a.Fields[0].Text != "10/24/2026" || a.Fields[1].Text != "lots" {
		t.Errorf("kept: %+v", a.Fields)
	}
	w := strings.Join(s.plan.Warnings, "\n")
	if !strings.Contains(w, `1 dates couldn't be read (such as "10/24/2026")`) || !strings.Contains(w, `1 estimates aren't a number of hours (such as "lots")`) {
		t.Errorf("warnings: %s", w)
	}
}
