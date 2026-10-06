package opensearchInit

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/opensearch-project/opensearch-go/v4"
)

// OpenSearch's security plugin writes an index of its own audit events every
// day (security-auditlog-2026.10.06) and never removes one. Each is a shard
// on a heap sized for a small server, so an install a year old carries 365 of
// them, and the node's shard limit (1000) eventually refuses to create any
// index at all. OneCamp keeps its own audit log in Postgres; these record only
// the cluster's internal traffic. A retention policy (ISM) deletes each one
// after auditLogKeep, and claims new ones as the plugin makes them.
const (
	auditLogPolicy  = "onecamp-audit-log-retention"
	auditLogPattern = "security-auditlog-*"
	auditLogKeep    = "90d"
)

var auditLogPolicyBody = `{"policy":{
	"description":"OneCamp: OpenSearch's own daily audit indices are deleted after ` + auditLogKeep + `",
	"default_state":"keep",
	"states":[
		{"name":"keep","actions":[],"transitions":[{"state_name":"delete","conditions":{"min_index_age":"` + auditLogKeep + `"}}]},
		{"name":"delete","actions":[{"delete":{}}],"transitions":[]}
	],
	"ism_template":[{"index_patterns":["` + auditLogPattern + `"],"priority":100}]
}}`

// rawReq is a request the client has no typed call for.
type rawReq struct {
	method, path, body string
}

func (r rawReq) GetRequest() (*http.Request, error) {
	var body io.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	return opensearch.BuildRequest(r.method, r.path, body, nil, nil)
}

// ensureAuditLogRetention installs the policy once and puts the indices made
// before it under it (a policy's template only claims new ones). Best effort:
// a cluster without the security or ISM plugin has nothing to tidy, and search
// works the same either way.
func ensureAuditLogRetention(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := OpenSearchClient.Client.Do(ctx, rawReq{http.MethodPut, "/_plugins/_ism/policies/" + auditLogPolicy, auditLogPolicyBody}, nil)
	if err != nil {
		helpers.LogInfoWithContext(ctx, "opensearchInit: audit log retention not set: %v", err)
		return
	}
	// 409: already installed by an earlier start.
	if res.IsError() && res.StatusCode != http.StatusConflict {
		helpers.LogInfoWithContext(ctx, "opensearchInit: audit log retention not set: status %d", res.StatusCode)
		return
	}
	// Indices already under the policy are reported and left as they are.
	res, err = OpenSearchClient.Client.Do(ctx, rawReq{http.MethodPost, "/_plugins/_ism/add/" + auditLogPattern, `{"policy_id":"` + auditLogPolicy + `"}`}, nil)
	if err != nil || res.IsError() {
		helpers.LogInfoWithContext(ctx, "opensearchInit: older audit log indices not put under retention: %v", err)
	}
}
