//go:build integration
// +build integration

package integration_test

import (
	"strings"
	"testing"
)

// sqlCode is a migration's SQL without its comment lines, so a rule about what
// the SQL does never trips on a comment that names what it avoids.
func sqlCode(sql string) string {
	var code []string
	for _, line := range strings.Split(sql, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code = append(code, line)
		}
	}
	return strings.Join(code, "\n")
}

// makesNoIDs fails the test when a migration's SQL makes ids itself. Postgres
// 12 has neither gen_random_uuid nor uuid_generate_* by default, so ids are
// made in Go; comment lines may still name them.
func makesNoIDs(t *testing.T, name, sql string) {
	t.Helper()
	code := sqlCode(sql)
	for _, banned := range []string{"gen_random_uuid", "uuid_generate"} {
		if strings.Contains(code, banned) {
			t.Fatalf("%s uses %s; Postgres 12 has neither by default, so ids are made in Go", name, banned)
		}
	}
}
