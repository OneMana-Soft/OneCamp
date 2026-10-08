package todoist

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A full sync from Todoist API v1: yes/no fields are true/false, missing
// ids are null, and /sync keeps the names items and notes.
const syncV1 = `{"full_sync":true,"sync_token":"abc",
 "user":{"id":"2671355","full_name":"Ada","email":"ada@example.com","image_id":"img"},
 "projects":[{"id":"6XGgm6PHrGgMpCFX","name":"Launch","color":"lime_green","parent_id":null,"is_archived":false,"is_deleted":false},
             {"id":"6XGgOld","name":"Old","is_archived":true,"is_deleted":true}],
 "sections":[{"id":"6fFPHV272WWh3gpW","name":"Doing","project_id":"6XGgm6PHrGgMpCFX","is_collapsed":false,"is_deleted":false,"is_archived":false}],
 "items":[{"id":"6XGgmFVcrG5RRjVr","content":"Welcome the new hire","description":"","project_id":"6XGgm6PHrGgMpCFX","section_id":"6fFPHV272WWh3gpW",
   "parent_id":null,"priority":4,"labels":["onboarding"],"responsible_uid":null,"checked":false,"is_deleted":false,"added_at":"2025-01-15T10:30:00Z",
   "completed_at":null,"updated_at":"2025-01-16T10:30:00Z","due":{"date":"2025-02-12","is_recurring":false,"lang":"en","string":"tomorrow"},"deadline":null},
  {"id":"6XGgDone","content":"Send the contract","project_id":"6XGgm6PHrGgMpCFX","checked":true,"is_deleted":false,"completed_at":"2025-01-20T09:00:00Z","due":null}],
 "notes":[{"id":"6X7gfQHG59V8CJJV","posted_uid":"2671355","item_id":"6XGgmFVcrG5RRjVr","content":"Note","is_deleted":false,"posted_at":"2014-10-01T14:54:55.000000Z",
   "file_attachment":{"file_type":"text/plain","file_name":"File1.txt","file_size":1234,"file_url":"https://example.com/File1.txt","upload_state":"completed"},"reactions":{}}],
 "collaborators":[{"email":"grace@example.com","full_name":"Grace","id":"123456","is_deleted":false,"timezone":"Europe/London"}],
 "labels":[{"id":"l1","name":"onboarding","is_deleted":false}]}`

func TestSyncV1(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.Path
		if err := r.ParseForm(); err != nil || r.Form.Get("sync_token") != "*" || r.Form.Get("resource_types") != `["projects","items"]` {
			t.Errorf("form: %v %v", r.Form, err)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, syncV1)
	}))
	defer srv.Close()
	defer func(old string) { syncURL = old }(syncURL)
	syncURL = srv.URL + "/api/v1/sync"

	snap, err := New().sync(context.Background(), "tok", []string{"projects", "items"})
	if err != nil {
		t.Fatalf("a v1 answer decodes: %v", err)
	}
	if asked != "POST /api/v1/sync" {
		t.Errorf("asked %s", asked)
	}
	if got := snap.activeProjects(); len(got) != 1 || got[0].Name != "Launch" || got[0].IsArchived.on() {
		t.Errorf("deleted projects are left out: %+v", got)
	}
	if len(snap.Items) != 2 || snap.Items[0].Checked.on() || !snap.Items[1].Checked.on() || snap.Items[0].ParentID != "" {
		t.Errorf("items: %+v", snap.Items)
	}
	st := buildSourceTask(snap, &snap.Items[1], map[string]string{})
	if !st.Completed {
		t.Error("a checked item is a completed task")
	}
	if len(snap.Notes) != 1 || snap.Notes[0].FileAttachment == nil || snap.Notes[0].FileAttachment.FileName != "File1.txt" {
		t.Errorf("notes: %+v", snap.Notes)
	}
}

func TestFlagReadsBothForms(t *testing.T) {
	for raw, want := range map[string]bool{"true": true, "false": false, "1": true, "0": false, `"1"`: true, "null": false} {
		var f flag
		if err := f.UnmarshalJSON([]byte(raw)); err != nil || f.on() != want {
			t.Errorf("%s: %v %v", raw, f, err)
		}
	}
}
