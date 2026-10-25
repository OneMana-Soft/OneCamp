package notion

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/google/uuid"
)

// answersByPath is a Notion that answers each path it knows, and 404 the rest.
type answersByPath map[string]string

func (a answersByPath) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := a[strings.TrimPrefix(r.URL.RequestURI(), "/v1")]
	status := http.StatusOK
	if !ok {
		status, body = http.StatusNotFound, `{"object":"error","status":404,"code":"object_not_found"}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// A database of tasks comes across as a project for the person importing
// alone: Notion's API doesn't say who a database is shared with, so it can't
// come with everyone in the workspace, who may never have been able to open it.
func TestANotionDatabaseComesAcrossToTheImporterAlone(t *testing.T) {
	saved := importProvider.SharedAPIClient
	t.Cleanup(func() { importProvider.SharedAPIClient = saved })
	importProvider.SharedAPIClient = &http.Client{Transport: answersByPath{
		"/databases/db1": `{"object":"database","id":"db1","url":"https://www.notion.so/db1","title":[{"plain_text":"Salaries"}],"properties":{}}`,
		"/users?page_size=100": `{"object":"list","has_more":false,"results":[
			{"object":"user","id":"ada","type":"person","name":"Ada","person":{"email":"ada@example.com"}},
			{"object":"user","id":"grace","type":"person","name":"Grace","person":{"email":"grace@example.com"}}]}`,
	}}
	p := &Provider{rl: noWait{}, dbCache: map[uuid.UUID][]string{}}

	got := p.projectsOf(context.Background(), "tok", []string{"db1"})
	if len(got) != 1 || got[0].SourceID != "db1" || got[0].Name != "Salaries" {
		t.Fatalf("projects: %+v", got)
	}
	if len(got[0].MemberIds) != 0 || len(got[0].AdminIds) != 0 {
		t.Errorf("the project comes with %v and admins %v, want no one but the importer", got[0].MemberIds, got[0].AdminIds)
	}
}
