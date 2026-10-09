package todoist

import (
	"encoding/json"
	"strings"
	"testing"
)

// An account with two projects: Launch (Grace works in it) and Garden
// (Linus works in it).
const twoProjects = `{
 "user":{"id":"me","full_name":"Ada","email":"ada@example.com"},
 "projects":[{"id":"launch","name":"Launch"},{"id":"garden","name":"Garden"},{"id":"gone","name":"Gone","is_deleted":true}],
 "sections":[{"id":"s1","name":"Doing","project_id":"launch"},{"id":"s2","name":"Beds","project_id":"garden"}],
 "items":[{"id":"i1","content":"Ship","project_id":"launch","responsible_uid":"grace"},
          {"id":"i2","content":"Ship docs","project_id":"launch","parent_id":"i1"},
          {"id":"i3","content":"Water","project_id":"garden","responsible_uid":"linus"}],
 "notes":[{"id":"n1","item_id":"i2","posted_uid":"me","content":"done"},{"id":"n2","item_id":"i3","posted_uid":"linus","content":"wet"}],
 "collaborators":[{"id":"grace","full_name":"Grace"},{"id":"linus","full_name":"Linus"}]}`

func TestAPickedTodoistProjectIsTheOnlyOneImported(t *testing.T) {
	var full todoistSnapshot
	if err := json.Unmarshal([]byte(twoProjects), &full); err != nil {
		t.Fatal(err)
	}
	ids := func(n int, id func(i int) string) string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, id(i))
		}
		return strings.Join(out, ",")
	}

	s, err := full.scoped("launch")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(len(s.Projects), func(i int) string { return s.Projects[i].ID }); got != "launch" {
		t.Errorf("projects: %s", got)
	}
	if got := ids(len(s.Items), func(i int) string { return s.Items[i].ID }); got != "i1,i2" {
		t.Errorf("items, subtasks included: %s", got)
	}
	if got := ids(len(s.Notes), func(i int) string { return s.Notes[i].ID }); got != "n1" {
		t.Errorf("notes: %s", got)
	}
	if got := ids(len(s.Sections), func(i int) string { return s.Sections[i].ID }); got != "s1" {
		t.Errorf("sections: %s", got)
	}
	// The people are the project's: Grace works in it, Linus doesn't.
	if got := ids(len(s.Collaborators), func(i int) string { return s.Collaborators[i].ID }); got != "grace" || s.User.ID != "me" {
		t.Errorf("people: %s, user %s", got, s.User.ID)
	}

	if all, err := full.scoped(""); err != nil || all != &full {
		t.Errorf("no pick is the whole account: %v", err)
	}
	for _, gone := range []string{"gone", "never-was"} {
		if _, err := full.scoped(gone); err == nil || !strings.Contains(err.Error(), "isn't there any more") {
			t.Errorf("%s: %v", gone, err)
		}
	}
}
