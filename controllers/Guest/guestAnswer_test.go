package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
)

// decodeObject is an encoded guest answer read back as the web app reads it.
func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, raw)
	}
	return out
}

// Every guest read carries whose workspace it is beside its own fields, as
// one object, whatever shape the read's data has.
func TestWithWorkspaceNameAddsTheNameBesideTheData(t *testing.T) {
	view := &guestBusiness.GuestProjectView{Project: "Website", ProjectUUID: "b3f1c2de-0000-4000-8000-000000000001", Columns: []guestBusiness.GuestColumn{}, Updates: []guestBusiness.GuestUpdate{}}
	cases := map[string]struct {
		data   any
		fields map[string]any
	}{
		"a struct":      {view, map[string]any{"project": "Website", "project_uuid": view.ProjectUUID}},
		"a map":         {map[string]any{"available": true}, map[string]any{"available": true}},
		"an empty map":  {map[string]any{}, map[string]any{}},
		"a nested list": {map[string]any{"comments": []string{"a", "b"}}, map[string]any{"comments": []any{"a", "b"}}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(withWorkspaceName(c.data, "Acme Studio"))
			if err != nil {
				t.Fatal(err)
			}
			got := decodeObject(t, raw)
			if got["workspace_name"] != "Acme Studio" {
				t.Errorf("workspace_name = %v in %s", got["workspace_name"], raw)
			}
			for k, want := range c.fields {
				gotV, _ := json.Marshal(got[k])
				wantV, _ := json.Marshal(want)
				if string(gotV) != string(wantV) {
					t.Errorf("%s = %s, want %s (the data's own fields are kept)", k, gotV, wantV)
				}
			}
			if n := strings.Count(string(raw), `"workspace_name"`); n != 1 {
				t.Errorf("workspace_name written %d times: %s", n, raw)
			}
		})
	}
}

// No name is an empty one, still there: the page then uses its own build's.
func TestWithWorkspaceNameKeepsAnEmptyName(t *testing.T) {
	raw, err := json.Marshal(withWorkspaceName(map[string]any{"available": true}, ""))
	if err != nil {
		t.Fatal(err)
	}
	got := decodeObject(t, raw)
	if name, ok := got["workspace_name"]; !ok || name != "" {
		t.Errorf("workspace_name = %v (present %v), want \"\"", name, ok)
	}
}

// A name is a string like any other: quotes, markup and backslashes arrive as
// written and break nothing around them.
func TestWithWorkspaceNameEscapesTheName(t *testing.T) {
	name := `Bob's "Studio" <b>&</b> \ Co`
	raw, err := json.Marshal(withWorkspaceName(map[string]any{"title": "Plan"}, name))
	if err != nil {
		t.Fatal(err)
	}
	got := decodeObject(t, raw)
	if got["workspace_name"] != name || got["title"] != "Plan" {
		t.Errorf("read back %v", got)
	}
}

// Data that isn't an object has nowhere to put the name, and is written as it is.
func TestWithWorkspaceNameLeavesOtherDataAlone(t *testing.T) {
	for name, c := range map[string]struct {
		data any
		want string
	}{
		"a list":  {[]string{"a"}, `["a"]`},
		"nothing": {nil, `null`},
		"a word":  {"x", `"x"`},
	} {
		raw, err := json.Marshal(withWorkspaceName(c.data, "Acme"))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != c.want {
			t.Errorf("%s: %s, want %s", name, raw, c.want)
		}
	}
}

// The answer as it leaves the server: WriteJSON's envelope, the name from
// ORG_NAME, and every field of a real guest view still in place.
func TestWriteGuestReadAnswersWithTheWorkspaceName(t *testing.T) {
	t.Setenv("ORG_NAME", "  Acme   Studio ")
	view := &guestBusiness.GuestChannelView{
		Channel: "launch", CanPost: true,
		Messages: []guestBusiness.GuestMessage{{ID: "m1", Author: "Ada", Text: "Hi", CreatedAt: time.Unix(1700000000, 0).UTC()}},
	}
	rec := httptest.NewRecorder()
	writeGuestRead(rec, view)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Data struct {
			WorkspaceName string                       `json:"workspace_name"`
			Channel       string                       `json:"channel"`
			CanPost       bool                         `json:"can_post"`
			Messages      []guestBusiness.GuestMessage `json:"messages"`
			HasMore       *bool                        `json:"has_more"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v\n%s", err, rec.Body.String())
	}
	d := body.Data
	if d.WorkspaceName != "Acme Studio" || d.Channel != "launch" || !d.CanPost || d.HasMore == nil ||
		len(d.Messages) != 1 || d.Messages[0].Text != "Hi" || !d.Messages[0].CreatedAt.Equal(view.Messages[0].CreatedAt) {
		t.Errorf("answered %s", rec.Body.String())
	}
}
