//go:build integration

package opensearchInit_test

// The audit log retention (auditRetention.go) on the harness's OpenSearch,
// the version production runs, with its ISM plugin.
// Run: go test -tags=integration ./initializers/opensearchInit/ -run '^TestAuditLogRetentionOnARealCluster$' -v

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAuditLogRetentionOnARealCluster(t *testing.T) {
	env := integration.SetupOpenSearch(t)
	call := func(method, path, body string) string {
		t.Helper()
		req, err := http.NewRequest(method, env.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, b)
		}
		// Decoded and encoded again, so keys and spacing read the same whatever
		// the server wrote.
		var out map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, path, err, b)
		}
		b, _ = json.Marshal(out)
		return string(b)
	}
	// A day's index the security plugin made before the policy existed.
	call(http.MethodPut, "/security-auditlog-2026.01.01", `{"settings":{"number_of_replicas":0}}`)

	// Twice: the second start finds the policy already there.
	opensearchInit.EnsureAuditLogRetention(context.Background())
	opensearchInit.EnsureAuditLogRetention(context.Background())

	if policy := call(http.MethodGet, "/_plugins/_ism/policies/"+opensearchInit.AuditLogPolicy, ""); !strings.Contains(policy, `"min_index_age":"`+opensearchInit.AuditLogKeep+`"`) {
		t.Fatalf("policy not as written: %s", policy)
	}
	// A new day's index is claimed by the policy's template as it is made.
	call(http.MethodPut, "/security-auditlog-2026.01.02", `{"settings":{"number_of_replicas":0}}`)

	// ISM reports what it manages a moment after it starts managing it.
	for _, index := range []string{"security-auditlog-2026.01.01", "security-auditlog-2026.01.02"} {
		explained := ""
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
			if explained = call(http.MethodGet, "/_plugins/_ism/explain/"+index, ""); strings.Contains(explained, `"policy_id":"`+opensearchInit.AuditLogPolicy+`"`) {
				break
			}
		}
		if !strings.Contains(explained, `"policy_id":"`+opensearchInit.AuditLogPolicy+`"`) {
			t.Fatalf("%s is not under the policy: %s", index, explained)
		}
	}
}
