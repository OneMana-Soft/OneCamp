//go:build integration

package opensearchInit

// For auditRetention_integration_test.go, which runs on the harness's
// OpenSearch and so sits outside this package: the harness
// (tests/integration) imports this one.
var EnsureAuditLogRetention = ensureAuditLogRetention

const (
	AuditLogPolicy = auditLogPolicy
	AuditLogKeep   = auditLogKeep
)
