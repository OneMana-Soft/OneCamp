package provider

import (
	"encoding/json"
	"fmt"
	"strings"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// Option keys naming what the admin picked to import, per provider.
const (
	OptJiraProjectKey     = "project_key"
	OptTodoistProjectID   = "project_id"
	OptAsanaWorkspaceGID  = "workspace_gid"
	OptBasecampAccountID  = "account_id"
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

// Scope names what a provider imports from in the admin's words: Asana's
// "workspace" seen by a "token", Basecamp's "account" seen by a "sign-in".
type Scope struct {
	Provider, Noun, Credential string
}

// PickScope is the id of the scope to import from those the credential can
// see: the one the admin picked; else the one named like the import's label;
// else the credential's only one. A pick it can't see any more, two named
// like the label, or several and no way to tell are errors that say what to
// do; it never guesses. Pure.
func PickScope(items []DiscoverItem, picked, label string, s Scope) (string, error) {
	if picked != "" {
		for _, it := range items {
			if it.ID == picked {
				return it.ID, nil
			}
		}
		return "", fmt.Errorf("the %s %s you picked isn't visible to this %s any more. Pick another %s, or reconnect with an account that can see it", s.Provider, s.Noun, s.Credential, s.Noun)
	}
	if len(items) == 0 {
		return "", fmt.Errorf("this %s %s can't see any %s. Reconnect with an account that belongs to the %s you want to import", s.Provider, s.Credential, s.Noun, s.Noun)
	}
	want := strings.ToLower(strings.TrimSpace(label))
	var named []DiscoverItem
	for _, it := range items {
		if strings.ToLower(strings.TrimSpace(it.Name)) == want {
			named = append(named, it)
		}
	}
	switch {
	case len(named) == 1:
		return named[0].ID, nil
	case len(named) > 1:
		return "", fmt.Errorf("more than one %s %s is called %q. Pick the one to import", s.Provider, s.Noun, strings.TrimSpace(label))
	case len(items) == 1:
		return items[0].ID, nil
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Name)
	}
	return "", fmt.Errorf("pick which %s %s to import: this %s can see %d (%s)", s.Provider, s.Noun, s.Credential, len(items), strings.Join(names, ", "))
}
