package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// A re-check reads each issue as Jira has it now, by the import's own rules:
// a security level keeps it to its reporter (and its assignee); a comment
// kept to a role or group, and a service desk's internal note, are kept;
// an issue Jira no longer answers for is gone, a comment it no longer has is
// missing; and Jira refusing the credentials is a refused token.
func TestARecheckReadsEachIssueAsJiraHasItNow(t *testing.T) {
	refuse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{}`)
			return
		}
		switch r.URL.Path {
		case "/rest/api/3/issue/HR-1":
			fmt.Fprint(w, `{"key":"HR-1","fields":{"security":{"id":"10","name":"HR only"},"reporter":{"accountId":"u-rae"},
				"assignee":{"accountId":"u-pip"},"creator":{"accountId":"u-cy"},"project":{"key":"HR"},"issuetype":{"name":"Task"}}}`)
		case "/rest/api/3/issue/HR-2":
			fmt.Fprint(w, `{"key":"HR-2","fields":{"reporter":{"accountId":"u-rae"},"creator":{"accountId":"u-cy"},
				"project":{"key":"HR"},"issuetype":{"name":"Task"}}}`)
		case "/rest/api/3/issue/HR-2/comment":
			if !strings.Contains(r.URL.Query().Get("expand"), "properties") {
				t.Errorf("comments read without their properties, where a service desk keeps an internal note: %s", r.URL)
			}
			fmt.Fprint(w, `{"startAt":0,"maxResults":100,"total":4,"comments":[{"id":"c1"},
				{"id":"c2","visibility":{"type":"role","value":"Developers"}},{"id":"c3","jsdPublic":false},
				{"id":"c4","properties":[{"key":"sd.public.comment","value":{"internal":true}}]}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`)
		}
	}))
	defer srv.Close()
	got, err := New().recheck(context.Background(), "tok", srv.URL, srv.URL, []string{"HR-1", "HR-2", "HR-9"},
		map[string][]string{"HR-2": {"c1", "c2", "c3", "c4", "c8"}})
	if err != nil {
		t.Fatal(err)
	}
	hr1 := got["HR-1"].Task
	if hr1.Restricted == "" || !reflect.DeepEqual(hr1.Audience, []string{"u-rae"}) || !reflect.DeepEqual(hr1.AssigneeIds, []string{"u-pip"}) ||
		hr1.CreatedBy != "u-cy" || hr1.ProjectSourceID != "HR" {
		t.Errorf("HR-1 under a security level reads as %+v", hr1)
	}
	if hr2 := got["HR-2"]; hr2.Gone || hr2.Task.Restricted != "" {
		t.Errorf("HR-2, open to its project, reads as %+v", hr2)
	}
	if want := map[string]string{"c2": "a project role or group", "c3": "an internal note", "c4": "an internal note"}; !reflect.DeepEqual(got["HR-2"].KeptComments, want) {
		t.Errorf("HR-2's kept comments: %v; want %v", got["HR-2"].KeptComments, want)
	}
	if !reflect.DeepEqual(got["HR-2"].MissingComments, []string{"c8"}) {
		t.Errorf("HR-2's comments Jira no longer has: %v; want [c8]", got["HR-2"].MissingComments)
	}
	if !got["HR-9"].Gone {
		t.Errorf("HR-9, which Jira doesn't answer for, reads as %+v; want gone", got["HR-9"])
	}
	refuse = true
	if _, err := New().recheck(context.Background(), "tok", srv.URL, srv.URL, []string{"HR-1"}, nil); !errors.Is(err, importProvider.ErrTokenRejected) {
		t.Errorf("Jira refusing the credentials: %v; want a refused token", err)
	}
}
