package business

import (
	"context"
	"testing"
)

// Tables' policies answer today's behaviour (PROGRAM.md §5): nothing is
// embedded outside, no scripts run, AI fields follow AI, and nothing deleted
// or recorded is ever removed. None is listed for admins, or can be set, until
// the package that enforces it adds its line to specs: a switch that does
// nothing is never shown.
func TestTablesPoliciesAreTodaysAndHiddenUntilEnforced(t *testing.T) {
	ctx := context.Background()
	if Policies.Bool(ctx, TablesEmbedOutside) || Policies.Bool(ctx, TablesScripts) || !Policies.Bool(ctx, TablesAIFields) {
		t.Error("embeds outside and scripts are off, and AI fields on, today")
	}
	if Policies.Int(ctx, TablesTrashRetentionDays) != 0 || Policies.Int(ctx, TablesHistoryRetentionDays) != 0 {
		t.Error("tables keep everything until the owner decides how long")
	}
	for _, p := range []string{TablesEmbedOutside.Key, TablesScripts.Key, TablesAIFields.Key, TablesTrashRetentionDays.Key, TablesHistoryRetentionDays.Key} {
		for _, s := range specs {
			if s.key == p {
				t.Errorf("%s is listed for admins before anything enforces it", p)
			}
		}
	}
}
