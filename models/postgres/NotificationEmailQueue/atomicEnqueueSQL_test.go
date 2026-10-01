package models

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// AtomicEnqueue's statement reuses $8 in two places with different inferred
// types, and Postgres rejected the whole statement at PREPARE time with
// 42P08 "text versus character varying". Nothing was ever enqueued, and because
// the dispatcher only logged, the symptom was notification email that silently
// never arrived.
//
// This is a source check because the bug is in SQL that only a real Postgres
// will reject, and the unit suite has no database. It asserts the property that
// broke: a parameter used both in the INSERT ... SELECT list and in the WHERE
// must carry an explicit cast, or Postgres has to deduce two types for it.
func TestAtomicEnqueueCastsTheReusedParameter(t *testing.T) {
	src, err := os.ReadFile("notificationEmailQueueModel.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	text := string(src)

	start := strings.Index(text, "INSERT INTO notification_email_queue")
	if start < 0 {
		t.Fatal("AtomicEnqueue statement not found")
	}
	stmt := text[start:]
	if end := strings.Index(stmt, "`"); end > 0 {
		stmt = stmt[:end]
	}

	selectLine := ""
	for _, l := range strings.Split(stmt, "\n") {
		if strings.Contains(l, "SELECT $1") {
			selectLine = l
			break
		}
	}
	if selectLine == "" {
		t.Fatal("SELECT parameter list not found")
	}

	// Every parameter that also appears in the WHERE clause must be cast where
	// it is selected, otherwise the two uses deduce different types.
	where := stmt[strings.Index(stmt, "WHERE NOT EXISTS"):]
	for _, m := range regexp.MustCompile(`\$\d+`).FindAllString(where, -1) {
		if !strings.Contains(selectLine, m+"::") {
			t.Errorf("%s is used in the WHERE clause and in the SELECT list but is not cast in the SELECT list; "+
				"Postgres must deduce one type per parameter and will reject the statement with 42P08", m)
		}
	}
}
