package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDocSearchRequestAdmitsOnlyWhatThePersonReaches(t *testing.T) {
	body, err := DocSearchRequest(DocSearchQuery{Text: " launch plan ", UserUUID: "u-1", ProjectUUIDs: []string{"p-1"}, TeamUUIDs: []string{"t-1"}})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for _, want := range []string{
		`"doc_private":false`, `"doc_created_by_user_id":"u-1"`, `"doc_reading_users":"u-1"`, `"doc_editing_users":"u-1"`,
		`"doc_commenting_users":"u-1"`, `"doc_owner_user_id":"u-1"`, `"doc_full_access_users":"u-1"`,
		`"doc_team_uuids":["t-1"]`, `"doc_space_uuid":["p-1","t-1"]`, `"minimum_should_match":1`,
		`"field":"deleted_date"`, `"doc_space_kind":"table"`, `"query":"launch plan"`, `"encoder":"html"`,
		`"_source":["doc_uuid","doc_title","doc_snippet"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the query lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"doc_body"],`) || strings.Contains(body, `"_source":true`) {
		t.Error("the body must never be sent back")
	}
	if strings.Contains(body, "owner") && strings.Count(body, `"doc_created_by_user_id"`) != 1 {
		t.Error("no owner filter was asked for")
	}
}

func TestDocSearchRequestKeepsOnePersonsDocs(t *testing.T) {
	body, _ := DocSearchRequest(DocSearchQuery{Text: "x y", UserUUID: "u-1", OwnerUUID: "o-9"})
	if !strings.Contains(body, `"doc_created_by_user_id":"o-9"`) || !strings.Contains(body, `"doc_owner_user_id":"o-9"`) {
		t.Errorf("the owner filter is missing:\n%s", body)
	}
	if strings.Contains(body, "doc_team_uuids") {
		t.Error("no teams: no terms clause on teams")
	}
}
