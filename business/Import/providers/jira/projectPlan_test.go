package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// Head 3 part 2 (linear-15's plan from Jira): a project read from Jira comes
// with its lead leading its plan. Jira keeps no dates, status or priority on
// a project, so the lead is all of it, and a project Jira shows no lead for
// has no plan. Before, every Jira project came with no plan.
func TestAJiraProjectsLeadLeadsItsPlan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/project/search" || !strings.Contains(r.URL.Query().Get("expand"), "lead") {
			t.Errorf("request: %s", r.URL)
		}
		fmt.Fprint(w, `{"isLast": true, "values": [
			{"id": "1", "key": "ENG", "name": "Engineering", "lead": {"accountId": "acc-1", "accountType": "atlassian", "displayName": "Maya"}},
			{"id": "2", "key": "OPS", "name": "Operations"}]}`)
	}))
	defer srv.Close()
	ctx, p, job := context.Background(), New(), &importModels.Job{Id: uuid.New()}
	projects, err := p.listProjects(ctx, "tok", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan importProvider.SourceProject, 4)
	if err := p.sendProjects(ctx, job, "tok", srv.URL, projects, "", "", out); err != nil {
		t.Fatal(err)
	}
	close(out)
	got := map[string]*importProvider.SourcePlan{}
	for sp := range out {
		got[sp.SourceID] = sp.Plan
	}
	if len(got) != 2 {
		t.Fatalf("%d projects came: %v", len(got), got)
	}
	if pl := got["ENG"]; pl == nil || !reflect.DeepEqual(*pl, importProvider.SourcePlan{LeadID: "acc-1"}) {
		t.Errorf("ENG's plan: %+v, want its lead acc-1", pl)
	}
	if pl := got["OPS"]; pl != nil {
		t.Errorf("OPS has no lead in Jira, and no plan here: %+v", pl)
	}
}

// A Jira issue comes with when it entered the status it comes in, as Jira
// keeps it: a done issue when it was resolved (or, with no resolution, when
// it reached done), any other when its status's category last changed (a
// reopened issue keeps its old resolution date, which no longer says where
// it stands). Every search asks for both.
func TestAJiraIssueComesWithWhenItEnteredItsStatus(t *testing.T) {
	for _, f := range []string{"resolutiondate", "statuscategorychangedate"} {
		if !strings.Contains(","+issueFields+",", ","+f+",") {
			t.Errorf("issues aren't asked for %s: %s", f, issueFields)
		}
	}
	issue := func(key, category, times string) string {
		return `{"id": "1", "key": "` + key + `", "fields": {"summary": "` + key + `", "status": {"name": "S", "statusCategory": {"key": "` + category + `"}},
			"issuetype": {"name": "Task"}, "created": "2026-09-01T09:00:00.000+0000", "updated": "2026-10-20T09:00:00.000+0000"` + times + `}}`
	}
	cases := []struct {
		name, issue string
		want        string
	}{
		{"resolved", issue("ENG-1", "done", `, "resolutiondate": "2026-10-09T12:00:00.000+0000", "statuscategorychangedate": "2026-10-08T12:00:00.000+0000"`), "2026-10-09T12:00:00Z"},
		{"done with no resolution", issue("ENG-2", "done", `, "resolutiondate": null, "statuscategorychangedate": "2026-10-08T12:00:00.000+0000"`), "2026-10-08T12:00:00Z"},
		{"reopened", issue("ENG-3", "indeterminate", `, "resolutiondate": "2026-09-05T12:00:00.000+0000", "statuscategorychangedate": "2026-10-04T12:00:00.000+0000"`), "2026-10-04T12:00:00Z"},
		{"no times", issue("ENG-4", "new", ``), ""},
	}
	for _, c := range cases {
		var iss jiraIssue
		if err := json.Unmarshal([]byte(c.issue), &iss); err != nil {
			t.Fatal(err)
		}
		got := buildSourceTask(&iss, "ENG", "https://acme.atlassian.net", siteFields{}).StatusSince
		switch {
		case c.want == "" && got != nil:
			t.Errorf("%s: %v, want none", c.name, got)
		case c.want != "":
			want, _ := time.Parse(time.RFC3339, c.want)
			if got == nil || !got.Equal(want) {
				t.Errorf("%s: %v, want %v", c.name, got, want)
			}
		}
	}
}
