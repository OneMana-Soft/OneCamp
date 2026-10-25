package todoist

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// An account sharing Launch with Grace and Garden with Linus, keeping Diary to
// itself. Linus left Launch, and Hedy's invitation to Garden is still open.
const sharedApart = `{
 "user":{"id":"me","full_name":"Ada","email":"ada@example.com"},
 "projects":[{"id":"launch","name":"Launch"},{"id":"garden","name":"Garden"},{"id":"diary","name":"Diary"}],
 "collaborators":[{"id":"grace","full_name":"Grace"},{"id":"linus","full_name":"Linus"},{"id":"hedy","full_name":"Hedy"}],
 "collaborator_states":[
  {"project_id":"launch","user_id":"me","state":"active","is_deleted":false},
  {"project_id":"launch","user_id":"grace","state":"active","is_deleted":false},
  {"project_id":"launch","user_id":"linus","state":"active","is_deleted":true},
  {"project_id":"garden","user_id":"me","state":"active","is_deleted":false},
  {"project_id":"garden","user_id":"linus","state":"active","is_deleted":false},
  {"project_id":"garden","user_id":"hedy","state":"invited","is_deleted":false}]}`

// projectMembers runs IterProjects over snapshot raw and answers each
// project's members, sorted, by project.
func projectMembers(t *testing.T, raw string) map[string]string {
	t.Helper()
	var snap todoistSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatal(err)
	}
	p := New()
	job := &importModels.Job{Id: uuid.New()}
	p.snapshot[job.Id] = &snap
	out, errs := p.IterProjects(context.Background(), job, importProvider.JobOptions{})
	got := map[string]string{}
	for sp := range out {
		ids := append([]string{}, sp.MemberIds...)
		sort.Strings(ids)
		got[sp.SourceID] = strings.Join(ids, ",")
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	return got
}

// A project comes with the people Todoist shares it with, and no one else: a
// project kept to oneself comes to the person importing alone, and someone who
// left a project or hasn't taken its invitation yet doesn't come with it.
func TestATodoistProjectComesWithOnlyThePeopleItIsSharedWith(t *testing.T) {
	got := projectMembers(t, sharedApart)
	want := map[string]string{"launch": "grace,me", "garden": "linus,me", "diary": "me"}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s comes with %q, want %q", id, got[id], w)
		}
	}
}

// When Todoist doesn't say who shares a project, the project comes to the
// person importing alone, never to everyone the account works with.
func TestATodoistProjectWithNoSharingKnownComesToTheImporterAlone(t *testing.T) {
	got := projectMembers(t, twoProjects)
	for _, id := range []string{"launch", "garden"} {
		if got[id] != "me" {
			t.Errorf("%s comes with %q, want only the importer", id, got[id])
		}
	}
}

// A picked project brings the people who share it, whether or not they have
// tasks in it, and only them.
func TestAPickedTodoistProjectBringsThePeopleWhoShareIt(t *testing.T) {
	var full todoistSnapshot
	if err := json.Unmarshal([]byte(sharedApart), &full); err != nil {
		t.Fatal(err)
	}
	s, err := full.scoped("garden")
	if err != nil {
		t.Fatal(err)
	}
	var people []string
	for _, c := range s.Collaborators {
		people = append(people, c.ID)
	}
	if got := strings.Join(people, ","); got != "linus" {
		t.Errorf("people: %s, want linus", got)
	}
	if got := strings.Join(s.peopleOf("garden"), ","); got != "me,linus" {
		t.Errorf("members: %s, want me,linus", got)
	}
}
