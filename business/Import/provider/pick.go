package provider

import (
	"encoding/json"
	"strings"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// Option keys naming what the admin picked to import, per provider.
const (
	OptJiraProjectKey     = "project_key"
	OptTodoistProjectID   = "project_id"
	OptAsanaWorkspaceGID  = "workspace_gid"
	optLegacyDiscoverPick = "discover_id"
)

// PickedID is the source resource the admin picked for an import (a Jira
// project's key, a Todoist project's id, an Asana workspace's gid), read from
// the job's options under key. "" when they picked nothing.
//
// The web app used to send every such pick as "discover_id", and the
// providers read none of them: the pick was shown and then ignored. A job
// created then still carries its pick there, so that counts too.
func PickedID(opts JobOptions, key string) string {
	for _, k := range []string{key, optLegacyDiscoverPick} {
		if v, ok := opts[k].(string); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return ""
}

// JobPickedID is PickedID for a job's stored options, for the provider code
// that resolves its scope once per job rather than per call.
func JobPickedID(j *importModels.Job, key string) string {
	opts := JobOptions{}
	if j != nil && len(j.Options) > 0 {
		_ = json.Unmarshal(j.Options, &opts)
	}
	return PickedID(opts, key)
}
