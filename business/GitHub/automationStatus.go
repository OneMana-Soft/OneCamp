package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	githubLinkDomain "github.com/akashc777/OneCamp/domain/GitHubLink"
	"github.com/google/uuid"
)

// An automation rule moves a linked task to a status when something happens on
// GitHub ("PR merged" to Done). The status may be one of the project's own,
// so rules are stored the way taskStatus.Resolve reads them back: a built-in
// key or a custom status's id, never a name, which a rename would break. When
// a custom status is renamed or deleted, the rules that named it follow
// (repointAutomationRules).

func init() {
	taskStatusBusiness.OnRepoint(repointAutomationRules)
}

// ruleTarget is the status a rule moves to, or "" for none ("_none" is how
// the settings screen says no change, and older rows may still carry it).
func ruleTarget(rules map[string]string, trigger string) string {
	v := strings.TrimSpace(rules[trigger])
	if v == "_none" {
		return ""
	}
	return v
}

// NormalizeAutomationRules checks every rule's status against the link's
// project and returns the rules as they are stored: rules with no change
// dropped, the rest as built-in keys or custom status ids. A status the
// project does not have is an error wrapping ErrUnknownStatus that lists the
// ones it does.
func NormalizeAutomationRules(ctx context.Context, projectID uuid.UUID, rules map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(rules))
	for trigger := range rules {
		v := ruleTarget(rules, trigger)
		if v == "" {
			continue
		}
		resolved, err := taskStatusBusiness.Resolve(ctx, projectID.String(), v)
		if err != nil {
			return nil, fmt.Errorf("%w: %q. %s", err, v, taskStatusBusiness.Describe(ctx, projectID.String()))
		}
		out[trigger] = resolved.Value()
	}
	return out, nil
}

// repointAutomationRules rewrites the project's rules that named a status
// that was renamed or deleted, and drops the cached copies so the next webhook
// reads them.
func repointAutomationRules(ctx context.Context, r taskStatusBusiness.Repoint) error {
	links, err := GetLinkedRepos(ctx, r.ProjectID)
	if err != nil {
		return err
	}
	for _, link := range links {
		rules := parseAutomationRules(link)
		changed := false
		for trigger, v := range rules {
			if r.Matches(v) {
				rules[trigger] = r.New
				changed = true
			}
		}
		if !changed {
			continue
		}
		b, err := json.Marshal(rules)
		if err != nil {
			return err
		}
		if err := githubLinkDomain.UpdateAutomationRules(ctx, link.Id, string(b)); err != nil {
			return err
		}
		invalidateLinkByLink(link)
	}
	return nil
}

// statusSyncPayload is what the app needs to show a task's new status without
// a reload: the category, and the custom status or none.
func statusSyncPayload(to taskStatusBusiness.Resolved) map[string]interface{} {
	return map[string]interface{}{
		"status":             to.Category,
		"custom_status":      to.CustomID,
		"custom_status_name": to.CustomName,
	}
}
