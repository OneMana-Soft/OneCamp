package business

import (
	"strings"
	"testing"
)

// These tests lock the per-engine SQL tokens the generic aggregate/plan builders
// compose. They are the safety net for the dialect abstraction: Postgres output
// is pinned so the refactor can't regress it, and MySQL output is pinned so its
// dialect is correct at the SQL-string level (live-connection validation is a
// separate step). No database is required.

func TestPostgresDialectTokens(t *testing.T) {
	d := postgresDialect{}
	checkEq(t, "quote", d.QuoteIdent("amount"), `"amount"`)
	checkEq(t, "quote-inject", d.QuoteIdent(`a"b`), `"a""b"`)
	checkEq(t, "placeholder", d.Placeholder(3), "$3")
	checkEq(t, "text", d.CastText(`"c"`), `("c")::text`)
	checkEq(t, "num", d.CastNumericParam("$1"), "$1::numeric")
	checkEq(t, "date", d.CastDateParam("$1"), "$1::timestamptz")
	checkEq(t, "daytrunc", d.DateTruncDay(`"c"`), `date_trunc('day', "c")::date`)
	checkEq(t, "contains", d.ContainsExpr(`("c")::text`, "$1"), `("c")::text ILIKE '%' || $1 || '%'`)
	checkEq(t, "timeout", d.TimeoutStmt(15000), "SET LOCAL statement_timeout = 15000")
	checkEq(t, "type-num", d.NormalizeType("bigint"), "number")
	checkEq(t, "type-date", d.NormalizeType("timestamp with time zone"), "date")
	checkEq(t, "type-text", d.NormalizeType("character varying"), "text")
	checkEq(t, "type-other", d.NormalizeType("jsonb"), "other")
}

func TestMySQLDialectTokens(t *testing.T) {
	d := mysqlDialect{}
	checkEq(t, "quote", d.QuoteIdent("amount"), "`amount`")
	checkEq(t, "quote-inject", d.QuoteIdent("a`b"), "`a``b`")
	checkEq(t, "placeholder", d.Placeholder(3), "?")
	checkEq(t, "text", d.CastText("`c`"), "CAST(`c` AS CHAR)")
	checkEq(t, "num", d.CastNumericParam("?"), "CAST(? AS DECIMAL(65,10))")
	checkEq(t, "date", d.CastDateParam("?"), "CAST(? AS DATETIME)")
	checkEq(t, "daytrunc", d.DateTruncDay("`c`"), "DATE(`c`)")
	checkEq(t, "contains", d.ContainsExpr("CAST(`c` AS CHAR)", "?"), "LOWER(CAST(`c` AS CHAR)) LIKE CONCAT('%', LOWER(?), '%')")
	checkEq(t, "timeout", d.TimeoutStmt(15000), "SET SESSION max_execution_time = 15000")
	checkEq(t, "type-num", d.NormalizeType("BIGINT"), "number")
	checkEq(t, "type-date", d.NormalizeType("datetime"), "date")
	checkEq(t, "type-text", d.NormalizeType("varchar"), "text")
	checkEq(t, "type-other", d.NormalizeType("json"), "other")
}

// TestBuildWhereClauseMySQL proves the shared predicate builder produces correct
// MySQL SQL (? placeholders, LOWER/LIKE contains, DECIMAL/DATETIME casts) and
// never interpolates a raw value.
func TestBuildWhereClauseMySQL(t *testing.T) {
	d := mysqlDialect{}
	col := "`amount`"
	cases := []struct {
		op       string
		dtype    string
		contains string
	}{
		{"eq", "text", "CAST(`amount` AS CHAR) = ?"},
		{"contains", "text", "LOWER(CAST(`amount` AS CHAR)) LIKE CONCAT('%', LOWER(?), '%')"},
		{"gt", "number", "`amount` > CAST(? AS DECIMAL(65,10))"},
		{"lte", "date", "`amount` <= CAST(? AS DATETIME)"},
		{"empty", "text", "(`amount` IS NULL OR CAST(`amount` AS CHAR) = '')"},
	}
	for _, c := range cases {
		argN := 1
		clause, _ := buildWhereClause(d, col, c.dtype, c.op, "x'; DROP TABLE t;--", &argN)
		checkEq(t, c.op, clause, c.contains)
		if strings.Contains(clause, "DROP TABLE") {
			t.Fatalf("op %s interpolated the value: %q", c.op, clause)
		}
	}
}

func checkEq(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}
