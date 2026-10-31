//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"

	entityLinkController "github.com/akashc777/OneCamp/controllers/EntityLink"
	githubController "github.com/akashc777/OneCamp/controllers/GitHub"
	taskController "github.com/akashc777/OneCamp/controllers/Task"
	"github.com/akashc777/OneCamp/tests/integration"
)

// V-52: an empty list is answered [], never null, whichever reader built it.
// About a hundred answers came from readers that return a nil slice for none
// (rows appended to a nil slice, a reader's "nothing" as nil), and a client reads
// null as "not read yet" (V-51: a project with no milestones loaded for ever).
// The writer, helpers.WriteJSON, now answers a nil list in its envelopes as [].
// Two such readers, over HTTP as the member asks them, for a project with none: its
// linked docs and boards (Dgraph, nil for none) and its linked GitHub repositories
// (Postgres rows appended to a nil slice). And one that always answered [] (the
// person's saved views) still does.
func TestV52AnEmptyListAnswersAnEmptyArray(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	s := newWA2Scale(t, env, integration.SetupDgraph(t))

	code, out := s.call("ana", http.MethodGet, "/?scope=mine", taskController.GetTaskViews, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("Ana reads her saved views, having none: %d %s", code, out)
	}
	var views struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &views); err != nil {
		t.Fatalf("the views answer isn't JSON: %v: %s", err, out)
	}
	if string(views.Data) != "[]" {
		t.Fatalf("no saved views answers data %s, want []", views.Data)
	}

	code, out = s.call("ana", http.MethodGet, "/", entityLinkController.GetSourceLinks, map[string]string{"source_type": "project", "source_uuid": s.project}, nil)
	if code != http.StatusOK {
		t.Fatalf("Ana reads the links of a project with none: %d %s", code, out)
	}
	var links struct {
		Data struct {
			Docs   json.RawMessage `json:"docs"`
			Boards json.RawMessage `json:"boards"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		t.Fatalf("the links answer isn't JSON: %v: %s", err, out)
	}
	if string(links.Data.Docs) != "[]" || string(links.Data.Boards) != "[]" {
		t.Fatalf("a project with no links answers docs %s and boards %s, want [] and []", links.Data.Docs, links.Data.Boards)
	}

	code, out = s.call("ana", http.MethodGet, "/", githubController.HandleGetLinkedRepos, map[string]string{"projectId": s.project}, nil)
	if code != http.StatusOK {
		t.Fatalf("Ana reads the GitHub repositories of a project with none: %d %s", code, out)
	}
	var repos struct {
		Links json.RawMessage `json:"links"`
	}
	if err := json.Unmarshal([]byte(out), &repos); err != nil {
		t.Fatalf("the repositories answer isn't JSON: %v: %s", err, out)
	}
	if string(repos.Links) != "[]" {
		t.Fatalf("a project with no linked repositories answers links %s, want []", repos.Links)
	}
}
