//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	projectController "github.com/akashc777/OneCamp/controllers/Project"
	"github.com/akashc777/OneCamp/helpers"
)

// The New task form's project list carries each project's key (linear-02: a
// new task's address names its project by key, as Linear's linear.new names
// a team), read from Postgres for the lot; the list still answers only the
// projects its reader runs, so a member who doesn't run one gets neither it
// nor its key.
func TestAProjectListCarriesItsProjectsKeys(t *testing.T) {
	w := seedWorkWorld(t)
	ctx := context.Background()
	key, err := taskKeys.Ensure(ctx, uuid.MustParse(w.project), "Website")
	if err != nil || key == "" {
		t.Fatalf("ensure a key: %q %v", key, err)
	}
	listed := func(who string) map[string]string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, w.account(who)))
		projectController.GetDgraphProjectListByAdminDgraphUID(rec, req)
		var out struct {
			Data []struct {
				UUID string `json:"project_uuid"`
				Key  string `json:"project_key"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", who, rec.Code, rec.Body.String())
		}
		keys := map[string]string{}
		for _, p := range out.Data {
			keys[p.UUID] = p.Key
		}
		return keys
	}
	if got, ok := listed("ana")[w.project]; !ok || got != key {
		t.Errorf("ana runs the project: listed %v with key %q, want %q", ok, got, key)
	}
	if got, ok := listed("ben")[w.project]; ok {
		t.Errorf("ben is a member who doesn't run the project, and got it in the list with key %q", got)
	}
}
