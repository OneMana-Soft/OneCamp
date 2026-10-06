package opensearchInit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Against a real cluster, when one is named:
//
//	OPENSEARCH_TEST_URL=https://127.0.0.1:9200 OPENSEARCH_TEST_PASSWORD=… go test ./initializers/opensearchInit/
//
// with an index security-auditlog-2026.01.01 made beforehand (and, on a disk
// past its flood stage, cluster.blocks.create_index lifted).
func TestAuditLogRetentionOnARealCluster(t *testing.T) {
	url := os.Getenv("OPENSEARCH_TEST_URL")
	if url == "" {
		t.Skip("OPENSEARCH_TEST_URL not set")
	}
	var err error
	if OpenSearchClient, err = newClient(&OpenSearchConfig{Host: url, Username: "admin", Password: os.Getenv("OPENSEARCH_TEST_PASSWORD")}); err != nil {
		t.Fatal(err)
	}
	// Twice: the second start finds the policy already there.
	ensureAuditLogRetention(context.Background())
	ensureAuditLogRetention(context.Background())

	get := func(path string) map[string]any {
		res, err := OpenSearchClient.Client.Do(context.Background(), rawReq{http.MethodGet, path, ""}, nil)
		if err != nil || res.IsError() {
			t.Fatalf("GET %s: %v %v", path, err, res)
		}
		b, _ := io.ReadAll(res.Body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return out
	}
	policy := get("/_plugins/_ism/policies/" + auditLogPolicy)
	if !strings.Contains(toJSON(policy), `"min_index_age":"`+auditLogKeep+`"`) {
		t.Fatalf("policy not as written: %s", toJSON(policy))
	}
	// A new day's index is claimed by the policy's template as it is made.
	if res, err := OpenSearchClient.Client.Do(context.Background(), rawReq{http.MethodPut, "/security-auditlog-2026.01.02", `{"settings":{"number_of_replicas":0}}`}, nil); err != nil || res.IsError() {
		t.Fatalf("could not make a new audit index: %v %v", err, res)
	}
	// ISM reports what it manages a moment after it starts managing it.
	for _, index := range []string{"security-auditlog-2026.01.01", "security-auditlog-2026.01.02"} {
		managed := false
		for range 30 {
			if strings.Contains(toJSON(get("/_plugins/_ism/explain/"+index)), `"policy_id":"`+auditLogPolicy+`"`) {
				managed = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if !managed {
			t.Fatalf("%s is not under the policy: %s", index, toJSON(get("/_plugins/_ism/explain/"+index)))
		}
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
