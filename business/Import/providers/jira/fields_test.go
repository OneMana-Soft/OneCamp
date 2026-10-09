package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

// The site's fields as GET /rest/api/3/field lists them.
const siteFieldsJSON = `[
 {"id":"summary","name":"Summary","custom":false,"schema":{"type":"string","system":"summary"}},
 {"id":"customfield_10001","name":"Channel","custom":true,"schema":{"type":"option","custom":"com.atlassian.jira.plugin.system.customfieldtypes:select"}},
 {"id":"customfield_10002","name":"Platforms","custom":true,"schema":{"type":"array","items":"option","custom":"com.atlassian.jira.plugin.system.customfieldtypes:multiselect"}},
 {"id":"customfield_10003","name":"Areas","custom":true,"schema":{"type":"array","items":"string","custom":"com.atlassian.jira.plugin.system.customfieldtypes:labels"}},
 {"id":"customfield_10016","name":"Story point estimate","custom":true,"schema":{"type":"number","custom":"com.pyxis.greenhopper.jira:jsw-story-points"}},
 {"id":"customfield_10004","name":"Notes","custom":true,"schema":{"type":"string","custom":"com.atlassian.jira.plugin.system.customfieldtypes:textarea"}},
 {"id":"customfield_10005","name":"Brief","custom":true,"schema":{"type":"string","custom":"com.atlassian.jira.plugin.system.customfieldtypes:url"}},
 {"id":"customfield_10006","name":"Launch","custom":true,"schema":{"type":"date","custom":"com.atlassian.jira.plugin.system.customfieldtypes:datepicker"}},
 {"id":"customfield_10007","name":"Go live","custom":true,"schema":{"type":"datetime","custom":"com.atlassian.jira.plugin.system.customfieldtypes:datetime"}},
 {"id":"customfield_10008","name":"Reviewer","custom":true,"schema":{"type":"user","custom":"com.atlassian.jira.plugin.system.customfieldtypes:userpicker"}},
 {"id":"customfield_10009","name":"Approvers","custom":true,"schema":{"type":"array","items":"user","custom":"com.atlassian.jira.plugin.system.customfieldtypes:multiuserpicker"}},
 {"id":"customfield_10020","name":"Sprint","custom":true,"schema":{"type":"array","items":"json","custom":"com.pyxis.greenhopper.jira:gh-sprint"}},
 {"id":"customfield_10019","name":"Rank","custom":true,"schema":{"type":"any","custom":"com.pyxis.greenhopper.jira:gh-lexo-rank"}}
]`

const issueJSON = `{"id":"1","key":"LAUNCH-1","fields":{"summary":"Launch post","status":{"name":"In Progress"},"issuetype":{"name":"Task"},
 "customfield_10001":{"self":"x","id":"20001","value":"Blog"},
 "customfield_10002":[{"id":"20101","value":"iOS"},{"id":"20102","value":"Web"}],
 "customfield_10003":["launch","q4"],
 "customfield_10016":5,
 "customfield_10004":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Short and warm"}]}]},
 "customfield_10005":"https://docs.example.com/b",
 "customfield_10006":"2026-11-02",
 "customfield_10007":"2026-11-03T09:00:00.000+0530",
 "customfield_10008":{"accountId":"acc-maya","displayName":"Maya"},
 "customfield_10009":[{"accountId":"acc-ada"},{"accountId":"acc-grace"}],
 "customfield_10020":[{"id":3,"name":"Sprint 3"}],
 "customfield_10019":"0|i0001:",
 "customfield_10099":null}}`

func siteFieldsOf(t *testing.T) siteFields {
	t.Helper()
	var metas []jiraFieldMeta
	if err := json.Unmarshal([]byte(siteFieldsJSON), &metas); err != nil {
		t.Fatal(err)
	}
	out := siteFields{}
	for _, m := range metas {
		if kindOf(m) != "" {
			out[m.ID] = m
		}
	}
	return out
}

func TestKindOf(t *testing.T) {
	fields := siteFieldsOf(t)
	if len(fields) != 10 || fields["customfield_10020"].ID != "" || fields["summary"].ID != "" {
		t.Fatalf("custom fields of kinds OneCamp has, without sprints, ranks or system fields: %v", fields)
	}
}

func TestIssueCustomValues(t *testing.T) {
	var iss jiraIssue
	if err := json.Unmarshal([]byte(issueJSON), &iss); err != nil {
		t.Fatal(err)
	}
	if iss.Fields.Summary != "Launch post" || len(iss.Custom) != 12 {
		t.Fatalf("the issue reads as before, keeping the custom values that are set: %+v %d", iss.Fields, len(iss.Custom))
	}
	st := buildSourceTask(&iss, "LAUNCH", "https://acme.atlassian.net", siteFieldsOf(t))
	got := map[string]any{}
	for _, v := range st.Fields {
		got[v.Field.Name] = v.Value
	}
	want := map[string]any{
		"Channel": "20001", "Platforms": []string{"20101", "20102"}, "Areas": []string{"launch", "q4"},
		"Story point estimate": 5.0, "Notes": "Short and warm", "Brief": "https://docs.example.com/b",
		"Launch": "2026-11-02", "Go live": "2026-11-03", "Reviewer": "acc-maya", "Approvers": "acc-ada",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values:\n got %v\nwant %v", got, want)
	}
	for _, v := range st.Fields {
		if v.Field.Name == "Platforms" && !reflect.DeepEqual(v.Field.Options, []importProvider.SourceOption{{SourceID: "20101", Label: "iOS"}, {SourceID: "20102", Label: "Web"}}) {
			t.Errorf("a value brings the options it names: %+v", v.Field.Options)
		}
		if v.Field.Name == "Reviewer" && v.Text != "Maya" {
			t.Errorf("a person comes with their name, for when they can't come across: %+v", v)
		}
	}
}

func TestSearchIssuesFollowsTokens(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/rest/api/3/search/jql" || r.URL.Query().Get("expand") != "renderedFields" || !strings.HasPrefix(r.URL.Query().Get("fields"), "*navigable,summary") {
			t.Errorf("request: %s", r.URL)
		}
		switch r.URL.Query().Get("nextPageToken") {
		case "":
			fmt.Fprint(w, `{"issues":[{"key":"A-1","fields":{}},{"key":"A-2","fields":{}}],"nextPageToken":"t1","isLast":false}`)
		case "t1":
			fmt.Fprint(w, `{"issues":[{"key":"A-3","fields":{}}],"nextPageToken":"t2","isLast":false}`)
		default:
			// A site handing out tokens past the end: nothing new.
			fmt.Fprint(w, `{"issues":[{"key":"A-3","fields":{}}],"nextPageToken":"t3","isLast":false}`)
		}
	}))
	defer srv.Close()
	var keys []string
	err := New().searchIssues(context.Background(), "tok", srv.URL, `project = "A"`, []string{"*navigable", "summary"}, true, func(iss *jiraIssue) bool {
		keys = append(keys, iss.Key)
		return true
	})
	if err != nil || strings.Join(keys, ",") != "A-1,A-2,A-3" || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("keys %v calls %d err %v", keys, calls, err)
	}
}

func TestCountIssues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/rest/api/3/search/approximate-count" || string(body) != `{"jql":"project = \"LAUNCH\""}` || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s %s", r.Method, r.URL.Path, body)
		}
		fmt.Fprint(w, `{"count":42}`)
	}))
	defer srv.Close()
	if n, err := New().countIssues(context.Background(), "tok", srv.URL, "LAUNCH"); err != nil || n != 42 {
		t.Fatalf("%d %v", n, err)
	}
}

func TestCustomFieldsReadOnceAnImport(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprint(w, siteFieldsJSON)
	}))
	defer srv.Close()
	p, job := New(), &importModels.Job{Id: uuid.New()}
	first, err1 := p.customFields(context.Background(), job, "tok", srv.URL)
	second, err2 := p.customFields(context.Background(), job, "tok", srv.URL)
	if err1 != nil || err2 != nil || len(first) != 10 || len(second) != 10 || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("%d %d fields, %d calls, %v %v", len(first), len(second), calls, err1, err2)
	}
	p.CleanupJob(job.Id.String())
	if _, ok := p.fieldsCache[job.Id]; ok {
		t.Error("a finished import's fields are let go")
	}
}

// Jira asking to slow down isn't remembered as "no custom fields": the
// chunk waits and asks again. A token that can't read the list imports
// without them.
func TestCustomFieldsWhenTheListFails(t *testing.T) {
	status := http.StatusTooManyRequests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	p, job := New(), &importModels.Job{Id: uuid.New()}
	if _, err := p.customFields(context.Background(), job, "tok", srv.URL); err == nil {
		t.Fatal("a 429 is the chunk's to retry")
	}
	if _, ok := p.fieldsCache[job.Id]; ok {
		t.Fatal("and isn't cached")
	}
	status = http.StatusBadGateway
	if _, err := p.customFields(context.Background(), job, "tok", srv.URL); err == nil {
		t.Fatal("a server error is the chunk's to retry too")
	}
	if _, ok := p.fieldsCache[job.Id]; ok {
		t.Fatal("and isn't cached either")
	}
	status = http.StatusForbidden
	if fields, err := p.customFields(context.Background(), job, "tok", srv.URL); err != nil || len(fields) != 0 {
		t.Fatalf("a 403 imports without custom fields: %v %v", fields, err)
	}
}
