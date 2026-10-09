package provider

import (
	"encoding/json"
	"testing"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

func TestThePickIsReadUnderItsOwnKeyOrTheOldOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts JobOptions
		want string
	}{
		{"its own key", JobOptions{"project_key": " ENG "}, "ENG"},
		{"a job from before each provider had its key", JobOptions{"discover_id": "ENG"}, "ENG"},
		{"its own key wins", JobOptions{"project_key": "ENG", "discover_id": "OPS"}, "ENG"},
		{"an empty pick is no pick", JobOptions{"project_key": "  ", "discover_id": ""}, ""},
		{"nothing picked", JobOptions{}, ""},
		{"not a string", JobOptions{"project_key": 42}, ""},
	} {
		if got := PickedID(tc.opts, OptJiraProjectKey); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	raw, _ := json.Marshal(JobOptions{"workspace_gid": "123"})
	if got := JobPickedID(&importModels.Job{Options: raw}, OptAsanaWorkspaceGID); got != "123" {
		t.Errorf("a job's stored pick: %q", got)
	}
	if got := JobPickedID(&importModels.Job{}, OptAsanaWorkspaceGID); got != "" {
		t.Errorf("a job with no options: %q", got)
	}
}
